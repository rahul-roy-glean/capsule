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
	uffdHandlers map[string]*uffd.Handler
	fuseDisks    map[string]*fuse.ChunkedDisk

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
		useNetNS:      cfg.UseNetNS,
		chunkedLogger: logger.WithField("component", "chunked-manager"),
	}

	// Setup chunked snapshot infrastructure if enabled
	if cfg.UseChunkedSnapshots {
		// Create chunk store with in-memory LRU cache
		cacheSize := cfg.ChunkCacheSizeBytes
		if cacheSize <= 0 {
			cacheSize = 2 * 1024 * 1024 * 1024 // 2GB default
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
				"version":      meta.Version,
				"mem_chunks":   len(meta.MemChunks),
				"disk_chunks":  len(meta.RootfsChunks),
				"total_mem":    meta.TotalMemSize,
				"total_disk":   meta.TotalDiskSize,
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

	// Setup UFFD handler for lazy memory loading
	uffdSocketPath := filepath.Join(cm.config.SocketDir, runnerID+".uffd.sock")
	uffdHandler, err := uffd.NewHandler(uffd.HandlerConfig{
		SocketPath: uffdSocketPath,
		ChunkStore: cm.chunkStore,
		Metadata:   cm.chunkedMeta,
		MemStart:   0, // Will be set by Firecracker
		MemSize:    uint64(cm.chunkedMeta.TotalMemSize),
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

	// Get snapshot state path (kernel state, not memory - that's via UFFD)
	snapshotPaths, err := cm.snapshotCache.GetSnapshotPaths()
	if err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to get snapshot paths: %w", err)
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

	// Create VM configuration
	vmCfg := firecracker.VMConfig{
		VMID:           runnerID,
		SocketDir:      cm.config.SocketDir,
		FirecrackerBin: cm.config.FirecrackerBin,
		KernelPath:     snapshotPaths.Kernel,
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
		LogPath:     runner.LogPath,
		MetricsPath: runner.MetricsPath,
	}

	// Create VM instance
	vm, err := firecracker.NewVM(vmCfg, cm.logger.Logger)
	if err != nil {
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// Restore from snapshot with UFFD backend for memory
	cm.chunkedLogger.WithFields(logrus.Fields{
		"runner_id":   runnerID,
		"snapshot":    snapshotPaths.State,
		"uffd_socket": uffdSocketPath,
	}).Info("Restoring VM with UFFD memory backend")

	if err := vm.RestoreFromSnapshotWithUFFD(ctx, snapshotPaths.State, uffdSocketPath, true); err != nil {
		cm.chunkedLogger.WithError(err).Warn("UFFD restore failed, trying traditional restore")
		vm.Stop()

		// Fallback to traditional restore
		vm, err = firecracker.NewVM(vmCfg, cm.logger.Logger)
		if err != nil {
			cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
			return nil, fmt.Errorf("failed to recreate VM: %w", err)
		}

		if snapshotPaths.Mem != "" && snapshotPaths.State != "" {
			if err := vm.RestoreFromSnapshot(ctx, snapshotPaths.State, snapshotPaths.Mem, true); err != nil {
				cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
				return nil, fmt.Errorf("traditional restore also failed: %w", err)
			}
		} else {
			if err := vm.Start(ctx); err != nil {
				cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
				return nil, fmt.Errorf("cold boot failed: %w", err)
			}
		}
	}

	// Inject MMDS data
	mmdsData := cm.buildMMDSData(ctx, runner, tap, req)
	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		cm.cleanupChunkedRunner(runnerID, tap, netns, fuseDisk, uffdHandler)
		return nil, fmt.Errorf("failed to set MMDS data: %w", err)
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

	// Cleanup FUSE disk
	if disk, exists := cm.fuseDisks[runnerID]; exists {
		disk.Unmount()
		delete(cm.fuseDisks, runnerID)
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

// GetSubnet returns the subnet from either netns or nat network
func (cm *ChunkedManager) GetSubnet() *net.IPNet {
	if cm.useNetNS && cm.netnsNetwork != nil {
		return cm.netnsNetwork.GetSubnet()
	}
	return cm.network.GetSubnet()
}
