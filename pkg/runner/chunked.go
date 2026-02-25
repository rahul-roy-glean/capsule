//go:build linux
// +build linux

package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/ci"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/firecracker"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/fuse"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/uffd"
)

// ChunkedManager extends Manager with chunked snapshot support
type ChunkedManager struct {
	*Manager

	// Chunked snapshot infrastructure
	chunkStore    *snapshot.ChunkStore                         // disk chunks (FUSE rootfs + seed)
	memChunkStore *snapshot.ChunkStore                         // memory chunks (UFFD) — separate LRU to prevent disk prefetch from evicting hot memory pages
	chunkedMetas  map[string]*snapshot.ChunkedSnapshotMetadata // keyed by workloadKey

	// Per-runner UFFD handlers and FUSE disks
	uffdHandlers  map[string]*uffd.Handler
	fuseDisks     map[string]*fuse.ChunkedDisk // rootfs FUSE disks per runner
	fuseSeedDisks map[string]*fuse.ChunkedDisk // repo-cache-seed FUSE disks per runner

	// Network namespace manager (alternative to slot-based TAPs)
	netnsNetwork *network.NetNSNetwork
	useNetNS     bool

	// memBackend overrides metadata-based backend detection:
	// "chunked" forces UFFD, "file" forces file-backed, "" uses metadata.
	memBackend string

	chunkedLogger *logrus.Entry
}

// ChunkedManagerConfig extends HostConfig with chunked snapshot settings
type ChunkedManagerConfig struct {
	HostConfig

	// CIAdapter is the CI system adapter (may be nil for no-op)
	CIAdapter ci.Adapter

	// UseChunkedSnapshots enables chunked snapshot restore
	UseChunkedSnapshots bool

	// UseNetNS uses network namespaces instead of slot-based TAPs
	UseNetNS bool

	// ChunkCacheSizeBytes is the max size of the disk chunk LRU cache (FUSE)
	ChunkCacheSizeBytes int64

	// MemCacheSizeBytes is the max size of the memory chunk LRU cache (UFFD).
	// Separate from disk cache to prevent disk prefetch from evicting hot memory pages.
	MemCacheSizeBytes int64

	// MemBackend controls memory restore: "chunked" (UFFD lazy, default) or
	// "file" (download full snapshot.mem at startup). Overrides what the
	// snapshot metadata says, allowing rollback without rebuilding snapshots.
	MemBackend string

	// GCSPrefix is the top-level prefix for all GCS paths (e.g. "v1").
	GCSPrefix string
}

// NewChunkedManager creates a new manager with chunked snapshot support
func NewChunkedManager(ctx context.Context, cfg ChunkedManagerConfig, logger *logrus.Logger) (*ChunkedManager, error) {
	if logger == nil {
		logger = logrus.New()
	}

	// Create base manager
	baseManager, err := NewManager(ctx, cfg.HostConfig, cfg.CIAdapter, logger)
	if err != nil {
		return nil, err
	}

	cm := &ChunkedManager{
		Manager:       baseManager,
		chunkedMetas:  make(map[string]*snapshot.ChunkedSnapshotMetadata),
		uffdHandlers:  make(map[string]*uffd.Handler),
		fuseDisks:     make(map[string]*fuse.ChunkedDisk),
		fuseSeedDisks: make(map[string]*fuse.ChunkedDisk),
		useNetNS:      cfg.UseNetNS,
		memBackend:    cfg.MemBackend,
		chunkedLogger: logger.WithField("component", "chunked-manager"),
	}

	// Setup chunked snapshot infrastructure if enabled
	if cfg.UseChunkedSnapshots {
		// Disk chunk store (FUSE rootfs + seed) — larger cache for sequential disk reads
		diskCacheSize := cfg.ChunkCacheSizeBytes
		if diskCacheSize <= 0 {
			diskCacheSize = 8 * 1024 * 1024 * 1024 // 8GB default
		}

		chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
			GCSBucket:           cfg.SnapshotBucket,
			GCSPrefix:           cfg.GCSPrefix,
			LocalCachePath:      filepath.Join(cfg.SnapshotCachePath, "chunks"),
			ChunkCacheSizeBytes: diskCacheSize,
			ChunkSubdir:         "disk",
			Logger:              logger,
		})
		if err != nil {
			baseManager.Close()
			return nil, fmt.Errorf("failed to create disk chunk store: %w", err)
		}
		cm.chunkStore = chunkStore
		chunkStore.StartEagerFetcher()

		// Memory chunk store (UFFD) — separate LRU so disk prefetch can't evict
		// hot memory pages. Memory page faults block the guest VM, so cache
		// isolation is critical for latency.
		memCacheSize := cfg.MemCacheSizeBytes
		if memCacheSize <= 0 {
			memCacheSize = 2 * 1024 * 1024 * 1024 // 2GB default
		}

		memChunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
			GCSBucket:           cfg.SnapshotBucket,
			GCSPrefix:           cfg.GCSPrefix,
			LocalCachePath:      filepath.Join(cfg.SnapshotCachePath, "chunks"),
			ChunkCacheSizeBytes: memCacheSize,
			ChunkSubdir:         "mem",
			Logger:              logger,
		})
		if err != nil {
			chunkStore.Close()
			baseManager.Close()
			return nil, fmt.Errorf("failed to create mem chunk store: %w", err)
		}
		cm.memChunkStore = memChunkStore
		memChunkStore.StartEagerFetcher()

		cm.chunkedLogger.WithFields(logrus.Fields{
			"disk_cache_bytes": diskCacheSize,
			"mem_cache_bytes":  memCacheSize,
		}).Info("Created separate disk and memory chunk stores")

		// Wire session stores into base Manager so PauseRunner/ResumeFromSession
		// can upload/fetch chunks via the same GCS bucket as CI snapshots.
		// goldenChunkedMeta is set later by SyncManifest when the first heartbeat arrives.
		if cfg.SessionChunkBucket != "" {
			baseManager.SetSessionStores(memChunkStore, chunkStore, nil)
			baseManager.getDirtyDiskChunks = cm.getDirtyDiskChunksForRunner
			baseManager.setupFUSEDisk = cm.setupFUSEDiskForRunner
			cm.chunkedLogger.Info("GCS-backed session pause/resume enabled (stores wired)")
		}

		// Chunked metadata is loaded on demand via getOrLoadManifest (allocation)
		// and SyncManifest (heartbeat-driven sync). No startup preload needed.
	}

	// Setup network namespace manager if enabled
	if cfg.UseNetNS {
		netnsNet, err := network.NewNetNSNetwork(network.NetNSConfig{
			BridgeName:    cfg.BridgeName,
			Subnet:        cfg.MicroVMSubnet,
			ExternalIface: cfg.ExternalInterface,
			Logger:        logger,
		})
		if err != nil {
			cm.Close()
			return nil, fmt.Errorf("failed to create netns network: %w", err)
		}

		if err := netnsNet.Setup(); err != nil {
			cm.Close()
			return nil, fmt.Errorf("failed to setup netns network: %w", err)
		}

		cm.netnsNetwork = netnsNet
		cm.Manager.SetNetNSNetwork(netnsNet)
		cm.chunkedLogger.Info("Network namespace mode enabled")
	}

	return cm, nil
}

