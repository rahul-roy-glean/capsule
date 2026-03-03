package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/ci"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/firecracker"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

type recentAllocation struct {
	runner    *Runner
	allocTime time.Time
}

// uffdStopper is implemented by UFFD handlers that need cleanup on runner release.
type uffdStopper interface {
	Stop()
}

// Manager manages the lifecycle of runners on a host
type Manager struct {
	config         HostConfig
	runners        map[string]*Runner
	recentRequests map[string]*recentAllocation // keyed by RequestID, TTL 5min
	vms            map[string]*firecracker.VM
	uffdHandlers   map[string]uffdStopper // layered UFFD handlers per runner (session resume)
	snapshotCache  *snapshot.Cache
	network        *network.NATNetwork
	// credentialsImage is an ext4 image containing credentials (e.g. Buildbarn certs),
	// attached read-only to each microVM for Bazel remote cache/execution TLS config.
	credentialsImage string
	// gitCacheImage is an ext4 image containing git repository mirrors, attached
	// read-only to each microVM for fast reference cloning.
	gitCacheImage string
	// ciAdapter provides CI system integration (runner registration, drain, etc.)
	ciAdapter ci.Adapter
	// netnsNetwork manages per-VM network namespaces (nil if using legacy bridge mode).
	// When set, each VM gets its own namespace with point-to-point veth routing
	// instead of sharing the fcbr0 bridge.
	netnsNetwork *network.NetNSNetwork
	// slotToRunner tracks which runner is using each TAP slot (for snapshot restore).
	// Key is slot number (0, 1, 2, ...), value is runner ID.
	slotToRunner map[int]string
	// runnerToSlot is the reverse mapping for quick lookup during cleanup.
	runnerToSlot map[string]int
	draining     bool
	mu           sync.RWMutex
	logger       *logrus.Entry

	// Runner pool for VM reuse (nil if pooling disabled)
	pool *Pool

	// sessionMemStore and sessionDiskStore are chunk stores for GCS-backed
	// session pause/resume (nil when SessionChunkBucket is not configured).
	// When non-nil, PauseRunner uploads dirty diff chunks to GCS and
	// ResumeFromSession fetches chunks lazily via UFFD from any host.
	sessionMemStore  *snapshot.ChunkStore
	sessionDiskStore *snapshot.ChunkStore

	// goldenChunkedMeta holds the base snapshot metadata used as the reference
	// for session pause uploads. Only populated when sessionMemStore is non-nil.
	goldenChunkedMeta *snapshot.ChunkedSnapshotMetadata

	// getDirtyExtensionDiskChunks returns all dirty FUSE extension disk chunks for a runner.
	// Returns a map of driveID → dirty chunks. Nil when FUSE disks are not in use.
	getDirtyExtensionDiskChunks func(runnerID string) map[string]map[int][]byte

	// setupExtensionFUSEDisk is a callback set by ChunkedManager that creates
	// and mounts a FUSE-backed disk for a specific extension drive.
	// Returns the disk image path. Used by ResumeFromSession.
	setupExtensionFUSEDisk func(runnerID, driveID string, chunks []snapshot.ChunkRef, totalSize, chunkSize int64) (diskImagePath string, err error)

	// getDirtyRootfsDiskChunks returns dirty FUSE rootfs disk chunks for a runner.
	// Nil when FUSE rootfs disks are not in use (non-chunked mode).
	getDirtyRootfsDiskChunks func(runnerID string) map[int][]byte

	// setupRootfsFUSEDisk creates and mounts a FUSE-backed rootfs disk from
	// ChunkRefs during GCS-backed session resume. Returns the disk image path.
	setupRootfsFUSEDisk func(runnerID string, chunks []snapshot.ChunkRef, totalSize, chunkSize int64) (diskImagePath string, err error)

	// policyEnforcers tracks per-VM PolicyEnforcers for network policy enforcement.
	// Key is runner ID. nil map when no policies are in use.
	policyEnforcers map[string]*network.PolicyEnforcer
}

type QuarantineOptions struct {
	Reason      string
	BlockEgress *bool
	PauseVM     *bool
}

type UnquarantineOptions struct {
	UnblockEgress *bool
	ResumeVM      *bool
}

// NewManager creates a new runner manager
func NewManager(ctx context.Context, cfg HostConfig, ciAdapter ci.Adapter, logger *logrus.Logger) (*Manager, error) {
	if logger == nil {
		logger = logrus.New()
	}

	// Create snapshot cache
	cache, err := snapshot.NewCache(ctx, snapshot.CacheConfig{
		LocalPath:   cfg.SnapshotCachePath,
		GCSBucket:   cfg.SnapshotBucket,
		WorkloadKey: cfg.WorkloadKey,
		Logger:      logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot cache: %w", err)
	}

	// Create NAT network
	natNet, err := network.NewNATNetwork(network.NATConfig{
		BridgeName:    cfg.BridgeName,
		Subnet:        cfg.MicroVMSubnet,
		ExternalIface: cfg.ExternalInterface,
		Logger:        logger,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create NAT network: %w", err)
	}

	// Setup network
	if err := natNet.Setup(); err != nil {
		return nil, fmt.Errorf("failed to setup NAT network: %w", err)
	}

	// Ensure directories exist
	for _, dir := range []string{cfg.SocketDir, cfg.WorkspaceDir, cfg.LogDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	credentialsImg, err := ensureCredentialsImage(cfg, logger.WithField("component", "runner-manager"))
	if err != nil {
		return nil, fmt.Errorf("failed to prepare credentials image: %w", err)
	}

	// Check if git-cache image exists (created by startup script)
	gitCacheImg := ""
	if cfg.Bazel.GitCacheEnabled && cfg.Bazel.GitCacheImagePath != "" {
		if _, err := os.Stat(cfg.Bazel.GitCacheImagePath); err == nil {
			gitCacheImg = cfg.Bazel.GitCacheImagePath
			logger.WithField("git_cache_image", gitCacheImg).Info("Git-cache image found")
		} else {
			logger.WithField("git_cache_image", cfg.Bazel.GitCacheImagePath).Warn("Git-cache enabled but image not found")
		}
	}

	m := &Manager{
		config:           cfg,
		runners:          make(map[string]*Runner),
		recentRequests:   make(map[string]*recentAllocation),
		vms:              make(map[string]*firecracker.VM),
		uffdHandlers:     make(map[string]uffdStopper),
		snapshotCache:    cache,
		network:          natNet,
		credentialsImage: credentialsImg,
		gitCacheImage:    gitCacheImg,
		ciAdapter:        ciAdapter,
		slotToRunner:     make(map[int]string),
		runnerToSlot:     make(map[string]int),
		policyEnforcers:  make(map[string]*network.PolicyEnforcer),
		logger:           logger.WithField("component", "runner-manager"),
	}

	// Initialize runner pool if enabled
	if cfg.PoolEnabled {
		poolCfg := PoolConfig{
			Enabled:          true,
			MaxPooledRunners: cfg.PoolMaxRunners,
		}
		if cfg.PoolMaxTotalMemoryGB > 0 {
			poolCfg.MaxTotalMemoryBytes = int64(cfg.PoolMaxTotalMemoryGB) * 1024 * 1024 * 1024
		}
		if cfg.PoolMaxRunnerMemoryGB > 0 {
			poolCfg.MaxRunnerMemoryBytes = int64(cfg.PoolMaxRunnerMemoryGB) * 1024 * 1024 * 1024
		}
		if cfg.PoolMaxRunnerDiskGB > 0 {
			poolCfg.MaxRunnerDiskBytes = int64(cfg.PoolMaxRunnerDiskGB) * 1024 * 1024 * 1024
		}
		if cfg.PoolRecycleTimeoutSecs > 0 {
			poolCfg.RecycleTimeout = time.Duration(cfg.PoolRecycleTimeoutSecs) * time.Second
		}

		m.pool = NewPool(poolCfg, logger)
		m.pool.SetCallbacks(
			m.pauseRunnerVM,
			m.resumeRunnerVM,
			m.getRunnerVMStats,
			m.removeRunnerVM,
		)
		logger.WithFields(logrus.Fields{
			"max_runners":   poolCfg.MaxPooledRunners,
			"max_memory_gb": cfg.PoolMaxTotalMemoryGB,
		}).Info("Runner pooling enabled")
	}

	// Start idempotency cleanup loop
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			m.cleanupRecentRequests()
		}
	}()

	return m, nil
}

// SetSessionStores configures the Manager for GCS-backed session pause/resume.
// Called by ChunkedManager after its chunk stores are created so that sessions
// reuse the same GCS bucket and chunk stores as the CI snapshot pipeline —
// no separate SessionChunkBucket needed.
// goldenMeta is used as the base for memory diff merging on pause.
func (m *Manager) SetSessionStores(memStore, diskStore *snapshot.ChunkStore, goldenMeta *snapshot.ChunkedSnapshotMetadata) {
	m.sessionMemStore = memStore
	m.sessionDiskStore = diskStore
	m.goldenChunkedMeta = goldenMeta
}

// SetGoldenChunkedMeta updates the golden metadata used as the base for
// session pause uploads. Called after SyncManifest loads a new version.
func (m *Manager) SetGoldenChunkedMeta(meta *snapshot.ChunkedSnapshotMetadata) {
	m.mu.Lock()
	m.goldenChunkedMeta = meta
	m.mu.Unlock()
}

// cleanupRecentRequests removes expired idempotency entries (>5 min old).
func (m *Manager) cleanupRecentRequests() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for reqID, alloc := range m.recentRequests {
		if time.Since(alloc.allocTime) > 5*time.Minute {
			delete(m.recentRequests, reqID)
		}
	}
}

