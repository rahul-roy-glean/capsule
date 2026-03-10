//go:build linux
// +build linux

package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/firecracker"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/fuse"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/uffd"
)

type chunkedVM interface {
	RestoreFromSnapshot(ctx context.Context, snapshotPath, memPath string, resume bool) error
	RestoreFromSnapshotWithUFFD(ctx context.Context, snapshotPath, uffdSocketPath string, resume bool) error
	Start(ctx context.Context) error
	Resume(ctx context.Context) error
	Stop() error
	SetMMDSData(ctx context.Context, data interface{}) error
}

// ChunkedManager extends Manager with chunked snapshot support
type ChunkedManager struct {
	*Manager

	// Chunked snapshot infrastructure
	chunkStore    *snapshot.ChunkStore                         // disk chunks (FUSE rootfs + seed)
	memChunkStore *snapshot.ChunkStore                         // memory chunks (UFFD) — separate LRU to prevent disk prefetch from evicting hot memory pages
	chunkedMetas  map[string]*snapshot.ChunkedSnapshotMetadata // keyed by workloadKey

	// Per-runner UFFD handlers and FUSE disks
	uffdHandlers       map[string]*uffd.Handler
	fuseDisks          map[string]*fuse.ChunkedDisk            // rootfs FUSE disks per runner
	fuseExtensionDisks map[string]map[string]*fuse.ChunkedDisk // extension drive FUSE disks: runnerID → driveID → disk

	// Network namespace manager
	netnsNetwork *network.NetNSNetwork

	// memBackend overrides metadata-based backend detection:
	// "chunked" forces UFFD, "file" forces file-backed, "" uses metadata.
	memBackend string

	// readyTimeout is the max wait time for thaw-agent health check
	readyTimeout time.Duration

	// snapshotRestoreMu serializes access to the shared /tmp/snapshot symlink
	// namespace during Firecracker restore.
	snapshotRestoreMu sync.Mutex
	// cachePopulateMu serializes population of shared local cache files such as
	// kernel.bin and per-workload snapshot.mem files.
	cachePopulateMu sync.Mutex

	newVMFn                func(cfg firecracker.VMConfig, logger *logrus.Logger) (chunkedVM, error)
	setupChunkedSymlinksFn func(rootfsPath string, extensionDrivePaths map[string]string) (func(), error)
	waitForReadyFn         func(ctx context.Context, ip string, timeout time.Duration) error
	forwardPortFn          func(runnerID string, port int) error

	chunkedLogger *logrus.Entry
}

// ChunkedManagerConfig extends HostConfig with chunked snapshot settings
type ChunkedManagerConfig struct {
	HostConfig

	// UseChunkedSnapshots enables chunked snapshot restore
	UseChunkedSnapshots bool

	// Network namespace configuration
	MicroVMSubnet     string
	ExternalInterface string
	BridgeName        string

	// ChunkCacheSizeBytes is the max size of the disk chunk LRU cache (FUSE)
	ChunkCacheSizeBytes int64

	// MemCacheSizeBytes is the max size of the memory chunk LRU cache (UFFD).
	// Separate from disk cache to prevent disk prefetch from evicting hot memory pages.
	MemCacheSizeBytes int64

	// MemBackend controls memory restore: "chunked" (UFFD lazy, default) or
	// "file" (download full snapshot.mem at startup). Overrides what the
	// snapshot metadata says, allowing rollback without rebuilding snapshots.
	MemBackend string

	// ReadyTimeout is the maximum time to wait for the thaw-agent health
	// endpoint to return HTTP 200 after VM restore. If the agent doesn't
	// become healthy within this window the VM is killed and the allocation
	// fails (default 10s).
	ReadyTimeout time.Duration

	// GCSPrefix is the top-level prefix for all GCS paths (e.g. "v1").
	GCSPrefix string
}