// getOrLoadManifest returns the chunked metadata for a repo, loading it from GCS if needed.
func (cm *ChunkedManager) getOrLoadManifest(ctx context.Context, workloadKey, version string) (*snapshot.ChunkedSnapshotMetadata, error) {
	cm.mu.RLock()
	if meta, ok := cm.chunkedMetas[workloadKey]; ok && (version == "" || meta.Version == version) {
		cm.mu.RUnlock()
		return meta, nil
	}
	cm.mu.RUnlock()

	// If no version specified, resolve via the current-pointer.json for this workload key.
	if version == "" {
		var err error
		version, err = cm.chunkStore.ReadCurrentVersion(ctx, workloadKey)
		if err != nil {
			return nil, fmt.Errorf("failed to read current version for workload key %s: %w", workloadKey, err)
		}
	}

	meta, err := cm.chunkStore.LoadChunkedMetadata(ctx, workloadKey, version)
	if err != nil {
		return nil, fmt.Errorf("failed to load chunked metadata for %s/%s: %w", workloadKey, version, err)
	}

	cm.mu.Lock()
	cm.chunkedMetas[workloadKey] = meta
	cm.mu.Unlock()

	cm.chunkedLogger.WithFields(logrus.Fields{
		"workload_key": workloadKey,
		"version":      meta.Version,
	}).Info("Loaded chunked manifest for workload key")

	return meta, nil
}