// SetNetNSNetwork configures the manager to use per-VM network namespaces
// instead of the shared bridge. When set, AllocateRunner creates a namespace
// per VM with point-to-point veth routing, and Firecracker is launched inside
// the namespace.
func (m *Manager) SetNetNSNetwork(netnsNet *network.NetNSNetwork) {
	m.netnsNetwork = netnsNet
}

// GetNetNSNetwork returns the netns network manager (may be nil).
func (m *Manager) GetNetNSNetwork() *network.NetNSNetwork {
	return m.netnsNetwork
}

func (m *Manager) IsDraining() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.draining
}

func (m *Manager) SetDraining(draining bool) (changed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining == draining {
		return false
	}
	m.draining = draining
	return true
}

// AllocateRunner allocates a new runner
func (m *Manager) AllocateRunner(ctx context.Context, req AllocateRequest) (*Runner, error) {
	// Idempotency check: if we've already allocated for this RequestID, return existing runner
	if req.RequestID != "" {
		m.mu.RLock()
		if recent, ok := m.recentRequests[req.RequestID]; ok {
			if time.Since(recent.allocTime) < 5*time.Minute {
				if existingRunner, exists := m.runners[recent.runner.ID]; exists {
					m.mu.RUnlock()
					m.logger.WithFields(logrus.Fields{
						"runner_id":  existingRunner.ID,
						"request_id": req.RequestID,
					}).Info("Returning existing runner for duplicate request")
					return existingRunner, nil
				}
			}
		}
		m.mu.RUnlock()
	}

	// Build pool key from request (before locking, as pool.Get needs the key)
	var poolKey *RunnerKey
	if m.pool != nil {
		// Get snapshot version for pool key
		snapshotPaths, err := m.snapshotCache.GetSnapshotPaths()
		if err == nil {
			poolKey = &RunnerKey{
				SnapshotVersion: snapshotPaths.Version,
				Platform:        "linux/amd64",
				GitHubRepo:      req.Repo,
				Labels:          req.Labels,
			}
		}
	}

	// Try to get from pool first (before acquiring main lock)
	if m.pool != nil && poolKey != nil {
		if pooled := m.pool.Get(ctx, poolKey); pooled != nil {
			m.mu.Lock()
			defer m.mu.Unlock()

			m.logger.WithField("runner_id", pooled.Runner.ID).Info("Reusing pooled runner")

			// Update runner state
			pooled.Runner.State = StateBusy
			pooled.Runner.TaskCount++
			pooled.Runner.JobID = req.RequestID

			// Update MMDS with new task data
			if vm, ok := m.vms[pooled.Runner.ID]; ok {
				var tap *network.TapDevice
				if slot, hasSlot := m.runnerToSlot[pooled.Runner.ID]; hasSlot {
					tap, _ = m.network.GetOrCreateTapSlot(slot, pooled.Runner.ID)
				}
				if tap != nil {
					mmdsData := m.buildMMDSData(ctx, pooled.Runner, tap, req)
					if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
						m.logger.WithError(err).Warn("Failed to update MMDS for pooled runner")
					}
				}
			}

			return pooled.Runner, nil
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.draining {
		return nil, fmt.Errorf("host is draining")
	}

	// Check capacity
	if len(m.runners) >= m.config.MaxRunners {
		return nil, fmt.Errorf("host at capacity: %d/%d runners", len(m.runners), m.config.MaxRunners)
	}

	runnerID := uuid.New().String()
	allocStart := time.Now()
	m.logger.WithField("runner_id", runnerID).Info("Allocating new runner")

	// Get snapshot paths first to determine if we should use slot-based TAP allocation
	snapshotPaths, err := m.snapshotCache.GetSnapshotPaths()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot paths: %w", err)
	}

	// Determine if snapshot restore is available
	useSnapshotRestore := snapshotPaths.Mem != "" && snapshotPaths.State != ""

	// Allocate network resources.
	// When using per-VM namespaces, each VM gets its own namespace with
	// tap-slot-0 inside it — no TAP rename needed, no shared bridge.
	// Otherwise, use the legacy slot-based TAP allocation on the shared bridge.
	var tap *network.TapDevice
	var nsInfo *network.VMNamespace
	var slot int = -1
	useNetNS := m.netnsNetwork != nil

	if useNetNS {
		// Per-VM namespace mode: create isolated namespace with inner bridge + TAP
		slot = m.findAvailableSlot()
		if slot < 0 {
			return nil, fmt.Errorf("no slots available (all %d slots in use)", m.config.MaxRunners)
		}
		nsInfo, err = m.netnsNetwork.CreateNamespaceForVM(runnerID, slot)
		if err != nil {
			return nil, fmt.Errorf("failed to create network namespace: %w", err)
		}
		tap = nsInfo.GetTapDevice(m.netnsNetwork.GetSubnet())
		m.slotToRunner[slot] = runnerID
		m.runnerToSlot[runnerID] = slot
		m.logger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"slot":      slot,
			"namespace": nsInfo.Name,
			"tap":       tap.Name,
		}).Debug("Using per-VM namespace with inner bridge")
	} else if useSnapshotRestore {
		// Legacy: slot-based TAP on shared bridge for snapshot restore
		slot = m.findAvailableSlot()
		if slot < 0 {
			return nil, fmt.Errorf("no TAP slots available (all %d slots in use)", m.config.MaxRunners)
		}
		tap, err = m.network.GetOrCreateTapSlot(slot, runnerID)
		if err != nil {
			return nil, fmt.Errorf("failed to get TAP slot %d: %w", slot, err)
		}
		m.slotToRunner[slot] = runnerID
		m.runnerToSlot[runnerID] = slot
		m.logger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"slot":      slot,
			"tap":       tap.Name,
		}).Debug("Using slot-based TAP for snapshot restore")
	} else {
		// Legacy: dynamic TAP allocation for cold boot
		tap, err = m.network.CreateTapForVM(runnerID)
		if err != nil {
			return nil, fmt.Errorf("failed to create TAP device: %w", err)
		}
		m.logger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"tap":       tap.Name,
		}).Debug("Using dynamic TAP for cold boot")
	}

	// Create rootfs overlay
	overlayStart := time.Now()
	overlayPath, err := m.snapshotCache.CreateOverlay(runnerID)
	if err != nil {
		m.network.ReleaseTap(runnerID)
		return nil, fmt.Errorf("failed to create rootfs overlay: %w", err)
	}
	overlayDur := time.Since(overlayStart)

	// Create per-runner writable repo cache layer image (upperdir/workdir lives here)
	repoCacheStart := time.Now()
	var repoCacheUpperPath string
	if m.config.Bazel.RepoCacheUpperSizeGB > 0 {
		repoCacheUpperPath = filepath.Join(m.config.WorkspaceDir, runnerID, "repo-cache-upper.img")
		if err := os.MkdirAll(filepath.Dir(repoCacheUpperPath), 0755); err != nil {
			m.cleanupRunner(runnerID, tap.Name, overlayPath, "")
			return nil, fmt.Errorf("failed to create repo-cache-upper directory: %w", err)
		}
		if err := createExt4Image(repoCacheUpperPath, m.config.Bazel.RepoCacheUpperSizeGB, "BAZEL_REPO_UPPER"); err != nil {
			m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
			return nil, fmt.Errorf("failed to create repo-cache-upper image: %w", err)
		}
	}
	repoCacheDur := time.Since(repoCacheStart)

	// Create runner record.
	// When using per-VM namespaces, InternalIP is set to the host-reachable
	// veth IP (10.0.{slot}.2) so the host proxy can reach the VM's services.
	// The guest still uses 172.16.0.2 internally (configured via boot args).
	internalIP := tap.IP
	if useNetNS && nsInfo != nil {
		internalIP = nsInfo.HostReachableIP
	}
	runner := &Runner{
		ID:              runnerID,
		HostID:          m.config.HostID,
		State:           StateBooting,
		InternalIP:      internalIP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: snapshotPaths.Version,
		WorkloadKey:     req.WorkloadKey,
		GitHubRepo:      req.Repo, // For pool key matching
		Resources:       req.Resources,
		CreatedAt:       time.Now(),
		SocketPath:      filepath.Join(m.config.SocketDir, runnerID+".sock"),
		LogPath:         filepath.Join(m.config.LogDir, runnerID+".log"),
		MetricsPath:     filepath.Join(m.config.LogDir, runnerID+".metrics"),
		RootfsOverlay:   overlayPath,
		RepoCacheUpper:  repoCacheUpperPath,
	}
	if req.StartCommand != nil {
		runner.ServicePort = req.StartCommand.Port
	}

	// Build kernel boot args with network configuration
	// Format: ip=<client-ip>::<gateway-ip>:<netmask>::<interface>:off
	// This configures networking at kernel boot time, before userspace starts
	// See: https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md
	netCfg := tap.GetNetworkConfig()
	// Extract IP without CIDR suffix (netCfg.IP is "172.16.0.2/24", we need "172.16.0.2")
	guestIP := strings.Split(netCfg.IP, "/")[0]
	gateway := netCfg.Gateway // e.g., "172.16.0.1"
	netmask := netCfg.Netmask // e.g., "255.255.255.0"
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=%s::%s:%s::eth0:off", guestIP, gateway, netmask)

	m.logger.WithFields(logrus.Fields{
		"guest_ip": guestIP,
		"gateway":  gateway,
		"netmask":  netmask,
	}).Debug("Configuring kernel network boot args")

	// Create VM configuration — resolve extension drives dynamically from snapshot
	extensionPaths := make(map[string]string)
	for driveID, path := range snapshotPaths.ExtensionDriveImages {
		extensionPaths[driveID] = path
	}
	if repoCacheUpperPath != "" {
		extensionPaths["repo_cache_upper"] = repoCacheUpperPath
	}
	vmCfg := firecracker.VMConfig{
		VMID:           runnerID,
		SocketDir:      m.config.SocketDir,
		FirecrackerBin: m.config.FirecrackerBin,
		KernelPath:     snapshotPaths.Kernel,
		RootfsPath:     overlayPath,
		BootArgs:       bootArgs,
		VCPUs:          runner.Resources.VCPUs,
		MemoryMB:       runner.Resources.MemoryMB,
		NetworkIface: &firecracker.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: tap.Name,
			GuestMAC:    tap.MAC,
		},
		MMDSConfig: &firecracker.MMDSConfig{
			Version:           "V1", // V1 for simple GET requests (thaw-agent uses V1 protocol)
			NetworkInterfaces: []string{"eth0"},
		},
		Drives:      m.buildDrives(extensionPaths),
		LogPath:     runner.LogPath,
		MetricsPath: runner.MetricsPath,
	}

	// When using per-VM namespaces, Firecracker must be launched inside the
	// namespace so it can open tap-slot-0 (which exists only in that namespace).
	if useNetNS && nsInfo != nil {
		vmCfg.NetNSPath = nsInfo.GetFirecrackerNetNSPath()
	}

	// Create VM instance
	vm, err := firecracker.NewVM(vmCfg, m.logger.Logger)
	if err != nil {
		m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// Try snapshot restore first (fast path), fall back to cold boot if needed
	if snapshotPaths.Mem != "" && snapshotPaths.State != "" {
		m.logger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"snapshot":  snapshotPaths.Version,
			"mem":       snapshotPaths.Mem,
			"state":     snapshotPaths.State,
		}).Info("Restoring runner from snapshot (fast path)")

		// Setup symlinks and TAP rename for snapshot restore.
		// AllocateRunner holds m.mu, which serializes these shared-resource operations.
		//
		// Drives: The snapshot bakes in drive paths at /tmp/snapshot/*.img.
		// Symlinks redirect these to actual host paths.
		//
		// TAP: The snapshot bakes in host_dev_name="tap-slot-0". If this runner
		// uses a different slot, we temporarily rename its TAP to tap-slot-0
		// for LoadSnapshot, then rename it back. Running VMs are unaffected
		// because they hold TAP FDs (kernel tracks by ifindex, not name).
		restoreOK := false
		cleanup, symlinkErr := m.setupSnapshotSymlinks(overlayPath, extensionPaths)
		if symlinkErr != nil {
			m.logger.WithError(symlinkErr).Warn("Failed to setup snapshot symlinks, falling back to cold boot")
		} else if useNetNS {
			// With per-VM namespaces, tap-slot-0 already exists in the namespace
			// with the right name — no TAP rename dance needed.
			restoreErr := vm.RestoreFromSnapshot(ctx, snapshotPaths.State, snapshotPaths.Mem, true)
			cleanup()
			if restoreErr != nil {
				m.logger.WithError(restoreErr).Warn("Snapshot restore failed, falling back to cold boot")
			} else {
				restoreOK = true
			}
		} else {
			tapRestore, tapErr := m.setupSnapshotTAPRename(tap.Name)
			if tapErr != nil {
				cleanup()
				m.logger.WithError(tapErr).Warn("Failed to setup TAP rename for snapshot, falling back to cold boot")
			} else {
				restoreErr := vm.RestoreFromSnapshot(ctx, snapshotPaths.State, snapshotPaths.Mem, true)
				tapRestore()
				cleanup()
				if restoreErr != nil {
					m.logger.WithError(restoreErr).Warn("Snapshot restore failed, falling back to cold boot")
				} else {
					restoreOK = true
				}
			}
		}

		if !restoreOK {
			// Stop the failed Firecracker process before retry
			vm.Stop()
			// Recreate VM for cold boot
			vm, err = firecracker.NewVM(vmCfg, m.logger.Logger)
			if err != nil {
				m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
				return nil, fmt.Errorf("failed to recreate VM for cold boot: %w", err)
			}
			if err := vm.Start(ctx); err != nil {
				m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
				return nil, fmt.Errorf("failed to start VM (cold boot fallback): %w", err)
			}
		}
	} else {
		// No snapshot available, cold boot
		m.logger.WithField("runner_id", runnerID).Info("Starting runner (cold boot - no snapshot available)")
		if err := vm.Start(ctx); err != nil {
			m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
			return nil, fmt.Errorf("failed to start VM: %w", err)
		}
	}

	// Inject MMDS data (VM is already running after Start() or RestoreFromSnapshot())
	mmdsData := m.buildMMDSData(ctx, runner, tap, req)
	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		vm.Stop()
		m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
		return nil, fmt.Errorf("failed to set MMDS data: %w", err)
	}

	// When using per-VM namespaces, set up port forwarding (DNAT) so the host
	// can reach services inside the VM via the host-reachable veth IP.
	// Port 10500 is the thaw-agent health/warmup server.
	// Port 10501 is the thaw-agent debug server (health checks, /exec endpoint).
	if useNetNS && m.netnsNetwork != nil {
		if err := m.netnsNetwork.ForwardPort(runnerID, snapshot.ThawAgentHealthPort); err != nil {
			m.logger.WithError(err).WithField("port", snapshot.ThawAgentHealthPort).Warn("Failed to forward port into namespace")
		}
		if err := m.netnsNetwork.ForwardPort(runnerID, snapshot.ThawAgentDebugPort); err != nil {
			m.logger.WithError(err).WithField("port", snapshot.ThawAgentDebugPort).Warn("Failed to forward port into namespace")
		}
		// Forward user service port if start_command is configured
		if req.StartCommand != nil && req.StartCommand.Port > 0 {
			if err := m.netnsNetwork.ForwardPort(runnerID, req.StartCommand.Port); err != nil {
				m.logger.WithError(err).WithField("port", req.StartCommand.Port).Warn("Failed to forward service port into namespace")
			}
		}
	}

	runner.State = StateInitializing
	runner.StartedAt = time.Now()

	m.runners[runnerID] = runner
	m.vms[runnerID] = vm

	// Track for idempotency
	if req.RequestID != "" {
		m.recentRequests[req.RequestID] = &recentAllocation{
			runner:    runner,
			allocTime: time.Now(),
		}
	}

	m.logger.WithFields(logrus.Fields{
		"runner_id":         runnerID,
		"ip":                runner.InternalIP.String(),
		"snapshot":          runner.SnapshotVersion,
		"alloc_ms":          time.Since(allocStart).Milliseconds(),
		"overlay_ms":        overlayDur.Milliseconds(),
		"repo_cache_img_ms": repoCacheDur.Milliseconds(),
	}).Info("Runner allocated successfully")

	return runner, nil
}