// NewChunkedManager creates a new manager with chunked snapshot support
func NewChunkedManager(ctx context.Context, cfg ChunkedManagerConfig, logger *logrus.Logger) (*ChunkedManager, error) {
	if logger == nil {
		logger = logrus.New()
	}

	// Create base manager
	baseManager, err := NewManager(ctx, cfg.HostConfig, logger)
	if err != nil {
		return nil, err
	}

	cm := &ChunkedManager{
		Manager:            baseManager,
		chunkedMetas:       make(map[string]*snapshot.ChunkedSnapshotMetadata),
		uffdHandlers:       make(map[string]*uffd.Handler),
		fuseDisks:          make(map[string]*fuse.ChunkedDisk),
		fuseExtensionDisks: make(map[string]map[string]*fuse.ChunkedDisk),
		memBackend:         cfg.MemBackend,
		readyTimeout:       cfg.ReadyTimeout,
		chunkedLogger:      logger.WithField("component", "chunked-manager"),
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
			baseManager.getDirtyExtensionDiskChunks = cm.getAllDirtyExtensionDiskChunks
			baseManager.setupExtensionFUSEDisk = cm.setupExtensionFUSEDiskForRunner
			baseManager.getDirtyRootfsDiskChunks = cm.getDirtyRootfsDiskChunksCallback
			baseManager.setupRootfsFUSEDisk = cm.setupRootfsFUSEDiskForRunner
			baseManager.cleanupFUSEDisks = cm.cleanupFUSEDisksForRunner
			cm.chunkedLogger.Info("GCS-backed session pause/resume enabled (stores wired)")
		}

		// Chunked metadata is loaded on demand via getOrLoadManifest (allocation)
		// and SyncManifest (heartbeat-driven sync). No startup preload needed.
	}

	// Setup network namespace manager
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

	// Also update the golden metadata on the base Manager so PauseRunner
	// has the correct base for session diff merging. This ensures it's set
	// even if SyncManifest hasn't been called yet (e.g. first allocate before
	// the heartbeat loop fires).
	if cm.sessionMemStore != nil {
		cm.SetGoldenChunkedMeta(meta)
	}

	cm.chunkedLogger.WithFields(logrus.Fields{
		"workload_key": workloadKey,
		"version":      meta.Version,
	}).Info("Loaded chunked manifest for workload key")

	return meta, nil
}

func (cm *ChunkedManager) reserveRunnerSlot(runnerID string) (int, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	slot := cm.findAvailableSlot()
	if slot < 0 {
		return -1, fmt.Errorf("no slots available")
	}
	cm.slotToRunner[slot] = runnerID
	cm.runnerToSlot[runnerID] = slot
	return slot, nil
}

func (cm *ChunkedManager) releaseRunnerSlot(runnerID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if slot, ok := cm.runnerToSlot[runnerID]; ok {
		delete(cm.slotToRunner, slot)
		delete(cm.runnerToSlot, runnerID)
	}
}

func (cm *ChunkedManager) ensureLocalMemFile(ctx context.Context, runnerID, workloadKey string, meta *snapshot.ChunkedSnapshotMetadata) (string, error) {
	localMemPath := filepath.Join(cm.config.SnapshotCachePath, workloadKey, "snapshot.mem")
	if _, err := os.Stat(localMemPath); err == nil {
		return localMemPath, nil
	} else if meta.MemFilePath == "" {
		return "", fmt.Errorf("raw memory file not found at %s and no MemFilePath in metadata: %w", localMemPath, err)
	}

	cm.cachePopulateMu.Lock()
	defer cm.cachePopulateMu.Unlock()

	if _, err := os.Stat(localMemPath); err == nil {
		return localMemPath, nil
	}

	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"gcs_path":   meta.MemFilePath,
		"local_path": localMemPath,
	}).Info("Downloading snapshot.mem on demand for repo")
	if dlErr := cm.chunkStore.DownloadRawFile(ctx, meta.MemFilePath, localMemPath); dlErr != nil {
		return "", fmt.Errorf("failed to download snapshot.mem from %s: %w", meta.MemFilePath, dlErr)
	}
	fi, _ := os.Stat(localMemPath)
	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"local_path": localMemPath,
		"size_bytes": fi.Size(),
	}).Info("snapshot.mem downloaded on demand")
	return localMemPath, nil
}

func (cm *ChunkedManager) ensureKernelPath(ctx context.Context, meta *snapshot.ChunkedSnapshotMetadata) (string, error) {
	kernelPath := filepath.Join(cm.config.SnapshotCachePath, "kernel.bin")
	if _, err := os.Stat(kernelPath); err == nil {
		return kernelPath, nil
	} else if meta.KernelHash == "" || cm.chunkStore == nil {
		return "", fmt.Errorf("kernel not found at %s and no KernelHash in metadata to fetch it: %w", kernelPath, err)
	}

	cm.cachePopulateMu.Lock()
	defer cm.cachePopulateMu.Unlock()

	if _, err := os.Stat(kernelPath); err == nil {
		return kernelPath, nil
	}

	cm.chunkedLogger.WithField("kernel_hash", meta.KernelHash).Info("Fetching kernel on demand during allocation")
	kernelData, fetchErr := cm.chunkStore.GetChunk(ctx, meta.KernelHash)
	if fetchErr != nil {
		return "", fmt.Errorf("failed to fetch kernel chunk on demand: %w", fetchErr)
	}
	if writeErr := os.WriteFile(kernelPath, kernelData, 0644); writeErr != nil {
		return "", fmt.Errorf("failed to write kernel to %s: %w", kernelPath, writeErr)
	}
	cm.chunkedLogger.WithFields(logrus.Fields{
		"kernel_size": len(kernelData),
		"path":        kernelPath,
	}).Info("Kernel fetched on demand during allocation")
	return kernelPath, nil
}