// AllocateRunnerChunked allocates a runner using chunked snapshots
func (cm *ChunkedManager) AllocateRunnerChunked(ctx context.Context, req AllocateRequest) (*Runner, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.draining {
		return nil, fmt.Errorf("host is draining")
	}

	if len(cm.runners) >= cm.config.MaxRunners {
		return nil, fmt.Errorf("host at capacity: %d/%d runners", len(cm.runners), cm.config.MaxRunners)
	}

	// Derive workload key — the request must always carry one (resolved upstream).
	workloadKey := req.WorkloadKey

	// Get the appropriate manifest for this workload key
	var meta *snapshot.ChunkedSnapshotMetadata
	if cm.chunkStore != nil {
		cm.mu.Unlock()
		var err error
		meta, err = cm.getOrLoadManifest(ctx, workloadKey, req.SnapshotVersion)
		cm.mu.Lock()
		if err != nil {
			return nil, fmt.Errorf("failed to load manifest for workload key %q: %w", workloadKey, err)
		}
	}

	// Check if we can use chunked restore
	if meta == nil || cm.chunkStore == nil {
		cm.chunkedLogger.Warn("Chunked snapshots not available, falling back to traditional restore")
		cm.mu.Unlock()
		runner, err := cm.Manager.AllocateRunner(ctx, req)
		cm.mu.Lock()
		return runner, err
	}

	runnerID := uuid.New().String()
	cm.chunkedLogger.WithField("runner_id", runnerID).Info("Allocating runner with chunked snapshot")

	startTime := time.Now()

	// Setup network (namespace or slot-based)
	var tap *network.TapDevice
	var netns *network.VMNamespace
	var err error

	if cm.useNetNS && cm.netnsNetwork != nil {
		slot := cm.findAvailableSlot()
		if slot < 0 {
			return nil, fmt.Errorf("no slots available")
		}
		netns, err = cm.netnsNetwork.CreateNamespaceForVM(runnerID, slot)
		if err != nil {
			return nil, fmt.Errorf("failed to create network namespace: %w", err)
		}
		tap = netns.GetTapDevice(cm.netnsNetwork.GetSubnet())
		cm.slotToRunner[slot] = runnerID
		cm.runnerToSlot[runnerID] = slot
	} else {
		// Use slot-based allocation from base manager
		slot := cm.findAvailableSlot()
		if slot < 0 {
			return nil, fmt.Errorf("no TAP slots available")
		}
		tap, err = cm.network.GetOrCreateTapSlot(slot, runnerID)
		if err != nil {
			return nil, fmt.Errorf("failed to get TAP slot: %w", err)
		}
		cm.slotToRunner[slot] = runnerID
		cm.runnerToSlot[runnerID] = slot
	}

	// Setup FUSE disk for lazy rootfs loading with CoW
	fuseMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse")
	if err := os.MkdirAll(fuseMountDir, 0755); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, nil, nil)
		return nil, fmt.Errorf("failed to create FUSE mount dir: %w", err)
	}

	fuseDisk, err := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
		ChunkStore: cm.chunkStore,
		Chunks:     meta.RootfsChunks,
		TotalSize:  meta.TotalDiskSize,
		ChunkSize:  meta.ChunkSize,
		MountPoint: fuseMountDir,
		Logger:     cm.logger.Logger,
	})
	if err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, nil, nil)
		return nil, fmt.Errorf("failed to create FUSE disk: %w", err)
	}

	if err := fuseDisk.Mount(); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, nil, nil)
		return nil, fmt.Errorf("failed to mount FUSE disk: %w", err)
	}
	cm.fuseDisks[runnerID] = fuseDisk

	// Setup FUSE disk for lazy repo-cache-seed loading (if chunks are available).
	// This avoids downloading the full ~20GB repo cache seed image.
	var fuseSeedDisk *fuse.ChunkedDisk
	if len(meta.RepoCacheSeedChunks) > 0 {
		fuseSeedMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse-seed")
		if err := os.MkdirAll(fuseSeedMountDir, 0755); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("failed to create FUSE seed mount dir: %w", err)
		}

		// Compute total repo-cache-seed size from chunks
		var totalSeedSize int64
		for _, c := range meta.RepoCacheSeedChunks {
			end := c.Offset + c.Size
			if end > totalSeedSize {
				totalSeedSize = end
			}
		}

		fuseSeedDisk, err = fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
			ChunkStore: cm.chunkStore,
			Chunks:     meta.RepoCacheSeedChunks,
			TotalSize:  totalSeedSize,
			ChunkSize:  meta.ChunkSize,
			MountPoint: fuseSeedMountDir,
			Logger:     cm.logger.Logger,
		})
		if err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("failed to create FUSE seed disk: %w", err)
		}

		if err := fuseSeedDisk.Mount(); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("failed to mount FUSE seed disk: %w", err)
		}
		cm.fuseSeedDisks[runnerID] = fuseSeedDisk
		cm.chunkedLogger.WithField("runner_id", runnerID).Info("Mounted FUSE-backed repo-cache-seed")
	}

	// Setup memory backend: flag overrides metadata when set, otherwise fall
	// back to metadata-based detection (MemFilePath set → file, else chunked).
	useFileBackedMem := meta.MemFilePath != ""
	if cm.memBackend == "file" {
		useFileBackedMem = true
	} else if cm.memBackend == "chunked" {
		useFileBackedMem = false
	}
	var uffdHandler *uffd.Handler
	var uffdSocketPath string
	var localMemPath string

	if useFileBackedMem {
		// Per-workload-key path so multiple workload keys don't share a single snapshot.mem.
		localMemPath = filepath.Join(cm.config.SnapshotCachePath, workloadKey, "snapshot.mem")
		if _, err := os.Stat(localMemPath); err != nil && meta.MemFilePath != "" {
			// snapshot.mem not cached locally yet — download on demand from GCS.
			cm.chunkedLogger.WithFields(logrus.Fields{
				"runner_id":  runnerID,
				"gcs_path":   meta.MemFilePath,
				"local_path": localMemPath,
			}).Info("Downloading snapshot.mem on demand for repo")
			if dlErr := cm.chunkStore.DownloadRawFile(ctx, meta.MemFilePath, localMemPath); dlErr != nil {
				cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
				return nil, fmt.Errorf("failed to download snapshot.mem from %s: %w", meta.MemFilePath, dlErr)
			}
			fi, _ := os.Stat(localMemPath)
			cm.chunkedLogger.WithFields(logrus.Fields{
				"runner_id":  runnerID,
				"local_path": localMemPath,
				"size_bytes": fi.Size(),
			}).Info("snapshot.mem downloaded on demand")
		} else if err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("raw memory file not found at %s and no MemFilePath in metadata: %w", localMemPath, err)
		}
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"mem_path":  localMemPath,
		}).Info("Using file-backed memory restore")
	} else {
		// Legacy: UFFD lazy memory loading from dedicated memory chunk store.
		uffdSocketPath = filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock")
		uffdHandler, err = uffd.NewHandler(uffd.HandlerConfig{
			SocketPath: uffdSocketPath,
			ChunkStore: cm.memChunkStore,
			Metadata:   meta,
			Logger:     cm.logger.Logger,
		})
		if err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("failed to create UFFD handler: %w", err)
		}

		if err := uffdHandler.Start(); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("failed to start UFFD handler: %w", err)
		}
		cm.uffdHandlers[runnerID] = uffdHandler
	}

	// Pre-warm critical disk chunks before VM resume to prevent guest kernel
	// soft lockups. On restore, jbd2 (ext4 journal) and the filesystem mount
	// immediately read the superblock, block group descriptors, and journal.
	// With FUSE-backed disks these reads block on GCS fetches; if 4 VMs all
	// resume simultaneously the chunk store is overwhelmed and the guest vCPU
	// stalls for >20s triggering a soft lockup watchdog before thaw-agent
	// can register with GitHub Actions.
	//
	// Prefetching the first 16 chunks (64MB @ 4MB/chunk) covers:
	//   - ext4 superblock (offset 1024)
	//   - block group descriptor table
	//   - entire jbd2 journal (usually within first 64MB on a 50GB volume)
	// Repo-cache-seed only needs the superblock (first 2 chunks = 8MB).
	// These fetches run in parallel and populate the chunk store LRU cache
	// so FUSE Read() returns immediately from cache on the actual mount.
	{
		prefetchCtx, prefetchCancel := context.WithTimeout(ctx, 30*time.Second)
		defer prefetchCancel()

		prefetchDone := make(chan error, 2)
		go func() {
			err := fuseDisk.PrefetchHead(prefetchCtx, 16)
			if err != nil {
				cm.chunkedLogger.WithError(err).WithField("runner_id", runnerID).Warn("Rootfs prefetch incomplete (non-fatal)")
			}
			prefetchDone <- err
		}()
		go func() {
			if fuseSeedDisk != nil {
				err := fuseSeedDisk.PrefetchHead(prefetchCtx, 2)
				if err != nil {
					cm.chunkedLogger.WithError(err).WithField("runner_id", runnerID).Warn("Seed disk prefetch incomplete (non-fatal)")
				}
				prefetchDone <- err
			} else {
				prefetchDone <- nil
			}
		}()
		<-prefetchDone
		<-prefetchDone
		cm.chunkedLogger.WithField("runner_id", runnerID).Debug("Pre-resume disk prefetch complete")
	}

	// Eagerly fetch the VM state (CPU/device state) from the ChunkStore.
	// This is small (~100KB) and required as a local file for Firecracker restore.
	snapshotDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to create snapshot dir: %w", err)
	}

	localStatePath := filepath.Join(snapshotDir, "snapshot.state")
	if meta.StateHash != "" {
		stateData, err := cm.chunkStore.GetChunk(ctx, meta.StateHash)
		if err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("failed to fetch vmstate chunk: %w", err)
		}
		if err := os.WriteFile(localStatePath, stateData, 0644); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("failed to write vmstate: %w", err)
		}
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id":  runnerID,
			"state_size": len(stateData),
		}).Debug("Fetched vmstate from chunk store")
	}

	// In chunked mode, rootfs and repo-cache-seed are served via FUSE, memory
	// via UFFD, and state was eagerly fetched above. The only traditional local
	// file we need is the kernel, which was fetched by SyncManifest when the
	// first heartbeat arrived. The kernel is shared across workloads.
	kernelPath := filepath.Join(cm.config.SnapshotCachePath, "kernel.bin")
	if _, err := os.Stat(kernelPath); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("kernel not found at %s (should have been fetched by SyncManifest): %w", kernelPath, err)
	}

	// Create runner record.
	// When using per-VM namespaces, InternalIP is set to the host-reachable
	// veth IP (10.0.{slot}.2) so the host proxy can reach the VM's services.
	internalIP := tap.IP
	if netns != nil {
		internalIP = netns.HostReachableIP
	}
	runner := &Runner{
		ID:              runnerID,
		HostID:          cm.config.HostID,
		State:           StateBooting,
		InternalIP:      internalIP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: meta.Version,
		WorkloadKey:     workloadKey,
		Resources: Resources{
			VCPUs:    cm.config.VCPUsPerRunner,
			MemoryMB: cm.config.MemoryMBPerRunner,
		},
		CreatedAt:   time.Now(),
		SocketPath:  filepath.Join(cm.config.SocketDir, runnerID+".sock"),
		LogPath:     filepath.Join(cm.config.LogDir, runnerID+".log"),
		MetricsPath: filepath.Join(cm.config.LogDir, runnerID+".metrics"),
		// FUSE disk provides the rootfs via lazy loading
		RootfsOverlay: fuseDisk.DiskImagePath(),
	}
	if req.StartCommand != nil {
		runner.ServicePort = req.StartCommand.Port
	}

	// Build kernel boot args
	netCfg := tap.GetNetworkConfig()
	guestIP := tap.IP.String()
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=%s::%s:%s::eth0:off",
		guestIP, netCfg.Gateway, netCfg.Netmask)

	// For repo-cache-seed drive: use FUSE-backed path if available,
	// otherwise fall back to a placeholder (should not happen in chunked mode).
	var repoCacheSeedPath string
	if fuseSeedDisk != nil {
		repoCacheSeedPath = fuseSeedDisk.DiskImagePath()
	} else {
		repoCacheSeedPath = filepath.Join(cm.config.SnapshotCachePath, "repo-cache-seed.img")
	}

	// Build drives to match the snapshot's drive layout.
	// Create per-runner writable repo cache upper image.
	repoCacheUpperPath := filepath.Join(cm.config.WorkspaceDir, runnerID, "repo-cache-upper.img")
	if err := os.MkdirAll(filepath.Dir(repoCacheUpperPath), 0755); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to create repo-cache-upper directory: %w", err)
	}
	if err := createExt4Image(repoCacheUpperPath, cm.config.RepoCacheUpperSizeGB, "BAZEL_REPO_UPPER"); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to create repo-cache-upper image: %w", err)
	}
	drives := cm.buildDrives(repoCacheSeedPath, repoCacheUpperPath)

	// Create VM configuration
	vmCfg := firecracker.VMConfig{
		VMID:           runnerID,
		SocketDir:      cm.config.SocketDir,
		FirecrackerBin: cm.config.FirecrackerBin,
		KernelPath:     kernelPath,
		RootfsPath:     fuseDisk.DiskImagePath(), // FUSE-backed disk
		BootArgs:       bootArgs,
		VCPUs:          runner.Resources.VCPUs,
		MemoryMB:       runner.Resources.MemoryMB,
		NetworkIface: &firecracker.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: tap.Name,
			GuestMAC:    tap.MAC,
		},
		MMDSConfig: &firecracker.MMDSConfig{
			Version:           "V1",
			NetworkInterfaces: []string{"eth0"},
		},
		Drives:      drives,
		LogPath:     runner.LogPath,
		MetricsPath: runner.MetricsPath,
	}

	// When using per-VM namespaces, Firecracker runs inside the namespace
	if netns != nil {
		vmCfg.NetNSPath = netns.GetFirecrackerNetNSPath()
	}

	// Create VM instance
	vm, err := firecracker.NewVM(vmCfg, cm.logger.Logger)
	if err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// Use the eagerly-fetched local vmstate for restore.
	restoreStatePath := localStatePath

	// Setup symlinks from the snapshot's baked-in drive paths (/tmp/snapshot/*.img)
	// to the actual FUSE-backed / local paths. Firecracker opens drives by the paths
	// recorded in the snapshot state file during LoadSnapshot.
	symlinkCleanup, err := cm.setupChunkedSymlinks(
		fuseDisk.DiskImagePath(),
		repoCacheSeedPath,
		repoCacheUpperPath,
	)
	if err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to setup snapshot symlinks: %w", err)
	}

	// Setup TAP rename: the snapshot bakes in host_dev_name="tap-slot-0".
	// With per-VM namespaces, tap-slot-0 already exists in the namespace — no rename needed.
	var tapRestore func()
	if netns == nil {
		var tapErr error
		tapRestore, tapErr = cm.setupSnapshotTAPRename(tap.Name)
		if tapErr != nil {
			symlinkCleanup()
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("failed to setup TAP rename: %w", tapErr)
		}
	}

	// Restore from snapshot.
	// Load WITHOUT resuming so we can inject MMDS data before the guest
	// thaw-agent wakes up. Otherwise the agent reads stale MMDS from the
	// snapshot and skips runner registration.
	var restoreErr error
	if useFileBackedMem {
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"snapshot":  restoreStatePath,
			"mem_path":  localMemPath,
		}).Info("Restoring VM with file-backed memory")
		restoreErr = vm.RestoreFromSnapshot(ctx, restoreStatePath, localMemPath, false)
	} else {
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id":   runnerID,
			"snapshot":    restoreStatePath,
			"uffd_socket": uffdSocketPath,
		}).Info("Restoring VM with UFFD memory backend")
		restoreErr = vm.RestoreFromSnapshotWithUFFD(ctx, restoreStatePath, uffdSocketPath, false)
	}
	if tapRestore != nil {
		tapRestore()
	}
	symlinkCleanup()

	if restoreErr != nil {
		cm.chunkedLogger.WithError(restoreErr).Warn("UFFD restore failed, trying cold boot fallback")
		vm.Stop()

		vm, err = firecracker.NewVM(vmCfg, cm.logger.Logger)
		if err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("failed to recreate VM: %w", err)
		}

		if err := vm.Start(ctx); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("cold boot fallback failed: %w", err)
		}
	}

	// Inject MMDS data BEFORE resuming so the thaw-agent sees fresh config
	mmdsData := cm.buildMMDSData(ctx, runner, tap, req)
	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to set MMDS data: %w", err)
	}

	// Now resume the VM — thaw-agent will read the fresh MMDS data
	if restoreErr == nil {
		if err := vm.Resume(ctx); err != nil {
			vm.Stop()
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("failed to resume VM after MMDS injection: %w", err)
		}
	}

	// When using per-VM namespaces, set up port forwarding (DNAT) so the host
	// can reach services inside the VM via the host-reachable veth IP.
	if netns != nil && cm.netnsNetwork != nil {
		for _, port := range []int{snapshot.ThawAgentHealthPort, snapshot.ThawAgentDebugPort} {
			if err := cm.netnsNetwork.ForwardPort(runnerID, port); err != nil {
				cm.chunkedLogger.WithField("port", port).WithError(err).Warn("Failed to forward port into namespace")
			}
		}
		// Forward user service port if start_command is configured
		if req.StartCommand != nil && req.StartCommand.Port > 0 {
			if err := cm.netnsNetwork.ForwardPort(runnerID, req.StartCommand.Port); err != nil {
				cm.chunkedLogger.WithField("port", req.StartCommand.Port).WithError(err).Warn("Failed to forward service port into namespace")
			}
		}
	}

	runner.State = StateInitializing
	runner.StartedAt = time.Now()

	cm.runners[runnerID] = runner
	cm.vms[runnerID] = vm

	restoreTime := time.Since(startTime)
	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":    runnerID,
		"ip":           runner.InternalIP.String(),
		"restore_time": restoreTime,
		"use_netns":    cm.useNetNS,
	}).Info("Runner allocated with chunked snapshot")

	return runner, nil
}