// buildMMDSData builds the MMDS data structure for a runner
func (m *Manager) buildMMDSData(ctx context.Context, runner *Runner, tap *network.TapDevice, req AllocateRequest) MMDSData {
	netCfg := tap.GetNetworkConfig()

	var data MMDSData
	data.Latest.Meta.RunnerID = runner.ID
	data.Latest.Meta.HostID = m.config.HostID
	data.Latest.Meta.InstanceName = m.config.InstanceName
	data.Latest.Meta.Environment = m.config.Environment
	data.Latest.Meta.JobID = req.RequestID
	data.Latest.Meta.CurrentTime = time.Now().UTC().Format(time.RFC3339)
	data.Latest.Buildbarn.CertsMountPath = m.config.Bazel.BuildbarnCertsMountPath
	data.Latest.Network.IP = netCfg.IP
	data.Latest.Network.Gateway = netCfg.Gateway
	data.Latest.Network.Netmask = netCfg.Netmask
	data.Latest.Network.DNS = netCfg.DNS
	data.Latest.Network.Interface = netCfg.Interface
	data.Latest.Network.MAC = netCfg.MAC
	data.Latest.Job.Repo = req.Repo
	data.Latest.Job.Branch = req.Branch
	data.Latest.Job.Commit = req.Commit
	data.Latest.Job.GitHubRunnerToken = req.GitHubRunnerToken
	data.Latest.Job.Labels = req.Labels
	data.Latest.Snapshot.Version = runner.SnapshotVersion

	// Get CI runner token if adapter is configured and no token in request
	if m.ciAdapter != nil && req.GitHubRunnerToken == "" {
		token, err := m.ciAdapter.GetRunnerToken(ctx, ci.RunnerTokenOpts{})
		if err != nil {
			m.logger.WithError(err).Warn("Failed to get CI runner registration token")
		} else if token != "" {
			data.Latest.Job.GitHubRunnerToken = token
			runnerURL := m.ciAdapter.RunnerURL()
			if runnerURL != "" {
				data.Latest.Job.Repo = runnerURL
			}
			if len(m.config.CI.GitHubRunnerLabels) > 0 {
				labels := make(map[string]string)
				for _, label := range m.config.CI.GitHubRunnerLabels {
					labels[label] = "true"
				}
				data.Latest.Job.Labels = labels
			}
			m.logger.WithField("runner_id", runner.ID).Info("Got CI runner registration token")
		}
	}

	// Git cache configuration
	if m.config.Bazel.GitCacheEnabled && m.gitCacheImage != "" {
		data.Latest.GitCache.Enabled = true
		data.Latest.GitCache.MountPath = m.config.Bazel.GitCacheMountPath
		data.Latest.GitCache.RepoMappings = m.config.Bazel.GitCacheRepoMappings

		// Ensure Job.Repo is set for git-cache workspace setup
		// This is needed even if GitHub runner registration fails
		if data.Latest.Job.Repo == "" && m.config.CI.GitHubRepo != "" {
			data.Latest.Job.Repo = m.config.CI.GitHubRepo
		}

		// Set pre-cloned path (where repo was cloned during warmup, baked into snapshot)
		// This allows thaw-agent to create symlinks from workspace to pre-cloned repo
		if m.config.Bazel.GitCachePreClonedPath != "" {
			data.Latest.GitCache.PreClonedPath = m.config.Bazel.GitCachePreClonedPath
		}
		// Note: if PreClonedPath is not set, thaw-agent will derive it from job.repo
	}

	// Always set WorkspaceDir - needed for pre-cloned repo symlink even without git-cache
	if m.config.Bazel.GitCacheWorkspaceDir != "" {
		data.Latest.GitCache.WorkspaceDir = m.config.Bazel.GitCacheWorkspaceDir
	}

	// Runner configuration
	data.Latest.Runner.Ephemeral = m.config.CI.GitHubRunnerEphemeral
	if m.ciAdapter != nil {
		data.Latest.Runner.CISystem = m.ciAdapter.Name()
	}

	// Set exec mode when explicitly requested via ci_system=none
	if req.CISystem == "none" {
		data.Latest.Meta.Mode = "exec"
		data.Latest.Runner.CISystem = "none"
	}

	// Populate start_command for user service startup
	if req.StartCommand != nil {
		data.Latest.StartCommand.Command = req.StartCommand.Command
		data.Latest.StartCommand.Port = req.StartCommand.Port
		data.Latest.StartCommand.HealthPath = req.StartCommand.HealthPath
		data.Latest.StartCommand.Env = req.StartCommand.Env
	}

	return data
}