func (cm *ChunkedManager) registerAllocatedRunner(
	runnerID string,
	runner *Runner,
	vm *firecracker.VM,
	fuseDisk *fuse.ChunkedDisk,
	extensionDisks map[string]*fuse.ChunkedDisk,
	uffdHandler *uffd.Handler,
	proxy *authproxy.AuthProxy,
) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.draining {
		return fmt.Errorf("host is draining")
	}

	cm.fuseDisks[runnerID] = fuseDisk
	if len(extensionDisks) > 0 {
		cm.fuseExtensionDisks[runnerID] = extensionDisks
	}
	if uffdHandler != nil {
		cm.uffdHandlers[runnerID] = uffdHandler
	}
	if proxy != nil {
		cm.authProxies[runnerID] = proxy
	}
	cm.runners[runnerID] = runner
	cm.vms[runnerID] = vm
	return nil
}

func (cm *ChunkedManager) newVM(cfg firecracker.VMConfig) (chunkedVM, error) {
	if cm.newVMFn != nil {
		return cm.newVMFn(cfg, cm.logger.Logger)
	}
	return firecracker.NewVM(cfg, cm.logger.Logger)
}

func (cm *ChunkedManager) setupRestoreSymlinks(rootfsPath string, extensionDrivePaths map[string]string) (func(), error) {
	if cm.setupChunkedSymlinksFn != nil {
		return cm.setupChunkedSymlinksFn(rootfsPath, extensionDrivePaths)
	}
	return cm.setupChunkedSymlinks(rootfsPath, extensionDrivePaths)
}

func (cm *ChunkedManager) waitForRunnerReady(ctx context.Context, ip string, timeout time.Duration) error {
	if cm.waitForReadyFn != nil {
		return cm.waitForReadyFn(ctx, ip, timeout)
	}
	return cm.waitForThawAgent(ctx, ip, timeout)
}

func (cm *ChunkedManager) forwardRunnerPort(runnerID string, port int) error {
	if cm.forwardPortFn != nil {
		return cm.forwardPortFn(runnerID, port)
	}
	return cm.netnsNetwork.ForwardPort(runnerID, port)
}

func (cm *ChunkedManager) restoreAndActivateRunner(
	ctx context.Context,
	runnerID string,
	req AllocateRequest,
	runner *Runner,
	netns *network.VMNamespace,
	tap *network.TapDevice,
	vmCfg firecracker.VMConfig,
	restoreStatePath string,
	localMemPath string,
	uffdSocketPath string,
	useFileBackedMem bool,
	extensionDrivePaths map[string]string,
) (chunkedVM, *authproxy.AuthProxy, error) {
	vm, err := cm.newVM(vmCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create VM: %w", err)
	}

	var restoreErr error
	cm.snapshotRestoreMu.Lock()
	func() {
		defer cm.snapshotRestoreMu.Unlock()

		symlinkCleanup, setupErr := cm.setupRestoreSymlinks(
			vmCfg.RootfsPath,
			extensionDrivePaths,
		)
		if setupErr != nil {
			err = fmt.Errorf("failed to setup snapshot symlinks: %w", setupErr)
			return
		}
		defer symlinkCleanup()

		if useFileBackedMem {
			cm.chunkedLogger.WithFields(logrus.Fields{
				"runner_id": runnerID,
				"snapshot":  restoreStatePath,
				"mem_path":  localMemPath,
			}).Info("Restoring VM with file-backed memory")
			restoreErr = vm.RestoreFromSnapshot(ctx, restoreStatePath, localMemPath, false)
			return
		}

		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id":   runnerID,
			"snapshot":    restoreStatePath,
			"uffd_socket": uffdSocketPath,
		}).Info("Restoring VM with UFFD memory backend")
		restoreErr = vm.RestoreFromSnapshotWithUFFD(ctx, restoreStatePath, uffdSocketPath, false)
	}()
	if err != nil {
		return nil, nil, err
	}

	if restoreErr != nil {
		cm.chunkedLogger.WithError(restoreErr).Warn("UFFD restore failed, trying cold boot fallback")
		vm.Stop()

		vm, err = cm.newVM(vmCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to recreate VM: %w", err)
		}

		if err := vm.Start(ctx); err != nil {
			return nil, nil, fmt.Errorf("cold boot fallback failed: %w", err)
		}
	}

	var proxy *authproxy.AuthProxy
	if req.AuthConfig != nil && netns != nil {
		hostVethIP := net.IPv4(10, 200, byte(netns.Slot), 1).String()
		proxy, err = authproxy.NewAuthProxy(
			runnerID,
			*req.AuthConfig,
			netns.Path,
			netns.Gateway.String(),
			hostVethIP,
			cm.chunkedLogger,
		)
		if err != nil {
			vm.Stop()
			return nil, nil, fmt.Errorf("failed to create auth proxy: %w", err)
		}
		if err := proxy.Start(context.Background()); err != nil {
			vm.Stop()
			return nil, nil, fmt.Errorf("failed to start auth proxy: %w", err)
		}
	}

	mmdsData := cm.buildMMDSData(ctx, runner, tap, req)
	if proxy != nil {
		mmdsData.Latest.Proxy.Address = proxy.ProxyAddress()
		mmdsData.Latest.Proxy.CACertPEM = string(proxy.CACertPEM)
		mmdsData.Latest.Proxy.MetadataHost = proxy.GatewayIP()
	}
	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		return nil, proxy, fmt.Errorf("failed to set MMDS data: %w", err)
	}

	if restoreErr == nil {
		if err := vm.Resume(ctx); err != nil {
			vm.Stop()
			return nil, proxy, fmt.Errorf("failed to resume VM after MMDS injection: %w", err)
		}
	}

	for _, port := range []int{snapshot.ThawAgentHealthPort, snapshot.ThawAgentDebugPort} {
		if err := cm.forwardRunnerPort(runnerID, port); err != nil {
			cm.chunkedLogger.WithField("port", port).WithError(err).Warn("Failed to forward port into namespace")
		}
	}
	if req.StartCommand != nil && req.StartCommand.Port > 0 {
		if err := cm.forwardRunnerPort(runnerID, req.StartCommand.Port); err != nil {
			cm.chunkedLogger.WithField("port", req.StartCommand.Port).WithError(err).Warn("Failed to forward service port into namespace")
		}
	}

	readyTimeout := cm.readyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 10 * time.Second
	}
	if err := cm.waitForRunnerReady(ctx, runner.InternalIP.String(), readyTimeout); err != nil {
		cm.chunkedLogger.WithError(err).WithField("runner_id", runnerID).Error("Thaw-agent failed ready check, killing VM")
		vm.Stop()
		return nil, proxy, fmt.Errorf("thaw-agent not ready after %v: %w", readyTimeout, err)
	}

	return vm, proxy, nil
}