// ReleaseRunnerChunked releases a runner and optionally saves dirty chunks
func (cm *ChunkedManager) ReleaseRunnerChunked(ctx context.Context, runnerID string, saveIncremental bool) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	_, exists := cm.runners[runnerID]
	if !exists {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":        runnerID,
		"save_incremental": saveIncremental,
	}).Info("Releasing chunked runner")

	// Save incremental snapshot if requested
	if saveIncremental && cm.chunkStore != nil {
		if fuseDisk, ok := cm.fuseDisks[runnerID]; ok {
			dirtyCount := fuseDisk.DirtyChunkCount()
			if dirtyCount > 0 {
				cm.chunkedLogger.WithFields(logrus.Fields{
					"runner_id":    runnerID,
					"dirty_chunks": dirtyCount,
				}).Info("Saving dirty chunks for incremental snapshot")

				dirtyChunks := fuseDisk.GetDirtyChunks()
				uploader := snapshot.NewIncrementalUploader(cm.chunkStore, cm.logger.Logger)

				defaultMeta := cm.chunkedMetas[""]
				newVersion := fmt.Sprintf("%s-%s", defaultMeta.Version, runnerID[:8])
				newMeta, err := uploader.UploadIncrementalSnapshot(ctx, defaultMeta, dirtyChunks, nil, newVersion)
				if err != nil {
					cm.chunkedLogger.WithError(err).Warn("Failed to save incremental snapshot")
				} else {
					cm.chunkedLogger.WithFields(logrus.Fields{
						"new_version":  newMeta.Version,
						"dirty_chunks": dirtyCount,
					}).Info("Incremental snapshot saved")
				}
			}
		}
	}

	// Stop VM
	if vm, exists := cm.vms[runnerID]; exists {
		vm.Stop()
		delete(cm.vms, runnerID)
	}

	// Cleanup UFFD handler
	if handler, exists := cm.uffdHandlers[runnerID]; exists {
		handler.Stop()
		delete(cm.uffdHandlers, runnerID)
	}

	// Cleanup FUSE disks
	if disk, exists := cm.fuseDisks[runnerID]; exists {
		disk.Unmount()
		delete(cm.fuseDisks, runnerID)
	}
	if disk, exists := cm.fuseSeedDisks[runnerID]; exists {
		disk.Unmount()
		delete(cm.fuseSeedDisks, runnerID)
	}

	// Cleanup network
	var tap *network.TapDevice
	if cm.useNetNS && cm.netnsNetwork != nil {
		cm.netnsNetwork.ReleaseNamespace(runnerID)
	} else {
		if slot, ok := cm.runnerToSlot[runnerID]; ok {
			cm.network.ReleaseTapSlot(slot, runnerID)
			delete(cm.slotToRunner, slot)
			delete(cm.runnerToSlot, runnerID)
		}
	}

	// Cleanup workspace
	workspaceDir := filepath.Join(cm.config.WorkspaceDir, runnerID)
	os.RemoveAll(workspaceDir)

	// Cleanup socket
	os.Remove(filepath.Join(cm.config.SocketDir, runnerID+".sock"))
	os.Remove(filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock"))

	delete(cm.runners, runnerID)

	_ = tap // silence unused variable warning
	return nil
}

