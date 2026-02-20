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
	chunkStore  *snapshot.ChunkStore
	chunkCache  *snapshot.LRUCache
	chunkedMeta *snapshot.ChunkedSnapshotMetadata

	// Per-runner UFFD handlers and FUSE disks
	uffdHandlers      map[string]*uffd.Handler
	fuseDisks         map[string]*fuse.ChunkedDisk // rootfs FUSE disks per runner
	fuseSeedDisks     map[string]*fuse.ChunkedDisk // repo-cache-seed FUSE disks per runner

	// Network namespace manager (alternative to slot-based TAPs)
	netnsNetwork *network.NetNSNetwork
	useNetNS     bool

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

	// ChunkCacheSizeBytes is the max size of the local chunk LRU cache
	ChunkCacheSizeBytes int64
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
		uffdHandlers:  make(map[string]*uffd.Handler),
		fuseDisks:     make(map[string]*fuse.ChunkedDisk),
		fuseSeedDisks: make(map[string]*fuse.ChunkedDisk),
		useNetNS:      cfg.UseNetNS,
		chunkedLogger: logger.WithField("component", "chunked-manager"),
	}

	// Setup chunked snapshot infrastructure if enabled
	if cfg.UseChunkedSnapshots {
		// Create chunk store with in-memory LRU cache
		cacheSize := cfg.ChunkCacheSizeBytes
		if cacheSize <= 0 {
			cacheSize = 8 * 1024 * 1024 * 1024 // 8GB default
		}

		chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
			GCSBucket:           cfg.SnapshotBucket,
			LocalCachePath:      filepath.Join(cfg.SnapshotCachePath, "chunks"),
			ChunkCacheSizeBytes: cacheSize,
			Logger:              logger,
		})
		if err != nil {
			baseManager.Close()
			return nil, fmt.Errorf("failed to create chunk store: %w", err)
		}
		cm.chunkStore = chunkStore

		// Start eager prefetcher for background chunk loading
		chunkStore.StartEagerFetcher()
		cm.chunkedLogger.Info("Started eager chunk prefetcher")

		// Create separate LRU cache for runner-level caching
		cm.chunkCache = snapshot.NewLRUCache(cacheSize)

		// Try to load chunked metadata
		meta, err := chunkStore.LoadChunkedMetadata(ctx, "current")
		if err != nil {
			cm.chunkedLogger.WithError(err).Warn("No chunked snapshot metadata found, will use traditional restore")
		} else {
			cm.chunkedMeta = meta
			cm.chunkedLogger.WithFields(logrus.Fields{
				"version":     meta.Version,
				"mem_chunks":  len(meta.MemChunks),
				"disk_chunks": len(meta.RootfsChunks),
				"total_mem":   meta.TotalMemSize,
				"total_disk":  meta.TotalDiskSize,
			}).Info("Loaded chunked snapshot metadata")
		}
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
		cm.chunkedLogger.Info("Network namespace mode enabled")
	}

	return cm, nil
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

	// Check if we can use chunked restore
	if cm.chunkedMeta == nil || cm.chunkStore == nil {
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
		netns, err = cm.netnsNetwork.CreateNamespaceForVM(runnerID)
		if err != nil {
			return nil, fmt.Errorf("failed to create network namespace: %w", err)
		}
		tap = netns.GetTapDevice(cm.netnsNetwork.GetSubnet())
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
		Chunks:     cm.chunkedMeta.RootfsChunks,
		TotalSize:  cm.chunkedMeta.TotalDiskSize,
		ChunkSize:  cm.chunkedMeta.ChunkSize,
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
	if len(cm.chunkedMeta.RepoCacheSeedChunks) > 0 {
		fuseSeedMountDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "fuse-seed")
		if err := os.MkdirAll(fuseSeedMountDir, 0755); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("failed to create FUSE seed mount dir: %w", err)
		}

		// Compute total repo-cache-seed size from chunks
		var totalSeedSize int64
		for _, c := range cm.chunkedMeta.RepoCacheSeedChunks {
			end := c.Offset + c.Size
			if end > totalSeedSize {
				totalSeedSize = end
			}
		}

		fuseSeedDisk, err = fuse.NewChunkedDisk(fuse.ChunkedDiskConfig{
			ChunkStore: cm.chunkStore,
			Chunks:     cm.chunkedMeta.RepoCacheSeedChunks,
			TotalSize:  totalSeedSize,
			ChunkSize:  cm.chunkedMeta.ChunkSize,
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

	// Setup memory backend: either file-backed (new) or UFFD lazy loading (legacy).
	useFileBackedMem := cm.chunkedMeta.MemFilePath != ""
	var uffdHandler *uffd.Handler
	var uffdSocketPath string
	var localMemPath string

	if useFileBackedMem {
		// New-style: memory was downloaded as a single file at manager startup.
		// Use file-backed restore — no UFFD handler needed.
		localMemPath = filepath.Join(cm.config.SnapshotCachePath, "snapshot.mem")
		if _, err := os.Stat(localMemPath); err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, nil)
			return nil, fmt.Errorf("raw memory file not found at %s (should have been downloaded at startup): %w", localMemPath, err)
		}
		cm.chunkedLogger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"mem_path":  localMemPath,
		}).Info("Using file-backed memory restore")
	} else {
		// Legacy: UFFD lazy memory loading from chunk store.
		uffdSocketPath = filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock")
		uffdHandler, err = uffd.NewHandler(uffd.HandlerConfig{
			SocketPath: uffdSocketPath,
			ChunkStore: cm.chunkStore,
			Metadata:   cm.chunkedMeta,
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

	// Eagerly fetch the VM state (CPU/device state) from the ChunkStore.
	// This is small (~100KB) and required as a local file for Firecracker restore.
	snapshotDir := filepath.Join(cm.config.WorkspaceDir, runnerID, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to create snapshot dir: %w", err)
	}

	localStatePath := filepath.Join(snapshotDir, "snapshot.state")
	if cm.chunkedMeta.StateHash != "" {
		stateData, err := cm.chunkStore.GetChunk(ctx, cm.chunkedMeta.StateHash)
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
	// file we need is the kernel, which was eagerly fetched at manager startup.
	kernelPath := filepath.Join(cm.config.SnapshotCachePath, "kernel.bin")
	if _, err := os.Stat(kernelPath); err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("kernel not found at %s (should have been eagerly fetched at startup): %w", kernelPath, err)
	}

	// Create runner record
	runner := &Runner{
		ID:              runnerID,
		HostID:          cm.config.HostID,
		State:           StateBooting,
		InternalIP:      tap.IP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: cm.chunkedMeta.Version,
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
	// If this runner uses a different slot, temporarily rename its TAP.
	tapRestore, err := cm.setupSnapshotTAPRename(tap.Name)
	if err != nil {
		symlinkCleanup()
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to setup TAP rename: %w", err)
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
	tapRestore()
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

				newVersion := fmt.Sprintf("%s-%s", cm.chunkedMeta.Version, runnerID[:8])
				newMeta, err := uploader.UploadIncrementalSnapshot(ctx, cm.chunkedMeta, dirtyChunks, nil, newVersion)
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

	if cm.chunkCache != nil {
		cacheStats := cm.chunkCache.Stats()
		stats.CacheSize = cacheStats.Size
		stats.CacheMaxSize = cacheStats.MaxSize
		stats.CacheItems = cacheStats.ItemCount
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
	// LRU Cache stats
	CacheSize    int64
	CacheMaxSize int64
	CacheItems   int

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

	// Close chunk store
	if cm.chunkStore != nil {
		cm.chunkStore.Close()
	}

	// Close base manager
	return cm.Manager.Close()
}

// GetChunkedMetadata returns the loaded chunked snapshot metadata (may be nil).
func (cm *ChunkedManager) GetChunkedMetadata() *snapshot.ChunkedSnapshotMetadata {
	return cm.chunkedMeta
}

// GetChunkStore returns the underlying chunk store.
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