// AllocateRunnerChunked allocates a runner using chunked snapshots
func (cm *ChunkedManager) AllocateRunnerChunked(ctx context.Context, req AllocateRequest) (_ *Runner, retErr error) {
	var idempotentAlloc *recentAllocation
	var allocatedRunner *Runner

	if existing, alloc, leader := cm.beginIdempotentAllocation(req.RequestID); existing != nil {
		return existing, nil
	} else if !leader {
		return cm.waitForIdempotentAllocation(ctx, req.RequestID, alloc)
	} else {
		idempotentAlloc = alloc
		defer func() {
			cm.finishIdempotentAllocation(req.RequestID, idempotentAlloc, allocatedRunner, retErr)
		}()
	}

	if err := cm.reserveAllocationSlot(); err != nil {
		retErr = err
		return nil, retErr
	}
	defer cm.releaseAllocationSlot()

	// Derive workload key — the request must always carry one (resolved upstream).
	workloadKey := req.WorkloadKey

	// Get the appropriate manifest for this workload key
	var meta *snapshot.ChunkedSnapshotMetadata
	if cm.chunkStore != nil {
		var err error
		meta, err = cm.getOrLoadManifest(ctx, workloadKey, req.SnapshotVersion)
		if err != nil {
			retErr = fmt.Errorf("failed to load manifest for workload key %q: %w", workloadKey, err)
			return nil, retErr
		}
	}

	// Check if we can use chunked restore
	if meta == nil || cm.chunkStore == nil {
		retErr = fmt.Errorf("chunked snapshots not available: meta=%v, chunkStore=%v", meta != nil, cm.chunkStore != nil)
		return nil, retErr
	}

	runnerID := uuid.New().String()
	cm.chunkedLogger.WithField("runner_id", runnerID).Info("Allocating runner with chunked snapshot")

	startTime := time.Now()

	// Setup network namespace
	var tap *network.TapDevice
	var netns *network.VMNamespace
	var err error
	var fuseDisk *fuse.ChunkedDisk
	extensionFUSEDisks := make(map[string]*fuse.ChunkedDisk)
	var uffdHandler *uffd.Handler
	var localMemPath string
	var proxy *authproxy.AuthProxy

	cleanup := func() {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, extensionFUSEDisks, uffdHandler, proxy)
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			cleanup()
		}
	}()

	slot, err := cm.reserveRunnerSlot(runnerID)
	if err != nil {
		retErr = err
		return nil, retErr
	}
	netns, err = cm.netnsNetwork.CreateNamespaceForVM(runnerID, slot)
	if err != nil {
		return nil, fmt.Errorf("failed to create network namespace: %w", err)
	}
	tap = netns.GetTapDevice(cm.netnsNetwork.GetSubnet())

	// Setup FUSE disk for lazy rootfs loading with CoW
	fuseMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse")
	if err := os.MkdirAll(fuseMountDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create FUSE mount dir: %w", err)
	}

	fuseDisk, err = fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
		ChunkStore: cm.chunkStore,
		Chunks:     meta.RootfsChunks,
		TotalSize:  meta.TotalDiskSize,
		ChunkSize:  meta.ChunkSize,
		MountPoint: fuseMountDir,
		Logger:     cm.logger.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create FUSE disk: %w", err)
	}

	if err := fuseDisk.Mount(); err != nil {
		return nil, fmt.Errorf("failed to mount FUSE disk: %w", err)
	}

	// Setup FUSE/writable disks for extension drives (from meta.ExtensionDrives).
	// Read-only drives get a FUSE-backed lazy disk; writable drives get a fresh ext4 image.
	extensionDrivePaths := make(map[string]string)
	for driveID, extDrive := range meta.ExtensionDrives {
		if len(extDrive.Chunks) > 0 {
			// FUSE-mount drives that have chunked content to preserve filesystem
			// state from the snapshot. This applies to both read-only and writable
			// drives — the kernel's cached inodes/dentries must match the on-disk
			// content for correct snapshot restore.
			fuseExtMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse-ext-"+driveID)
			if err := os.MkdirAll(fuseExtMountDir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create FUSE ext dir for %s: %w", driveID, err)
			}
			var totalExtSize int64
			for _, c := range extDrive.Chunks {
				if end := c.Offset + c.Size; end > totalExtSize {
					totalExtSize = end
				}
			}
			extFUSE, fuseErr := fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
				ChunkStore: cm.chunkStore,
				Chunks:     extDrive.Chunks,
				TotalSize:  totalExtSize,
				ChunkSize:  meta.ChunkSize,
				MountPoint: fuseExtMountDir,
				Logger:     cm.logger.Logger,
			})
			if fuseErr != nil {
				return nil, fmt.Errorf("failed to create FUSE ext disk %s: %w", driveID, fuseErr)
			}
			if err := extFUSE.Mount(); err != nil {
				return nil, fmt.Errorf("failed to mount FUSE ext disk %s: %w", driveID, err)
			}
			extensionFUSEDisks[driveID] = extFUSE
			extensionDrivePaths[driveID] = extFUSE.DiskImagePath()
			cm.chunkedLogger.WithFields(logrus.Fields{"runner_id": runnerID, "drive_id": driveID}).Info("Mounted FUSE-backed extension drive")
		} else {
			// No chunks: create fresh ext4 image (e.g. overlay drives)
			imgPath := filepath.Join(cm.config.WorkspaceDir, runnerID, driveID+"-upper.img")
			if mkErr := os.MkdirAll(filepath.Dir(imgPath), 0755); mkErr != nil {
				return nil, fmt.Errorf("failed to create dir for ext drive %s: %w", driveID, mkErr)
			}
			sizeGB := int(extDrive.SizeBytes / (1024 * 1024 * 1024))
			if sizeGB <= 0 {
				sizeGB = 10
			}
			if mkErr := createExt4Image(imgPath, sizeGB, "EXT_"+driveID); mkErr != nil {
				return nil, fmt.Errorf("failed to create ext drive image %s: %w", driveID, mkErr)
			}
			extensionDrivePaths[driveID] = imgPath
			cm.chunkedLogger.WithFields(logrus.Fields{"runner_id": runnerID, "drive_id": driveID}).Info("Created writable extension drive image")
		}
	}

	// Setup memory backend: flag overrides metadata when set, otherwise fall
	// back to metadata-based detection (MemFilePath set → file, else chunked).
	useFileBackedMem := meta.MemFilePath != ""
	if cm.memBackend == "file" {
		useFileBackedMem = true
	} else if cm.memBackend == "chunked" {
		useFileBackedMem = false
	}
	var uffdSocketPath string

	if useFileBackedMem {
		localMemPath, err = cm.ensureLocalMemFile(ctx, runnerID, workloadKey, meta)
		if err != nil {
			return nil, err
		}
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"mem_path":  localMemPath,
		}).Info("Using file-backed memory restore")
	} else {
		// Legacy: UFFD lazy memory loading from dedicated memory chunk store.
		uffdSocketPath = filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock")
		uffdHandler, err = uffd.NewHandler(uffd.HandlerConfig{
			SocketPath:             uffdSocketPath,
			ChunkStore:             cm.memChunkStore,
			Metadata:               meta,
			Logger:                 cm.logger.Logger,
			FaultConcurrency:       32,
			EnablePrefetchTracking: true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create UFFD handler: %w", err)
		}

		if err := uffdHandler.Start(); err != nil {
			return nil, fmt.Errorf("failed to start UFFD handler: %w", err)
		}
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

		// Collect all read-only extension FUSE disks to prefetch.
		nPrefetch := 1 + len(extensionFUSEDisks)
		prefetchDone := make(chan error, nPrefetch)
		go func() {
			err := fuseDisk.PrefetchHead(prefetchCtx, 16)
			if err != nil {
				cm.chunkedLogger.WithError(err).WithField("runner_id", runnerID).Warn("Rootfs prefetch incomplete (non-fatal)")
			}
			prefetchDone <- err
		}()
		for did, ed := range extensionFUSEDisks {
			did, ed := did, ed
			go func() {
				err := ed.PrefetchHead(prefetchCtx, 2)
				if err != nil {
					cm.chunkedLogger.WithError(err).WithFields(logrus.Fields{"runner_id": runnerID, "drive_id": did}).Warn("Extension disk prefetch incomplete (non-fatal)")
				}
				prefetchDone <- err
			}()
		}
		for i := 0; i < nPrefetch; i++ {
			<-prefetchDone
		}
		cm.chunkedLogger.WithField("runner_id", runnerID).Debug("Pre-resume disk prefetch complete")
	}

	// Eagerly fetch the VM state (CPU/device state) from the ChunkStore.
	// This is small (~100KB) and required as a local file for Firecracker restore.
	snapshotDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot dir: %w", err)
	}

	localStatePath := filepath.Join(snapshotDir, "snapshot.state")
	if meta.StateHash != "" {
		stateData, err := cm.chunkStore.GetChunk(ctx, meta.StateHash)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch vmstate chunk: %w", err)
		}
		if err := os.WriteFile(localStatePath, stateData, 0644); err != nil {
			return nil, fmt.Errorf("failed to write vmstate: %w", err)
		}
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id":  runnerID,
			"state_size": len(stateData),
		}).Debug("Fetched vmstate from chunk store")
	}

	// In chunked mode, rootfs and extension drives are served via FUSE, memory
	// via UFFD, and state was eagerly fetched above. The only traditional local
	// file we need is the kernel. It is normally fetched by SyncManifest on the
	// first heartbeat, but if allocation races ahead we fetch it on demand here.
	kernelPath, err := cm.ensureKernelPath(ctx, meta)
	if err != nil {
		return nil, err
	}

	// When using per-VM namespaces, InternalIP is set to the host-reachable
	// veth IP (10.0.{slot}.2) so the host proxy can reach the VM's services.
	internalIP := netns.HostReachableIP
	runner := &Runner{
		ID:              runnerID,
		HostID:          cm.config.HostID,
		State:           StateBooting,
		InternalIP:      internalIP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: meta.Version,
		WorkloadKey:     workloadKey,
		Resources:       req.Resources,
		CreatedAt:       time.Now(),
		SocketPath:      filepath.Join(cm.config.SocketDir, runnerID+".sock"),
		LogPath:         filepath.Join(cm.config.LogDir, runnerID+".log"),
		MetricsPath:     filepath.Join(cm.config.LogDir, runnerID+".metrics"),
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

	drives := cm.buildDrives(extensionDrivePaths)

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

	// Firecracker runs inside the network namespace
	vmCfg.NetNSPath = netns.GetFirecrackerNetNSPath()

	// Use the eagerly-fetched local vmstate for restore.
	restoreStatePath := localStatePath

	// With per-VM namespaces, tap-slot-0 already exists in the namespace — no rename needed.
	vmIface, proxy, err := cm.restoreAndActivateRunner(
		ctx,
		runnerID,
		req,
		runner,
		netns,
		tap,
		vmCfg,
		restoreStatePath,
		localMemPath,
		uffdSocketPath,
		useFileBackedMem,
		extensionDrivePaths,
	)
	if err != nil {
		return nil, err
	}
	vm, ok := vmIface.(*firecracker.VM)
	if !ok {
		if vmIface != nil {
			vmIface.Stop()
		}
		return nil, fmt.Errorf("unexpected VM implementation type %T", vmIface)
	}

	runner.State = StateIdle
	runner.StartedAt = time.Now()

	if err := cm.registerAllocatedRunner(runnerID, runner, vm, fuseDisk, extensionFUSEDisks, uffdHandler, proxy); err != nil {
		retErr = err
		return nil, retErr
	}

	restoreTime := time.Since(startTime)
	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":    runnerID,
		"ip":           runner.InternalIP.String(),
		"restore_time": restoreTime,
	}).Info("Runner allocated with chunked snapshot")

	cleanupOnError = false
	allocatedRunner = runner
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

	// Stop auth proxy
	if proxy, exists := cm.authProxies[runnerID]; exists {
		proxy.Stop()
		delete(cm.authProxies, runnerID)
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

	// Stop auth proxy if one exists
	if proxy, ok := cm.authProxies[runnerID]; ok {
		proxy.Stop()
		delete(cm.authProxies, runnerID)
	}

	// Cleanup rootfs FUSE disk
	if disk, exists := cm.fuseDisks[runnerID]; exists {
		disk.Unmount()
		delete(cm.fuseDisks, runnerID)
	}
	// Cleanup extension drive FUSE disks
	if extDisks, exists := cm.fuseExtensionDisks[runnerID]; exists {
		for _, disk := range extDisks {
			disk.Unmount()
		}
		delete(cm.fuseExtensionDisks, runnerID)
	}

	// Cleanup network
	cm.netnsNetwork.ReleaseNamespace(runnerID)
	if slot, ok := cm.runnerToSlot[runnerID]; ok {
		delete(cm.slotToRunner, slot)
		delete(cm.runnerToSlot, runnerID)
	}

	// Cleanup workspace
	workspaceDir := filepath.Join(cm.config.WorkspaceDir, runnerID)
	os.RemoveAll(workspaceDir)

	// Cleanup socket
	os.Remove(filepath.Join(cm.config.SocketDir, runnerID+".sock"))
	os.Remove(filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock"))

	delete(cm.runners, runnerID)

	return nil
}

// cleanupChunkedRunner cleans up resources on allocation failure
func (cm *ChunkedManager) cleanupChunkedRunner(
	runnerID string,
	tap *network.TapDevice,
	netns *network.VMNamespace,
	fuseDisk *fuse.ChunkedDisk,
	extensionDisks map[string]*fuse.ChunkedDisk,
	uffdHandler *uffd.Handler,
	proxy *authproxy.AuthProxy,
) {
	// Stop auth proxy if started
	if proxy != nil {
		proxy.Stop()
	}
	if uffdHandler != nil {
		uffdHandler.Stop()
	}
	if fuseDisk != nil {
		fuseDisk.Unmount()
	}
	for _, disk := range extensionDisks {
		disk.Unmount()
	}
	if netns != nil {
		cm.netnsNetwork.ReleaseNamespace(runnerID)
	}
	cm.releaseRunnerSlot(runnerID)
	workspaceDir := filepath.Join(cm.config.WorkspaceDir, runnerID)
	os.RemoveAll(workspaceDir)
	os.Remove(filepath.Join(cm.config.SocketDir, runnerID+".sock"))
	os.Remove(filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock"))
}

// setupChunkedSymlinks creates symlinks from the snapshot's baked-in drive paths
// (/tmp/snapshot/*.img) to the actual FUSE-backed or local paths on this host.
// Firecracker opens drive backing files during LoadSnapshot at the paths recorded
// in the snapshot state file. Returns a cleanup function to remove the symlinks
// after restore (Firecracker holds open fds, so symlinks can be removed).
func (cm *ChunkedManager) setupChunkedSymlinks(rootfsPath string, extensionDrivePaths map[string]string) (func(), error) {
	if err := os.MkdirAll(snapshotSymlinkDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}

	symlinks := []struct {
		name   string
		target string
	}{
		{"rootfs.img", rootfsPath},
	}
	// Add extension drives by driveID. The snapshot-builder bakes in paths using
	// driveID+".img" (e.g. "bazel_output.img"), so we must match that exactly.
	for driveID, path := range extensionDrivePaths {
		name := driveID + ".img"
		symlinks = append(symlinks, struct{ name, target string }{name, path})
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
	for _, perRunner := range cm.fuseExtensionDisks {
		for _, disk := range perRunner {
			ds := disk.Stats()
			stats.TotalDiskReads += ds.Reads
			stats.TotalDiskWrites += ds.Writes
			stats.TotalDirtyChunks += ds.DirtyChunks
		}
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
	for id, perRunner := range cm.fuseExtensionDisks {
		for _, disk := range perRunner {
			disk.Unmount()
		}
		delete(cm.fuseExtensionDisks, id)
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

// waitForThawAgent polls the thaw-agent /alive endpoint until it returns
// HTTP 200 or the timeout expires. This ensures the VM is functional after
// snapshot restore before we expose it to the scheduler.
func (cm *ChunkedManager) waitForThawAgent(ctx context.Context, ip string, timeout time.Duration) error {
	aliveURL := fmt.Sprintf("http://%s:%d/alive", ip, snapshot.ThawAgentDebugPort)
	deadline := time.Now().Add(timeout)
	pollInterval := 200 * time.Millisecond

	client := &http.Client{Timeout: 2 * time.Second}

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		resp, err := client.Get(aliveURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				cm.chunkedLogger.WithField("url", aliveURL).Debug("Thaw-agent ready")
				return nil
			}
		}

		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return fmt.Errorf("thaw-agent at %s did not become ready within %v", aliveURL, timeout)
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

// GetSubnet returns the subnet from the netns network
func (cm *ChunkedManager) GetSubnet() *net.IPNet {
	return cm.netnsNetwork.GetSubnet()
}

// getAllDirtyExtensionDiskChunks returns all dirty FUSE extension disk chunks for a runner.
// Returns driveID → (chunkIndex → data). Used as a callback by Manager.PauseRunner.
func (cm *ChunkedManager) getAllDirtyExtensionDiskChunks(runnerID string) map[string]map[int][]byte {
	cm.mu.RLock()
	perRunner, ok := cm.fuseExtensionDisks[runnerID]
	cm.mu.RUnlock()
	if !ok || len(perRunner) == 0 {
		return nil
	}
	result := make(map[string]map[int][]byte, len(perRunner))
	for driveID, disk := range perRunner {
		dirty := disk.GetDirtyChunks()
		if len(dirty) > 0 {
			result[driveID] = dirty
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// setupExtensionFUSEDiskForRunner creates and mounts a FUSE-backed extension disk.
// Used by Manager.ResumeFromSession for GCS-backed cross-host resume.
func (cm *ChunkedManager) setupExtensionFUSEDiskForRunner(runnerID, driveID string, chunks []snapshot.ChunkRef, totalSize, chunkSize int64) (string, error) {
	fuseMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse-ext-"+driveID)
	if err := os.MkdirAll(fuseMountDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create FUSE ext mount dir for %s: %w", driveID, err)
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
		return "", fmt.Errorf("failed to create FUSE ext disk %s: %w", driveID, err)
	}

	if err := fuseDisk.Mount(); err != nil {
		return "", fmt.Errorf("failed to mount FUSE ext disk %s: %w", driveID, err)
	}

	cm.mu.Lock()
	if cm.fuseExtensionDisks[runnerID] == nil {
		cm.fuseExtensionDisks[runnerID] = make(map[string]*fuse.ChunkedDisk)
	}
	cm.fuseExtensionDisks[runnerID][driveID] = fuseDisk
	cm.mu.Unlock()

	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"drive_id":  driveID,
		"chunks":    len(chunks),
	}).Info("FUSE extension disk mounted for session resume")

	return fuseDisk.DiskImagePath(), nil
}

// getDirtyRootfsDiskChunksCallback returns dirty FUSE rootfs disk chunks for a runner.
// Used as a callback by Manager.PauseRunner for GCS-backed rootfs upload.
func (cm *ChunkedManager) getDirtyRootfsDiskChunksCallback(runnerID string) map[int][]byte {
	cm.mu.RLock()
	disk, ok := cm.fuseDisks[runnerID]
	cm.mu.RUnlock()
	if !ok || disk == nil {
		return nil
	}
	return disk.GetDirtyChunks()
}

// cleanupFUSEDisksForRunner unmounts and removes all FUSE disks for a runner.
// Called during pause/checkpoint after VM stop so the next resume can create
// fresh mounts without collisions.
func (cm *ChunkedManager) cleanupFUSEDisksForRunner(runnerID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if disk, ok := cm.fuseDisks[runnerID]; ok {
		disk.Unmount()
		delete(cm.fuseDisks, runnerID)
		cm.chunkedLogger.WithField("runner_id", runnerID).Debug("Cleaned up rootfs FUSE disk during pause")
	}
	if extDisks, ok := cm.fuseExtensionDisks[runnerID]; ok {
		for driveID, disk := range extDisks {
			disk.Unmount()
			cm.chunkedLogger.WithFields(logrus.Fields{"runner_id": runnerID, "drive_id": driveID}).Debug("Cleaned up extension FUSE disk during pause")
		}
		delete(cm.fuseExtensionDisks, runnerID)
	}
}

// setupRootfsFUSEDiskForRunner creates and mounts a FUSE-backed rootfs disk.
// Used by Manager.ResumeFromSession for GCS-backed cross-host resume.
func (cm *ChunkedManager) setupRootfsFUSEDiskForRunner(runnerID string, chunks []snapshot.ChunkRef, totalSize, chunkSize int64) (string, error) {
	fuseMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse-rootfs")
	if err := os.MkdirAll(fuseMountDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create FUSE rootfs mount dir: %w", err)
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
		return "", fmt.Errorf("failed to create FUSE rootfs disk: %w", err)
	}

	if err := fuseDisk.Mount(); err != nil {
		return "", fmt.Errorf("failed to mount FUSE rootfs disk: %w", err)
	}

	cm.mu.Lock()
	cm.fuseDisks[runnerID] = fuseDisk
	cm.mu.Unlock()

	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"chunks":    len(chunks),
	}).Info("FUSE rootfs disk mounted for session resume")

	return fuseDisk.DiskImagePath(), nil
}

// createExt4Image creates a sparse ext4 filesystem image of the given size.
func createExt4Image(path string, sizeGB int, label string) error {
	if sizeGB <= 0 {
		return fmt.Errorf("invalid sizeGB: %d", sizeGB)
	}
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", sizeGB), path).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", label, "-E", "lazy_itable_init=1,lazy_journal_init=1", path).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}
	return nil
}