// cleanupChunkedRunner cleans up resources on allocation failure
func (cm *ChunkedManager) cleanupChunkedRunner(
	runnerID string,
	tap *network.TapDevice,
	netns *network.VMNamespace,
	fuseDisk *fuse.ChunkedDisk,
	uffdHandler *uffd.Handler,
) {
	if uffdHandler != nil {
		uffdHandler.Stop()
	}
	if fuseDisk != nil {
		fuseDisk.Unmount()
	}
	// Also clean up repo-cache-seed FUSE disk if it was mounted
	if seedDisk, ok := cm.fuseSeedDisks[runnerID]; ok {
		seedDisk.Unmount()
		delete(cm.fuseSeedDisks, runnerID)
	}
	if cm.useNetNS && cm.netnsNetwork != nil && netns != nil {
		cm.netnsNetwork.ReleaseNamespace(runnerID)
	} else if tap != nil {
		if slot, ok := cm.runnerToSlot[runnerID]; ok {
			cm.network.ReleaseTapSlot(slot, runnerID)
			delete(cm.slotToRunner, slot)
			delete(cm.runnerToSlot, runnerID)
		}
	}
	workspaceDir := filepath.Join(cm.config.WorkspaceDir, runnerID)
	os.RemoveAll(workspaceDir)
}