// ReleaseRunner releases a runner
func (m *Manager) ReleaseRunner(runnerID string, destroy bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	runner, exists := m.runners[runnerID]
	if !exists {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	if runner.State == StateQuarantined {
		if destroy {
			return fmt.Errorf("runner %s is quarantined; unquarantine before destroying", runnerID)
		}
		return nil
	}

	m.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"destroy":   destroy,
	}).Info("Releasing runner")

	vm, exists := m.vms[runnerID]
	if exists {
		vm.Stop()
		delete(m.vms, runnerID)
	}

	// Stop UFFD handler if one exists
	if handler, ok := m.uffdHandlers[runnerID]; ok {
		handler.Stop()
		delete(m.uffdHandlers, runnerID)
	}

	// Remove policy enforcer if one exists
	if enforcer, ok := m.policyEnforcers[runnerID]; ok {
		enforcer.Remove()
		delete(m.policyEnforcers, runnerID)
	}

	// Suspended runners already had network released during pause; only clean up overlay/files
	if runner.State == StateSuspended {
		if runner.RootfsOverlay != "" {
			os.Remove(runner.RootfsOverlay)
		}
		if runner.RepoCacheUpper != "" {
			os.Remove(runner.RepoCacheUpper)
		}
		os.Remove(filepath.Join(m.config.SocketDir, runnerID+".sock"))
	} else {
		m.cleanupRunner(runnerID, runner.TapDevice, runner.RootfsOverlay, runner.RepoCacheUpper)
	}

	// Clean up session snapshot files if this runner had a session
	if runner.SessionID != "" {
		sessionDir := filepath.Join(m.sessionBaseDir(), runner.SessionID)
		if err := os.RemoveAll(sessionDir); err != nil {
			m.logger.WithError(err).WithField("session_id", runner.SessionID).Warn("Failed to clean up session dir")
		}
	}

	delete(m.runners, runnerID)

	return nil
}

type quarantineManifest struct {
	RunnerID             string    `json:"runner_id"`
	HostID               string    `json:"host_id"`
	QuarantinedAt        time.Time `json:"quarantined_at"`
	Reason               string    `json:"reason,omitempty"`
	PreQuarantineState   State     `json:"pre_quarantine_state"`
	InternalIP           string    `json:"internal_ip"`
	TapDevice            string    `json:"tap_device"`
	SocketPath           string    `json:"socket_path"`
	LogPath              string    `json:"log_path"`
	MetricsPath          string    `json:"metrics_path"`
	RootfsOverlay        string    `json:"rootfs_overlay"`
	RepoCacheUpper       string    `json:"repo_cache_upper"`
	SnapshotVersion      string    `json:"snapshot_version"`
	BlockEgressRequested bool      `json:"block_egress_requested"`
	PauseVMRequested     bool      `json:"pause_vm_requested"`
	EgressBlocked        bool      `json:"egress_blocked"`
	Paused               bool      `json:"paused"`
}