// setupChunkedSymlinks creates symlinks from the snapshot's baked-in drive paths
// (/tmp/snapshot/*.img) to the actual FUSE-backed or local paths on this host.
// Firecracker opens drive backing files during LoadSnapshot at the paths recorded
// in the snapshot state file. Returns a cleanup function to remove the symlinks
// after restore (Firecracker holds open fds, so symlinks can be removed).
func (cm *ChunkedManager) setupChunkedSymlinks(rootfsPath, repoCacheSeedPath, repoCacheUpperPath string) (func(), error) {
	if err := os.MkdirAll(snapshotSymlinkDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}

	// Resolve git-cache image path
	gitCacheImg := cm.gitCacheImage
	if gitCacheImg == "" {
		gitCacheImg = cm.getOrCreateGitCachePlaceholder()
	}

	symlinks := []struct {
		name   string
		target string
	}{
		{"rootfs.img", rootfsPath},
		{"repo-cache-seed.img", repoCacheSeedPath},
		{"repo-cache-upper.img", repoCacheUpperPath},
		{"credentials.img", cm.credentialsImage},
		{"git-cache.img", gitCacheImg},
	}

	var created []string
	for _, s := range symlinks {
		if s.target == "" {
			continue
		}
		linkPath := filepath.Join(snapshotSymlinkDir, s.name)
		os.Remove(linkPath)
		if err := os.Symlink(s.target, linkPath); err != nil {
			for _, c := range created {
				os.Remove(c)
			}
			return nil, fmt.Errorf("symlink %s -> %s: %w", linkPath, s.target, err)
		}
		created = append(created, linkPath)
		cm.chunkedLogger.WithFields(logrus.Fields{
			"link":   linkPath,
			"target": s.target,
		}).Debug("Created snapshot symlink")
	}

	return func() {
		for _, c := range created {
			os.Remove(c)
		}
	}, nil
}

// GetChunkedStats returns statistics for chunked snapshot system
func (cm *ChunkedManager) GetChunkedStats() ChunkedStats {
	stats := ChunkedStats{}

	if cm.chunkStore != nil {
		s := cm.chunkStore.CacheStats()
		stats.DiskCacheSize = s.Size
		stats.DiskCacheMaxSize = s.MaxSize
		stats.DiskCacheItems = s.ItemCount
	}
	if cm.memChunkStore != nil {
		s := cm.memChunkStore.CacheStats()
		stats.MemCacheSize = s.Size
		stats.MemCacheMaxSize = s.MaxSize
		stats.MemCacheItems = s.ItemCount
	}

	for _, handler := range cm.uffdHandlers {
		hs := handler.Stats()
		stats.TotalPageFaults += hs.PageFaults
		stats.TotalCacheHits += hs.CacheHits
		stats.TotalChunkFetches += hs.ChunkFetches
	}

	for _, disk := range cm.fuseDisks {
		ds := disk.Stats()
		stats.TotalDiskReads += ds.Reads
		stats.TotalDiskWrites += ds.Writes
		stats.TotalDirtyChunks += ds.DirtyChunks
	}
	for _, disk := range cm.fuseSeedDisks {
		ds := disk.Stats()
		stats.TotalDiskReads += ds.Reads
		stats.TotalDiskWrites += ds.Writes
		stats.TotalDirtyChunks += ds.DirtyChunks
	}

	return stats
}

// ChunkedStats holds statistics for the chunked snapshot system
type ChunkedStats struct {
	// Disk LRU cache stats (FUSE rootfs + seed)
	DiskCacheSize    int64
	DiskCacheMaxSize int64
	DiskCacheItems   int

	// Memory LRU cache stats (UFFD)
	MemCacheSize    int64
	MemCacheMaxSize int64
	MemCacheItems   int

	// UFFD stats (aggregated across all runners)
	TotalPageFaults   uint64
	TotalCacheHits    uint64
	TotalChunkFetches uint64

	// FUSE disk stats (aggregated)
	TotalDiskReads   uint64
	TotalDiskWrites  uint64
	TotalDirtyChunks int
}

// Close shuts down the chunked manager
func (cm *ChunkedManager) Close() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.chunkedLogger.Info("Shutting down chunked manager")

	// Stop all UFFD handlers
	for id, handler := range cm.uffdHandlers {
		handler.Stop()
		delete(cm.uffdHandlers, id)
	}

	// Unmount all FUSE disks
	for id, disk := range cm.fuseDisks {
		disk.Unmount()
		delete(cm.fuseDisks, id)
	}
	for id, disk := range cm.fuseSeedDisks {
		disk.Unmount()
		delete(cm.fuseSeedDisks, id)
	}

	// Cleanup network namespaces
	if cm.netnsNetwork != nil {
		cm.netnsNetwork.Cleanup()
	}

	// Close chunk stores
	if cm.chunkStore != nil {
		cm.chunkStore.Close()
	}
	if cm.memChunkStore != nil {
		cm.memChunkStore.Close()
	}

	// Close base manager
	return cm.Manager.Close()
}

// GetChunkedMetadata returns the loaded chunked snapshot metadata (may be nil).
func (cm *ChunkedManager) GetChunkedMetadata() *snapshot.ChunkedSnapshotMetadata {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.chunkedMetas[""]
}

// GetManifest returns the loaded chunked metadata for a specific workload key (may be nil).
func (cm *ChunkedManager) GetManifest(workloadKey string) (*snapshot.ChunkedSnapshotMetadata, error) {
	cm.mu.RLock()
	meta, ok := cm.chunkedMetas[workloadKey]
	cm.mu.RUnlock()
	if ok {
		return meta, nil
	}
	return nil, fmt.Errorf("manifest not loaded for workload key %q", workloadKey)
}