func (m *Manager) QuarantineRunner(ctx context.Context, runnerID string, opts QuarantineOptions) (string, error) {
	blockEgress := true
	if opts.BlockEgress != nil {
		blockEgress = *opts.BlockEgress
	}
	pauseVM := true
	if opts.PauseVM != nil {
		pauseVM = *opts.PauseVM
	}

	m.mu.Lock()
	r, ok := m.runners[runnerID]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("runner not found: %s", runnerID)
	}
	if r.State == StateQuarantined {
		dir := r.QuarantineDir
		m.mu.Unlock()
		return dir, nil
	}
	vm := m.vms[runnerID]
	prevState := r.State
	now := time.Now()
	quarantineDir := filepath.Join(m.config.QuarantineDir, runnerID)
	r.PreQuarantineState = prevState
	r.State = StateQuarantined
	r.QuarantineReason = opts.Reason
	r.QuarantinedAt = now
	r.QuarantineDir = quarantineDir
	ip := append([]byte(nil), r.InternalIP...)
	tapName := r.TapDevice
	socketPath := r.SocketPath
	logPath := r.LogPath
	metricsPath := r.MetricsPath
	rootfsOverlay := r.RootfsOverlay
	repoCacheUpper := r.RepoCacheUpper
	snapshotVersion := r.SnapshotVersion
	m.mu.Unlock()

	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create quarantine dir: %w", err)
	}

	_ = os.Symlink(logPath, filepath.Join(quarantineDir, "runner.log"))
	_ = os.Symlink(metricsPath, filepath.Join(quarantineDir, "runner.metrics"))
	_ = os.Symlink(rootfsOverlay, filepath.Join(quarantineDir, "rootfs-overlay.img"))
	_ = os.Symlink(repoCacheUpper, filepath.Join(quarantineDir, "repo-cache-upper.img"))

	var errs []error
	egressBlocked := false
	if blockEgress {
		var blockErr error
		if m.netnsNetwork != nil {
			// Per-VM namespace mode: block by veth interface name
			blockErr = m.netnsNetwork.EmergencyBlockEgress(runnerID)
		} else {
			// Legacy bridge mode: block by VM IP
			blockErr = m.network.BlockEgress(net.IP(ip))
		}
		if blockErr != nil {
			errs = append(errs, fmt.Errorf("block egress: %w", blockErr))
		} else {
			egressBlocked = true
		}
	}

	paused := false
	if pauseVM {
		if vm == nil {
			errs = append(errs, fmt.Errorf("pause vm: VM not found"))
		} else if err := vm.Pause(ctx); err != nil {
			errs = append(errs, fmt.Errorf("pause vm: %w", err))
		} else {
			paused = true
		}
	}

	manifest := quarantineManifest{
		RunnerID:             runnerID,
		HostID:               m.config.HostID,
		QuarantinedAt:        now,
		Reason:               opts.Reason,
		PreQuarantineState:   prevState,
		InternalIP:           net.IP(ip).String(),
		TapDevice:            tapName,
		SocketPath:           socketPath,
		LogPath:              logPath,
		MetricsPath:          metricsPath,
		RootfsOverlay:        rootfsOverlay,
		RepoCacheUpper:       repoCacheUpper,
		SnapshotVersion:      snapshotVersion,
		BlockEgressRequested: blockEgress,
		PauseVMRequested:     pauseVM,
		EgressBlocked:        egressBlocked,
		Paused:               paused,
	}
	_ = writeJSON(filepath.Join(quarantineDir, "manifest.json"), manifest)

	m.mu.Lock()
	if rr, ok := m.runners[runnerID]; ok {
		rr.QuarantineEgressBlocked = egressBlocked
		rr.QuarantinePaused = paused
	}
	m.mu.Unlock()

	if len(errs) > 0 {
		return quarantineDir, joinErrors(errs)
	}
	return quarantineDir, nil
}

func (m *Manager) UnquarantineRunner(ctx context.Context, runnerID string, opts UnquarantineOptions) error {
	unblockEgress := true
	if opts.UnblockEgress != nil {
		unblockEgress = *opts.UnblockEgress
	}
	resumeVM := true
	if opts.ResumeVM != nil {
		resumeVM = *opts.ResumeVM
	}

	m.mu.Lock()
	r, ok := m.runners[runnerID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("runner not found: %s", runnerID)
	}
	if r.State != StateQuarantined {
		m.mu.Unlock()
		return fmt.Errorf("runner %s is not quarantined", runnerID)
	}
	vm := m.vms[runnerID]
	ip := append([]byte(nil), r.InternalIP...)
	prevState := r.PreQuarantineState
	egressWasBlocked := r.QuarantineEgressBlocked
	wasPaused := r.QuarantinePaused
	quarantineDir := r.QuarantineDir
	m.mu.Unlock()

	var errs []error
	unblocked := false
	if unblockEgress && egressWasBlocked {
		var unblockErr error
		if m.netnsNetwork != nil {
			unblockErr = m.netnsNetwork.EmergencyUnblockEgress(runnerID)
		} else {
			unblockErr = m.network.UnblockEgress(net.IP(ip))
		}
		if unblockErr != nil {
			errs = append(errs, fmt.Errorf("unblock egress: %w", unblockErr))
		} else {
			unblocked = true
		}
	}

	resumed := false
	if resumeVM && wasPaused {
		if vm == nil {
			errs = append(errs, fmt.Errorf("resume vm: VM not found"))
		} else if err := vm.Resume(ctx); err != nil {
			errs = append(errs, fmt.Errorf("resume vm: %w", err))
		} else {
			resumed = true
		}
	}

	m.mu.Lock()
	if rr, ok := m.runners[runnerID]; ok {
		if unblocked {
			rr.QuarantineEgressBlocked = false
		}
		if resumed {
			rr.QuarantinePaused = false
		}
		if len(errs) == 0 {
			if prevState == "" {
				prevState = StateIdle
			}
			rr.State = prevState
		}
	}
	m.mu.Unlock()

	if quarantineDir != "" {
		_ = writeJSON(filepath.Join(quarantineDir, "unquarantine.json"), map[string]any{
			"runner_id":        runnerID,
			"unquarantined_at": time.Now(),
			"unblock_egress":   unblockEgress,
			"resume_vm":        resumeVM,
			"errors":           errorsToStrings(errs),
		})
	}

	if len(errs) > 0 {
		return joinErrors(errs)
	}
	return nil
}

// ApplyNetworkPolicy resolves and applies a network policy for a runner.
// Called during allocation after the namespace is created.
func (m *Manager) ApplyNetworkPolicy(runnerID string, req AllocateRequest) error {
	policy := network.ResolvePolicy(req.NetworkPolicyPreset, req.NetworkPolicy)
	if policy == nil {
		return nil // unrestricted, no enforcement needed
	}

	if err := policy.Validate(); err != nil {
		return fmt.Errorf("invalid network policy: %w", err)
	}

	m.mu.RLock()
	r, ok := m.runners[runnerID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	if m.netnsNetwork == nil {
		m.logger.WithField("runner_id", runnerID).Warn("Network policy requested but netns mode not enabled; skipping enforcement")
		// Store policy on runner for observability even without enforcement
		m.mu.Lock()
		r.NetworkPolicy = policy
		r.NetworkPolicyVersion = 1
		m.mu.Unlock()
		return nil
	}

	ns, err := m.netnsNetwork.GetNamespace(runnerID)
	if err != nil {
		return fmt.Errorf("get namespace for policy: %w", err)
	}

	enforcer := network.NewPolicyEnforcer(network.PolicyEnforcerConfig{
		VMID:       runnerID,
		NSName:     ns.Name,
		VethVM:     ns.VethVM,
		HostVethIP: net.IPv4(10, 200, byte(ns.Slot), 1),
		Policy:     policy,
		Logger:     m.logger.Logger,
	})

	// Set initial ingress ports
	var ports []int
	if r.ServicePort > 0 {
		ports = append(ports, r.ServicePort)
	}
	ports = append(ports, 10500, 10501) // thaw-agent health + debug
	enforcer.SetInitialIngressPorts(ports)

	if err := enforcer.Apply(); err != nil {
		return fmt.Errorf("apply network policy: %w", err)
	}

	// Start DNS proxy if needed
	if err := enforcer.StartDNSProxy(func(fn func() error) error {
		return m.netnsNetwork.RunInNamespace(runnerID, fn)
	}); err != nil {
		enforcer.Remove()
		return fmt.Errorf("start dns proxy: %w", err)
	}

	m.mu.Lock()
	r.NetworkPolicy = policy
	r.NetworkPolicyVersion = 1
	m.policyEnforcers[runnerID] = enforcer
	m.mu.Unlock()

	m.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"policy":    policy.Name,
	}).Info("Network policy applied")

	return nil
}