// GetLoadedManifests returns a map of workload_key -> version for loaded manifests.
func (cm *ChunkedManager) GetLoadedManifests() map[string]string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	result := make(map[string]string)
	for slug, meta := range cm.chunkedMetas {
		if meta != nil {
			result[slug] = meta.Version
		}
	}
	return result
}

// SyncManifest loads (or refreshes) the chunked manifest for a given workload key and version.
// When using file-backed memory, it also downloads snapshot.mem to the per-workload-key path.
func (cm *ChunkedManager) SyncManifest(ctx context.Context, workloadKey, version string) error {
	meta, err := cm.getOrLoadManifest(ctx, workloadKey, version)
	if err != nil {
		return err
	}

	// Eagerly fetch the kernel from the chunk store so it's available as a
	// local file for Firecracker boot config. The kernel is small (~10MB)
	// and shared across workloads, so we always write it to the root cache path.
	if meta.KernelHash != "" && cm.chunkStore != nil {
		kernelPath := filepath.Join(cm.config.SnapshotCachePath, "kernel.bin")
		if _, statErr := os.Stat(kernelPath); statErr != nil {
			cm.chunkedLogger.WithField("kernel_hash", meta.KernelHash).Info("Fetching kernel from chunk store")
			kernelData, err := cm.chunkStore.GetChunk(ctx, meta.KernelHash)
			if err != nil {
				return fmt.Errorf("failed to fetch kernel chunk: %w", err)
			}
			if err := os.WriteFile(kernelPath, kernelData, 0644); err != nil {
				return fmt.Errorf("failed to write kernel to %s: %w", kernelPath, err)
			}
			cm.chunkedLogger.WithFields(logrus.Fields{
				"kernel_size": len(kernelData),
				"path":        kernelPath,
			}).Info("Kernel fetched from chunk store")
		}
	}

	// Download snapshot.mem for file-backed memory mode.
	useFileMem := meta.MemFilePath != ""
	if cm.memBackend == "file" {
		useFileMem = true
	} else if cm.memBackend == "chunked" {
		useFileMem = false
	}

	if useFileMem && meta.MemFilePath != "" && cm.chunkStore != nil {
		memPath := filepath.Join(cm.config.SnapshotCachePath, workloadKey, "snapshot.mem")

		if _, statErr := os.Stat(memPath); statErr != nil {
			// Ensure parent directory exists.
			if err := os.MkdirAll(filepath.Dir(memPath), 0755); err != nil {
				return fmt.Errorf("failed to create directory for snapshot.mem: %w", err)
			}
			cm.chunkedLogger.WithFields(logrus.Fields{
				"workload_key": workloadKey,
				"gcs_path":     meta.MemFilePath,
				"local_path":   memPath,
			}).Info("Downloading snapshot.mem for workload key")
			if err := cm.chunkStore.DownloadRawFile(ctx, meta.MemFilePath, memPath); err != nil {
				return fmt.Errorf("failed to download snapshot.mem for %s: %w", workloadKey, err)
			}
		}
	}

	// Update the golden metadata on the base Manager so PauseRunner can use it
	// as the base for session diff merging.
	if cm.sessionMemStore != nil {
		cm.SetGoldenChunkedMeta(meta)
	}

	return nil
}
func (cm *ChunkedManager) GetChunkStore() *snapshot.ChunkStore {
	return cm.chunkStore
}

// GetSubnet returns the subnet from either netns or nat network
func (cm *ChunkedManager) GetSubnet() *net.IPNet {
	if cm.useNetNS && cm.netnsNetwork != nil {
		return cm.netnsNetwork.GetSubnet()
	}
	return cm.network.GetSubnet()
}

// getDirtyDiskChunksForRunner returns the dirty FUSE disk chunks for a runner,
// or nil if the runner has no FUSE disk. Used as a callback by Manager.PauseRunner
// to include disk changes in the session upload.
func (cm *ChunkedManager) getDirtyDiskChunksForRunner(runnerID string) map[int][]byte {
	cm.mu.RLock()
	disk, ok := cm.fuseDisks[runnerID]
	cm.mu.RUnlock()
	if !ok || disk == nil {
		return nil
	}
	return disk.GetDirtyChunks()
}

// setupFUSEDiskForRunner creates and mounts a FUSE-backed disk from chunk refs.
// Used by Manager.ResumeFromSession for GCS-backed cross-host resume.
func (cm *ChunkedManager) setupFUSEDiskForRunner(runnerID string, chunks []snapshot.ChunkRef, totalSize, chunkSize int64) (string, error) {
	fuseMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse")
	if err := os.MkdirAll(fuseMountDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create FUSE mount dir: %w", err)
	}

	fuseDisk, err := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
		ChunkStore: cm.chunkStore,
		Chunks:     chunks,
		TotalSize:  totalSize,
		ChunkSize:  chunkSize,
		MountPoint: fuseMountDir,
		Logger:     cm.logger.Logger,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create FUSE disk: %w", err)
	}

	if err := fuseDisk.Mount(); err != nil {
		return "", fmt.Errorf("failed to mount FUSE disk: %w", err)
	}

	cm.mu.Lock()
	cm.fuseDisks[runnerID] = fuseDisk
	cm.mu.Unlock()

	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"chunks":     len(chunks),
		"total_size": totalSize,
	}).Info("FUSE disk mounted for session resume")

	return fuseDisk.DiskImagePath(), nil
}