// UpdateNetworkPolicy updates the network policy for a running VM.
func (m *Manager) UpdateNetworkPolicy(runnerID string, newPolicy *network.NetworkPolicy) error {
	if newPolicy == nil {
		return fmt.Errorf("policy cannot be nil for update")
	}
	if err := newPolicy.Validate(); err != nil {
		return fmt.Errorf("invalid network policy: %w", err)
	}

	m.mu.Lock()
	r, ok := m.runners[runnerID]
	enforcer := m.policyEnforcers[runnerID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	if enforcer != nil {
		// Existing enforcer: update in place
		if err := enforcer.Update(newPolicy); err != nil {
			return fmt.Errorf("update policy: %w", err)
		}
	} else if m.netnsNetwork != nil {
		// No enforcer yet but netns available: create and apply
		req := AllocateRequest{NetworkPolicy: newPolicy}
		if err := m.ApplyNetworkPolicy(runnerID, req); err != nil {
			return fmt.Errorf("apply policy on update: %w", err)
		}
		return nil // ApplyNetworkPolicy already sets version to 1
	}

	// Store policy on runner (either enforcer updated or no netns)
	m.mu.Lock()
	r.NetworkPolicy = newPolicy
	r.NetworkPolicyVersion++
	m.mu.Unlock()

	return nil
}

// GetNetworkPolicy returns the effective policy for a runner.
func (m *Manager) GetNetworkPolicy(runnerID string) (*network.NetworkPolicy, int, error) {
	m.mu.RLock()
	r, ok := m.runners[runnerID]
	m.mu.RUnlock()
	if !ok {
		return nil, 0, fmt.Errorf("runner not found: %s", runnerID)
	}
	return r.NetworkPolicy, r.NetworkPolicyVersion, nil
}

// EmergencyBlockEgress blocks all egress for a VM at the host level (independent of namespace policy).
func (m *Manager) EmergencyBlockEgress(runnerID string) error {
	if m.netnsNetwork == nil {
		return fmt.Errorf("netns mode not enabled")
	}
	if err := m.netnsNetwork.EmergencyBlockEgress(runnerID); err != nil {
		return err
	}
	m.mu.Lock()
	if r, ok := m.runners[runnerID]; ok {
		r.EmergencyEgressBlocked = true
	}
	m.mu.Unlock()
	return nil
}

// EmergencyUnblockEgress removes the host-level egress block.
func (m *Manager) EmergencyUnblockEgress(runnerID string) error {
	if m.netnsNetwork == nil {
		return fmt.Errorf("netns mode not enabled")
	}
	if err := m.netnsNetwork.EmergencyUnblockEgress(runnerID); err != nil {
		return err
	}
	m.mu.Lock()
	if r, ok := m.runners[runnerID]; ok {
		r.EmergencyEgressBlocked = false
	}
	m.mu.Unlock()
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func joinErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	msg := "errors:"
	for _, err := range errs {
		msg += " " + err.Error() + ";"
	}
	return fmt.Errorf("%s", msg)
}

func errorsToStrings(errs []error) []string {
	if len(errs) == 0 {
		return nil
	}
	out := make([]string, 0, len(errs))
	for _, err := range errs {
		out = append(out, err.Error())
	}
	return out
}

// cleanupRunner cleans up runner resources
func (m *Manager) cleanupRunner(runnerID, tapDevice, overlayPath, repoCacheUpperPath string) {
	if m.netnsNetwork != nil {
		// Per-VM namespace mode: release the entire namespace
		m.netnsNetwork.ReleaseNamespace(runnerID)
		if slot, ok := m.runnerToSlot[runnerID]; ok {
			delete(m.slotToRunner, slot)
			delete(m.runnerToSlot, runnerID)
		}
	} else if slot, ok := m.runnerToSlot[runnerID]; ok {
		// Legacy: release TAP slot
		m.network.ReleaseTapSlot(slot, runnerID)
		delete(m.slotToRunner, slot)
		delete(m.runnerToSlot, runnerID)
	} else {
		// Legacy: release dynamic TAP device (cold boot path)
		m.network.ReleaseTap(runnerID)
	}

	// Remove overlay
	if overlayPath != "" {
		os.Remove(overlayPath)
	}

	// Remove repo cache upper image
	if repoCacheUpperPath != "" {
		os.Remove(repoCacheUpperPath)
	}

	// Remove socket
	socketPath := filepath.Join(m.config.SocketDir, runnerID+".sock")
	os.Remove(socketPath)
}

// findAvailableSlot finds the first available TAP slot for snapshot restore.
// Returns -1 if no slots are available.
func (m *Manager) findAvailableSlot() int {
	for i := 0; i < m.config.MaxRunners; i++ {
		if _, inUse := m.slotToRunner[i]; !inUse {
			return i
		}
	}
	return -1
}

// buildDrives constructs the list of block devices to attach to a microVM.
// extensionDrivePaths maps driveID to host path for extension drives.
// The credentials drive is always included.
func (m *Manager) buildDrives(extensionDrivePaths map[string]string) []firecracker.Drive {
	drives := []firecracker.Drive{
		{
			DriveID:      "credentials",
			PathOnHost:   m.credentialsImage,
			IsRootDevice: false,
			IsReadOnly:   true,
		},
	}

	// Append extension drives in deterministic order (sorted by driveID).
	driveIDs := make([]string, 0, len(extensionDrivePaths))
	for id := range extensionDrivePaths {
		driveIDs = append(driveIDs, id)
	}
	sort.Strings(driveIDs)
	for _, id := range driveIDs {
		drives = append(drives, firecracker.Drive{
			DriveID:      id,
			PathOnHost:   extensionDrivePaths[id],
			IsRootDevice: false,
			IsReadOnly:   false,
		})
	}

	return drives
}

// snapshotSymlinkDir is the directory where symlinks are created to match the
// snapshot-builder's output paths baked into the snapshot state file.
// Must match snapshot-builder's --output-dir flag (default: /tmp/snapshot).
const snapshotSymlinkDir = "/tmp/snapshot"

// snapshotTAPName is the TAP device name baked into the snapshot state file.
// Must match the TAP name used by the snapshot-builder (always "tap-slot-0").
const snapshotTAPName = "tap-slot-0"

// setupSnapshotSymlinks creates symlinks from the snapshot's baked-in drive paths
// to the actual host paths. Firecracker validates (opens) drive backing files during
// LoadSnapshot at the paths recorded in the snapshot state. Since the snapshot was
// built with drives at /tmp/snapshot/*.img but the host has them at different locations,
// symlinks bridge the gap.
//
// This function must be called while m.mu is held (AllocateRunner holds it),
// which serializes access to the shared /tmp/snapshot/ directory.
//
// Returns a cleanup function that removes the symlinks. The cleanup should be
// called after LoadSnapshot returns, as Firecracker holds open file descriptors
// and no longer needs the symlink paths.
func (m *Manager) setupSnapshotSymlinks(overlayPath string, extensionDrivePaths map[string]string) (func(), error) {
	if err := os.MkdirAll(snapshotSymlinkDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot symlink dir: %w", err)
	}

	// Map snapshot filenames to actual host paths.
	// These filenames must match what snapshot-builder creates in its output dir.
	symlinks := []struct {
		name   string // filename in /tmp/snapshot/
		target string // actual path on host
	}{
		{"rootfs.img", overlayPath},
		{"credentials.img", m.credentialsImage},
	}
	// Add extension drives by driveID (e.g. "repo_cache_seed" → "repo-cache-seed.img")
	for driveID, path := range extensionDrivePaths {
		name := strings.ReplaceAll(driveID, "_", "-") + ".img"
		symlinks = append(symlinks, struct{ name, target string }{name, path})
	}

	var created []string
	for _, s := range symlinks {
		if s.target == "" {
			continue
		}
		linkPath := filepath.Join(snapshotSymlinkDir, s.name)
		// Remove any existing file/symlink at the path
		os.Remove(linkPath)
		if err := os.Symlink(s.target, linkPath); err != nil {
			// Cleanup on failure
			for _, c := range created {
				os.Remove(c)
			}
			return nil, fmt.Errorf("symlink %s -> %s: %w", linkPath, s.target, err)
		}
		created = append(created, linkPath)
		m.logger.WithFields(logrus.Fields{
			"link":   linkPath,
			"target": s.target,
		}).Debug("Created snapshot symlink")
	}

	cleanup := func() {
		for _, c := range created {
			os.Remove(c)
		}
	}
	return cleanup, nil
}

// setupSnapshotTAPRename temporarily renames TAP devices so that this runner's
// TAP has the name baked into the snapshot (tap-slot-0). Firecracker opens the
// TAP device by name during LoadSnapshot via TUNSETIFF, so the name must match.
//
// If tap-slot-0 is already held by another runner's Firecracker process, we
// rename it to a temporary name first, then restore it after. Firecracker holds
// TAP devices by file descriptor, so renames are transparent to running VMs.
// Bridge membership (fcbr0) is also preserved since the kernel tracks ports
// by ifindex, not by name.
//
// This function must be called while m.mu is held.
// Returns a restore function that undoes the renames (call after LoadSnapshot).
func (m *Manager) setupSnapshotTAPRename(currentTAP string) (func(), error) {
	if currentTAP == snapshotTAPName {
		// Already has the right name (slot 0), no rename needed
		return func() {}, nil
	}

	// Check if tap-slot-0 already exists (held by another runner)
	_, err := net.InterfaceByName(snapshotTAPName)
	existingTAPInUse := err == nil

	tempName := snapshotTAPName + "-tmp"

	if existingTAPInUse {
		// Rename existing tap-slot-0 → tap-slot-0-tmp
		m.logger.WithFields(logrus.Fields{
			"from": snapshotTAPName,
			"to":   tempName,
		}).Debug("Temporarily renaming existing TAP for snapshot restore")
		if output, err := exec.Command("ip", "link", "set", snapshotTAPName, "name", tempName).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("rename %s → %s: %s: %w", snapshotTAPName, tempName, strings.TrimSpace(string(output)), err)
		}
	}

	// Rename our TAP → tap-slot-0
	m.logger.WithFields(logrus.Fields{
		"from": currentTAP,
		"to":   snapshotTAPName,
	}).Debug("Renaming TAP for snapshot restore")
	if output, err := exec.Command("ip", "link", "set", currentTAP, "name", snapshotTAPName).CombinedOutput(); err != nil {
		// Undo the temp rename if we did it
		if existingTAPInUse {
			exec.Command("ip", "link", "set", tempName, "name", snapshotTAPName).Run()
		}
		return nil, fmt.Errorf("rename %s → %s: %s: %w", currentTAP, snapshotTAPName, strings.TrimSpace(string(output)), err)
	}

	restore := func() {
		// Rename tap-slot-0 back to our real name
		if output, err := exec.Command("ip", "link", "set", snapshotTAPName, "name", currentTAP).CombinedOutput(); err != nil {
			m.logger.WithError(err).WithField("output", strings.TrimSpace(string(output))).Warn("Failed to restore TAP name after snapshot")
		}

		// Restore original tap-slot-0 if we moved it
		if existingTAPInUse {
			if output, err := exec.Command("ip", "link", "set", tempName, "name", snapshotTAPName).CombinedOutput(); err != nil {
				m.logger.WithError(err).WithField("output", strings.TrimSpace(string(output))).Warn("Failed to restore original tap-slot-0 name")
			}
		}
	}

	return restore, nil
}

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

func ensureCredentialsImage(cfg HostConfig, log *logrus.Entry) (string, error) {
	sharedDir := filepath.Join(cfg.WorkspaceDir, "_shared")
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create shared dir: %w", err)
	}

	imgPath := filepath.Join(sharedDir, "credentials.img")
	sizeMB := cfg.Bazel.BuildbarnCertsImageSizeMB
	if sizeMB <= 0 {
		sizeMB = 32
	}

	seedDir := cfg.Bazel.BuildbarnCertsDir
	if seedDir == "" {
		if _, err := os.Stat(imgPath); err == nil {
			return imgPath, nil
		}
		if err := createExt4ImageMB(imgPath, sizeMB, "CREDENTIALS"); err != nil {
			return "", err
		}
		_ = os.Chmod(imgPath, 0600)
		return imgPath, nil
	}

	if err := createExt4ImageMB(imgPath, sizeMB, "CREDENTIALS"); err != nil {
		return "", err
	}
	if err := seedExt4ImageFromDir(imgPath, seedDir); err != nil {
		if log != nil {
			log.WithError(err).WithFields(logrus.Fields{
				"seed_dir": seedDir,
				"image":    imgPath,
			}).Warn("Failed to seed credentials image; continuing with empty image")
		}
	}
	_ = os.Chmod(imgPath, 0600)
	return imgPath, nil
}

func createExt4ImageMB(path string, sizeMB int, label string) error {
	if sizeMB <= 0 {
		return fmt.Errorf("invalid sizeMB: %d", sizeMB)
	}
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dM", sizeMB), path).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", label, "-E", "lazy_itable_init=1,lazy_journal_init=1", path).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", string(output), err)
	}
	return nil
}

func seedExt4ImageFromDir(imgPath, seedDir string) error {
	info, err := os.Stat(seedDir)
	if err != nil {
		return fmt.Errorf("seed dir stat failed: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("seed dir is not a directory: %s", seedDir)
	}

	mountPoint := filepath.Join(filepath.Dir(imgPath), "mnt-buildbarn-certs")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	if output, err := exec.Command("mount", "-o", "loop", imgPath, mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("mount loop failed: %s: %w", string(output), err)
	}
	defer func() {
		_ = exec.Command("umount", mountPoint).Run()
	}()

	if _, err := exec.LookPath("rsync"); err == nil {
		cmd := exec.Command("rsync", "-a", "--delete", seedDir+string(os.PathSeparator), mountPoint+string(os.PathSeparator))
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("rsync failed: %s: %w", string(output), err)
		}
		return nil
	}

	cmd := exec.Command("cp", "-a", seedDir+string(os.PathSeparator)+".", mountPoint+string(os.PathSeparator))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp -a failed: %s: %w", string(output), err)
	}
	return nil
}

// GetRunner returns a runner by ID
func (m *Manager) GetRunner(runnerID string) (*Runner, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	runner, exists := m.runners[runnerID]
	if !exists {
		return nil, fmt.Errorf("runner not found: %s", runnerID)
	}

	return runner, nil
}

// ListRunners returns all runners, optionally filtered by state
func (m *Manager) ListRunners(stateFilter State) []*Runner {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var runners []*Runner
	for _, r := range m.runners {
		if stateFilter == "" || r.State == stateFilter {
			runners = append(runners, r)
		}
	}

	return runners
}

// SetRunnerState updates a runner's state
func (m *Manager) SetRunnerState(runnerID string, state State) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	runner, exists := m.runners[runnerID]
	if !exists {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	runner.State = state
	return nil
}

// GetStatus returns the current status of the manager
func (m *Manager) GetStatus() ManagerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var idle, busy int
	var usedCPU, usedMem int
	for _, r := range m.runners {
		switch r.State {
		case StateIdle:
			idle++
		case StateBusy:
			busy++
		}
		usedCPU += r.Resources.VCPUs * 1000
		usedMem += r.Resources.MemoryMB
	}

	return ManagerStatus{
		ActiveRunners:      len(m.runners),
		IdleRunners:        idle,
		BusyRunners:        busy,
		SnapshotVersion:    m.snapshotCache.CurrentVersion(),
		Draining:           m.draining,
		TotalCPUMillicores: m.config.TotalCPUMillicores,
		UsedCPUMillicores:  usedCPU,
		TotalMemoryMB:      m.config.TotalMemoryMB,
		UsedMemoryMB:       usedMem,
	}
}

// GetRunnerHeartbeatInfo returns per-runner status summaries for the heartbeat.
func (m *Manager) GetRunnerHeartbeatInfo() []RunnerHeartbeatInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]RunnerHeartbeatInfo, 0, len(m.runners))
	for _, r := range m.runners {
		info := RunnerHeartbeatInfo{
			RunnerID:    r.ID,
			State:       r.State,
			WorkloadKey: r.WorkloadKey,
		}
		if r.State == StateIdle && !r.LastExecAt.IsZero() {
			info.IdleSince = r.LastExecAt.Format(time.RFC3339)
		}
		infos = append(infos, info)
	}
	return infos
}

// ManagerStatus represents the status of the runner manager
type ManagerStatus struct {
	ActiveRunners      int
	IdleRunners        int
	BusyRunners        int
	SnapshotVersion    string
	Draining           bool
	TotalCPUMillicores int
	UsedCPUMillicores  int
	TotalMemoryMB      int
	UsedMemoryMB       int
}

// SyncSnapshot syncs a new snapshot version from GCS
func (m *Manager) SyncSnapshot(ctx context.Context, version string) error {
	m.logger.WithField("version", version).Info("Syncing snapshot")
	return m.snapshotCache.SyncFromGCS(ctx, version)
}

// CanAddRunner checks if a new runner with the given resources can be added.
// It checks both the hard MaxRunners cap and actual host resource capacity.
func (m *Manager) CanAddRunner(vcpus, memoryMB int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.draining || len(m.runners) >= m.config.MaxRunners {
		return false
	}

	// If host doesn't report total resources, fall back to slot check
	if m.config.TotalCPUMillicores == 0 {
		return true
	}

	var usedCPU, usedMem int
	for _, r := range m.runners {
		usedCPU += r.Resources.VCPUs * 1000
		usedMem += r.Resources.MemoryMB
	}

	return (m.config.TotalCPUMillicores-usedCPU) >= vcpus*1000 &&
		(m.config.TotalMemoryMB-usedMem) >= memoryMB
}

// IdleCount returns the number of idle runners
func (m *Manager) IdleCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, r := range m.runners {
		if r.State == StateIdle {
			count++
		}
	}
	return count
}

// DrainIdleRunners stops and removes all idle runners on the host. Busy runners
// are left alone so in-flight jobs can finish.
func (m *Manager) DrainIdleRunners(ctx context.Context) (int, error) {
	ids := m.ListRunners(StateIdle)
	if len(ids) == 0 {
		return 0, nil
	}

	var errs []error
	stopped := 0
	for _, r := range ids {
		if err := m.ReleaseRunner(r.ID, true); err != nil {
			errs = append(errs, err)
			continue
		}
		stopped++
	}
	if len(errs) > 0 {
		return stopped, joinErrors(errs)
	}
	return stopped, nil
}

// PauseSessionRunners pauses all session-bound runners (idle or busy) so their
// state is uploaded to GCS before the host shuts down. This is called during
// graceful drain to ensure sessions can be resumed on another host.
func (m *Manager) PauseSessionRunners(ctx context.Context) (int, error) {
	m.mu.RLock()
	var targets []string
	for _, r := range m.runners {
		if r.SessionID != "" && (r.State == StateIdle || r.State == StateBusy) {
			targets = append(targets, r.ID)
		}
	}
	m.mu.RUnlock()

	if len(targets) == 0 {
		return 0, nil
	}

	var errs []error
	paused := 0
	for _, id := range targets {
		if _, err := m.PauseRunner(ctx, id); err != nil {
			m.logger.WithError(err).WithField("runner_id", id).Warn("Failed to pause session runner during drain")
			errs = append(errs, err)
			continue
		}
		paused++
	}
	if len(errs) > 0 {
		return paused, joinErrors(errs)
	}
	return paused, nil
}

// RemoveRunnerLabels removes custom labels from all runners on this host.
// This is called when entering drain mode to prevent the CI system from scheduling new jobs.
func (m *Manager) RemoveRunnerLabels(ctx context.Context) (int, error) {
	if m.ciAdapter == nil {
		m.logger.Debug("CI adapter not configured, skipping label removal")
		return 0, nil
	}

	m.mu.RLock()
	var runners []ci.RunnerInfo
	for _, r := range m.runners {
		runners = append(runners, ci.RunnerInfo{
			ID:   r.ID,
			Name: r.ID,
			Repo: r.GitHubRepo,
		})
	}
	m.mu.RUnlock()

	if len(runners) == 0 {
		return 0, nil
	}

	m.logger.WithField("runner_count", len(runners)).Info("Draining runners via CI adapter")
	if err := m.ciAdapter.OnDrain(ctx, runners); err != nil {
		return 0, err
	}
	return len(runners), nil
}

// Close shuts down the manager and all runners
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info("Shutting down runner manager")

	// Shutdown runner pool first
	if m.pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := m.pool.Shutdown(ctx); err != nil {
			m.logger.WithError(err).Warn("Failed to shutdown runner pool")
		}
	}

	// Stop all VMs
	for id, vm := range m.vms {
		m.logger.WithField("runner_id", id).Debug("Stopping VM")
		vm.Stop()
	}

	// Close CI adapter
	if m.ciAdapter != nil {
		m.ciAdapter.Close()
	}

	// Cleanup network
	m.network.Cleanup()

	// Close snapshot cache
	m.snapshotCache.Close()

	// Note: sessionMemStore and sessionDiskStore are NOT closed here — they are
	// owned by ChunkedManager which has its own Close() that shuts them down.

	return nil
}

// DiskUsage returns the disk usage percentage of the data directory (0.0 to 1.0).
func (m *Manager) DiskUsage() float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(m.config.WorkspaceDir, &stat); err != nil {
		m.logger.WithError(err).Warn("Failed to get disk usage")
		return 0
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return 0
	}
	return float64(total-free) / float64(total)
}

// CleanupOrphanedWorkspaces removes workspace directories that don't belong to any active runner.
func (m *Manager) CleanupOrphanedWorkspaces() {
	entries, err := os.ReadDir(m.config.WorkspaceDir)
	if err != nil {
		m.logger.WithError(err).Warn("Failed to read workspace directory")
		return
	}

	m.mu.RLock()
	activeRunners := make(map[string]bool)
	for id := range m.runners {
		activeRunners[id] = true
	}
	m.mu.RUnlock()

	cleaned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip special directories
		if name == "_shared" || name == "." || name == ".." {
			continue
		}
		if !activeRunners[name] {
			dirPath := filepath.Join(m.config.WorkspaceDir, name)
			m.logger.WithField("dir", dirPath).Info("Cleaning up orphaned workspace")
			if err := os.RemoveAll(dirPath); err != nil {
				m.logger.WithError(err).WithField("dir", dirPath).Warn("Failed to remove orphaned workspace")
			} else {
				cleaned++
			}
		}
	}
	if cleaned > 0 {
		m.logger.WithField("cleaned", cleaned).Info("Cleaned up orphaned workspaces")
	}
}

// pauseRunnerVM pauses a runner's VM (pool callback)
func (m *Manager) pauseRunnerVM(ctx context.Context, runnerID string) error {
	m.mu.RLock()
	vm := m.vms[runnerID]
	m.mu.RUnlock()

	if vm == nil {
		return fmt.Errorf("VM not found for runner %s", runnerID)
	}
	return vm.Pause(ctx)
}

// resumeRunnerVM resumes a runner's VM (pool callback)
func (m *Manager) resumeRunnerVM(ctx context.Context, runnerID string) error {
	m.mu.RLock()
	vm := m.vms[runnerID]
	m.mu.RUnlock()

	if vm == nil {
		return fmt.Errorf("VM not found for runner %s", runnerID)
	}
	return vm.Resume(ctx)
}

// getRunnerVMStats gets VM resource statistics (pool callback)
func (m *Manager) getRunnerVMStats(ctx context.Context, runnerID string) (*VMStats, error) {
	m.mu.RLock()
	runner := m.runners[runnerID]
	m.mu.RUnlock()

	if runner == nil {
		return nil, fmt.Errorf("runner not found: %s", runnerID)
	}

	// Estimate memory usage from config
	memoryUsage := int64(runner.Resources.MemoryMB) * 1024 * 1024

	// Disk usage would be the overlay size, but for now just estimate
	diskUsage := int64(m.config.Bazel.RepoCacheUpperSizeGB) * 1024 * 1024 * 1024

	return &VMStats{
		MemoryUsageBytes: memoryUsage,
		DiskUsageBytes:   diskUsage,
	}, nil
}

// removeRunnerVM removes a runner completely (pool callback)
func (m *Manager) removeRunnerVM(ctx context.Context, runnerID string) error {
	return m.ReleaseRunner(runnerID, true)
}

// ReconcileOrphans cleans up resources left behind by a previous manager incarnation.
// This includes:
// 1. Orphaned Firecracker sockets (from crashed VMs)
// 2. Orphaned workspace directories
// 3. Orphaned TAP/veth network devices
func (m *Manager) ReconcileOrphans(ctx context.Context) {
	m.logger.Info("Reconciling orphaned resources from previous incarnation")

	orphaned := 0

	// 1. Find orphaned Firecracker sockets
	sockEntries, err := os.ReadDir(m.config.SocketDir)
	if err != nil {
		m.logger.WithError(err).Warn("Failed to read socket directory")
	} else {
		m.mu.RLock()
		for _, entry := range sockEntries {
			if !strings.HasSuffix(entry.Name(), ".sock") {
				continue
			}
			runnerID := strings.TrimSuffix(entry.Name(), ".sock")
			if _, exists := m.runners[runnerID]; !exists {
				sockPath := filepath.Join(m.config.SocketDir, entry.Name())
				m.logger.WithField("socket", sockPath).Info("Removing orphaned socket")
				os.Remove(sockPath)
				orphaned++
			}
		}
		m.mu.RUnlock()
	}

	// 2. Clean up orphaned workspaces
	m.CleanupOrphanedWorkspaces()

	// 3. Clean up orphaned TAP devices
	netEntries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		m.logger.WithError(err).Debug("Failed to read /sys/class/net")
	} else {
		for _, entry := range netEntries {
			name := entry.Name()
			if strings.HasPrefix(name, "veth-") || (strings.HasPrefix(name, "tap-") && name != "tap-slot-0") {
				// Check if this TAP belongs to an active runner
				m.mu.RLock()
				inUse := false
				for _, r := range m.runners {
					if r.TapDevice == name {
						inUse = true
						break
					}
				}
				m.mu.RUnlock()

				if !inUse {
					m.logger.WithField("device", name).Info("Removing orphaned network device")
					exec.Command("ip", "link", "delete", name).Run()
					orphaned++
				}
			}
		}
	}

	if orphaned > 0 {
		m.logger.WithField("orphaned_cleaned", orphaned).Info("Orphan reconciliation complete")
	} else {
		m.logger.Info("No orphaned resources found")
	}
}

// GetPool returns the runner pool (may be nil if pooling disabled)
func (m *Manager) GetPool() *Pool {
	return m.pool
}

// ReleaseRunnerWithOptions releases a runner with more control over behavior
func (m *Manager) ReleaseRunnerWithOptions(ctx context.Context, runnerID string, opts ReleaseOptions) error {
	m.mu.Lock()
	runner, exists := m.runners[runnerID]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	// Don't pool quarantined runners
	if runner.State == StateQuarantined {
		m.mu.Unlock()
		if opts.Destroy {
			return fmt.Errorf("runner %s is quarantined; unquarantine before destroying", runnerID)
		}
		return nil
	}
	m.mu.Unlock()

	// Try to recycle if pooling enabled and requested
	if m.pool != nil && opts.TryRecycle && !opts.Destroy {
		pooled := &pooledRunner{
			Runner: runner,
			key: &RunnerKey{
				SnapshotVersion: runner.SnapshotVersion,
				Platform:        "linux/amd64",
				GitHubRepo:      runner.GitHubRepo,
			},
		}

		if err := m.pool.TryRecycle(ctx, pooled, opts.FinishedCleanly); err == nil {
			m.logger.WithField("runner_id", runnerID).Info("Runner recycled to pool")
			return nil
		} else {
			m.logger.WithError(err).WithField("runner_id", runnerID).Debug("Failed to recycle runner, destroying")
		}
	}

	// Fall back to destroy
	return m.ReleaseRunner(runnerID, true)
}
