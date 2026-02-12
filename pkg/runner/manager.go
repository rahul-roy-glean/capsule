package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/ci"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/firecracker"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// Manager manages the lifecycle of runners on a host
type Manager struct {
	config        HostConfig
	runners       map[string]*Runner
	vms           map[string]*firecracker.VM
	snapshotCache *snapshot.Cache
	network       *network.NATNetwork
	// credentialsImage is an ext4 image containing credentials (e.g. Buildbarn certs),
	// attached read-only to each microVM for Bazel remote cache/execution TLS config.
	credentialsImage string
	// gitCacheImage is an ext4 image containing git repository mirrors, attached
	// read-only to each microVM for fast reference cloning.
	gitCacheImage string
	// ciAdapter provides CI system integration (runner registration, drain, etc.)
	ciAdapter ci.Adapter
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
		LocalPath: cfg.SnapshotCachePath,
		GCSBucket: cfg.SnapshotBucket,
		Logger:    logger,
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
	if cfg.GitCacheEnabled && cfg.GitCacheImagePath != "" {
		if _, err := os.Stat(cfg.GitCacheImagePath); err == nil {
			gitCacheImg = cfg.GitCacheImagePath
			logger.WithField("git_cache_image", gitCacheImg).Info("Git-cache image found")
		} else {
			logger.WithField("git_cache_image", cfg.GitCacheImagePath).Warn("Git-cache enabled but image not found")
		}
	}

	m := &Manager{
		config:           cfg,
		runners:          make(map[string]*Runner),
		vms:              make(map[string]*firecracker.VM),
		snapshotCache:    cache,
		network:          natNet,
		credentialsImage: credentialsImg,
		gitCacheImage:    gitCacheImg,
		ciAdapter:        ciAdapter,
		slotToRunner:     make(map[int]string),
		runnerToSlot:     make(map[string]int),
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

	return m, nil
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
	m.logger.WithField("runner_id", runnerID).Info("Allocating new runner")

	// Get snapshot paths first to determine if we should use slot-based TAP allocation
	snapshotPaths, err := m.snapshotCache.GetSnapshotPaths()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot paths: %w", err)
	}

	// Determine if snapshot restore is available
	useSnapshotRestore := snapshotPaths.Mem != "" && snapshotPaths.State != ""

	// Allocate TAP device
	// For snapshot restore, we MUST use slot-based allocation because Firecracker
	// does not support changing host_dev_name after snapshot load. The snapshot was
	// created with tap-slot-0, so we need to use matching slot names.
	var tap *network.TapDevice
	var slot int = -1
	if useSnapshotRestore {
		// Find an available slot
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
		// Cold boot path - use dynamic TAP allocation
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
	overlayPath, err := m.snapshotCache.CreateOverlay(runnerID)
	if err != nil {
		m.network.ReleaseTap(runnerID)
		return nil, fmt.Errorf("failed to create rootfs overlay: %w", err)
	}

	// Create per-runner writable repo cache layer image (upperdir/workdir lives here)
	repoCacheUpperPath := filepath.Join(m.config.WorkspaceDir, runnerID, "repo-cache-upper.img")
	if err := os.MkdirAll(filepath.Dir(repoCacheUpperPath), 0755); err != nil {
		m.cleanupRunner(runnerID, tap.Name, overlayPath, "")
		return nil, fmt.Errorf("failed to create repo-cache-upper directory: %w", err)
	}
	if err := createExt4Image(repoCacheUpperPath, m.config.RepoCacheUpperSizeGB, "BAZEL_REPO_UPPER"); err != nil {
		m.cleanupRunner(runnerID, tap.Name, overlayPath, repoCacheUpperPath)
		return nil, fmt.Errorf("failed to create repo-cache-upper image: %w", err)
	}

	// Create runner record
	runner := &Runner{
		ID:              runnerID,
		HostID:          m.config.HostID,
		State:           StateBooting,
		InternalIP:      tap.IP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: snapshotPaths.Version,
		GitHubRepo:      req.Repo, // For pool key matching
		Resources: Resources{
			VCPUs:    m.config.VCPUsPerRunner,
			MemoryMB: m.config.MemoryMBPerRunner,
		},
		CreatedAt:      time.Now(),
		SocketPath:     filepath.Join(m.config.SocketDir, runnerID+".sock"),
		LogPath:        filepath.Join(m.config.LogDir, runnerID+".log"),
		MetricsPath:    filepath.Join(m.config.LogDir, runnerID+".metrics"),
		RootfsOverlay:  overlayPath,
		RepoCacheUpper: repoCacheUpperPath,
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

	// Create VM configuration
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
		Drives:      m.buildDrives(snapshotPaths.RepoCacheSeed, repoCacheUpperPath),
		LogPath:     runner.LogPath,
		MetricsPath: runner.MetricsPath,
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

		if err := vm.RestoreFromSnapshot(ctx, snapshotPaths.State, snapshotPaths.Mem, true); err != nil {
			m.logger.WithError(err).Warn("Snapshot restore failed, falling back to cold boot")
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

	runner.State = StateInitializing
	runner.StartedAt = time.Now()

	m.runners[runnerID] = runner
	m.vms[runnerID] = vm

	m.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"ip":        runner.InternalIP.String(),
		"snapshot":  runner.SnapshotVersion,
	}).Info("Runner allocated successfully")

	return runner, nil
}

// buildMMDSData builds the MMDS data structure for a runner
func (m *Manager) buildMMDSData(ctx context.Context, runner *Runner, tap *network.TapDevice, req AllocateRequest) MMDSData {
	netCfg := tap.GetNetworkConfig()

	var data MMDSData
	data.Latest.Meta.RunnerID = runner.ID
	data.Latest.Meta.HostID = m.config.HostID
	data.Latest.Meta.Environment = m.config.Environment
	data.Latest.Buildbarn.CertsMountPath = m.config.BuildbarnCertsMountPath
	data.Latest.Network.IP = netCfg.IP
	data.Latest.Network.Gateway = netCfg.Gateway
	data.Latest.Network.Netmask = netCfg.Netmask
	data.Latest.Network.DNS = netCfg.DNS
	data.Latest.Network.Interface = netCfg.Interface
	data.Latest.Network.MAC = netCfg.MAC
	data.Latest.Network.MTU = m.network.GetMTU()
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
			if len(m.config.GitHubRunnerLabels) > 0 {
				labels := make(map[string]string)
				for _, label := range m.config.GitHubRunnerLabels {
					labels[label] = "true"
				}
				data.Latest.Job.Labels = labels
			}
			m.logger.WithField("runner_id", runner.ID).Info("Got CI runner registration token")
		}
	}

	// Git cache configuration
	if m.config.GitCacheEnabled && m.gitCacheImage != "" {
		data.Latest.GitCache.Enabled = true
		data.Latest.GitCache.MountPath = m.config.GitCacheMountPath
		data.Latest.GitCache.RepoMappings = m.config.GitCacheRepoMappings

		// Ensure Job.Repo is set for git-cache workspace setup
		// This is needed even if GitHub runner registration fails
		if data.Latest.Job.Repo == "" && m.config.GitHubRepo != "" {
			data.Latest.Job.Repo = m.config.GitHubRepo
		}

		// Set pre-cloned path (where repo was cloned during warmup, baked into snapshot)
		// This allows thaw-agent to create symlinks from workspace to pre-cloned repo
		if m.config.GitCachePreClonedPath != "" {
			data.Latest.GitCache.PreClonedPath = m.config.GitCachePreClonedPath
		}
		// Note: if PreClonedPath is not set, thaw-agent will derive it from job.repo
	}

	// Always set WorkspaceDir - needed for pre-cloned repo symlink even without git-cache
	if m.config.GitCacheWorkspaceDir != "" {
		data.Latest.GitCache.WorkspaceDir = m.config.GitCacheWorkspaceDir
	}

	// Runner configuration
	data.Latest.Runner.Ephemeral = m.config.GitHubRunnerEphemeral
	if m.ciAdapter != nil {
		data.Latest.Runner.CISystem = m.ciAdapter.Name()
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

	m.cleanupRunner(runnerID, runner.TapDevice, runner.RootfsOverlay, runner.RepoCacheUpper)
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
		if err := m.network.BlockEgress(net.IP(ip)); err != nil {
			errs = append(errs, fmt.Errorf("block egress: %w", err))
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
		if err := m.network.UnblockEgress(net.IP(ip)); err != nil {
			errs = append(errs, fmt.Errorf("unblock egress: %w", err))
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
	// Release TAP slot if using slot-based allocation
	if slot, ok := m.runnerToSlot[runnerID]; ok {
		m.network.ReleaseTapSlot(slot, runnerID)
		delete(m.slotToRunner, slot)
		delete(m.runnerToSlot, runnerID)
	} else {
		// Release dynamic TAP device (cold boot path)
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
// All drives must be present to match the snapshot's drive layout for restore to work.
func (m *Manager) buildDrives(repoCacheSeedPath, repoCacheUpperPath string) []firecracker.Drive {
	drives := []firecracker.Drive{
		{
			DriveID:      "repo_cache_seed",
			PathOnHost:   repoCacheSeedPath,
			IsRootDevice: false,
			IsReadOnly:   true,
		},
		{
			DriveID:      "repo_cache_upper",
			PathOnHost:   repoCacheUpperPath,
			IsRootDevice: false,
			IsReadOnly:   false,
		},
		{
			DriveID:      "credentials",
			PathOnHost:   m.credentialsImage,
			IsRootDevice: false,
			IsReadOnly:   true,
		},
	}

	// Always add git-cache drive to match snapshot drive layout.
	// If no real git-cache image exists, use a placeholder.
	// This is required for snapshot restore to work (drive IDs must match).
	gitCacheImg := m.gitCacheImage
	if gitCacheImg == "" {
		gitCacheImg = m.getOrCreateGitCachePlaceholder()
	}
	if gitCacheImg != "" {
		drives = append(drives, firecracker.Drive{
			DriveID:      "git_cache",
			PathOnHost:   gitCacheImg,
			IsRootDevice: false,
			IsReadOnly:   true,
		})
	}

	return drives
}

// getOrCreateGitCachePlaceholder ensures a placeholder git-cache image exists for snapshot restore compatibility.
func (m *Manager) getOrCreateGitCachePlaceholder() string {
	sharedDir := filepath.Join(m.config.WorkspaceDir, "_shared")
	placeholderPath := filepath.Join(sharedDir, "git-cache-placeholder.img")

	// Check if already exists
	if _, err := os.Stat(placeholderPath); err == nil {
		return placeholderPath
	}

	// Create placeholder directory
	if err := os.MkdirAll(sharedDir, 0755); err != nil {
		m.logger.WithError(err).Warn("Failed to create shared dir for git-cache placeholder")
		return ""
	}

	// Create small placeholder image (64MB is minimum for ext4)
	if err := createExt4ImageMB(placeholderPath, 64, "GIT_CACHE"); err != nil {
		m.logger.WithError(err).Warn("Failed to create git-cache placeholder image")
		return ""
	}

	m.logger.WithField("path", placeholderPath).Info("Created git-cache placeholder image for snapshot compatibility")
	return placeholderPath
}

func createExt4Image(path string, sizeGB int, label string) error {
	if sizeGB <= 0 {
		return fmt.Errorf("invalid sizeGB: %d", sizeGB)
	}
	if err := exec.Command("truncate", "-s", fmt.Sprintf("%dG", sizeGB), path).Run(); err != nil {
		return fmt.Errorf("truncate failed: %w", err)
	}
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", label, path).CombinedOutput(); err != nil {
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
	sizeMB := cfg.BuildbarnCertsImageSizeMB
	if sizeMB <= 0 {
		sizeMB = 32
	}

	seedDir := cfg.BuildbarnCertsDir
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
	if output, err := exec.Command("mkfs.ext4", "-F", "-L", label, path).CombinedOutput(); err != nil {
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
	for _, r := range m.runners {
		switch r.State {
		case StateIdle:
			idle++
		case StateBusy:
			busy++
		}
	}

	return ManagerStatus{
		TotalSlots:      m.config.MaxRunners,
		UsedSlots:       len(m.runners),
		IdleRunners:     idle,
		BusyRunners:     busy,
		SnapshotVersion: m.snapshotCache.CurrentVersion(),
		Draining:        m.draining,
	}
}

// ManagerStatus represents the status of the runner manager
type ManagerStatus struct {
	TotalSlots      int
	UsedSlots       int
	IdleRunners     int
	BusyRunners     int
	SnapshotVersion string
	Draining        bool
}

// SyncSnapshot syncs a new snapshot version from GCS
func (m *Manager) SyncSnapshot(ctx context.Context, version string) error {
	m.logger.WithField("version", version).Info("Syncing snapshot")
	return m.snapshotCache.SyncFromGCS(ctx, version)
}

// CanAddRunner checks if a new runner can be added
func (m *Manager) CanAddRunner() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.draining && len(m.runners) < m.config.MaxRunners
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

	return nil
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
	diskUsage := int64(m.config.RepoCacheUpperSizeGB) * 1024 * 1024 * 1024

	return &VMStats{
		MemoryUsageBytes: memoryUsage,
		DiskUsageBytes:   diskUsage,
	}, nil
}

// removeRunnerVM removes a runner completely (pool callback)
func (m *Manager) removeRunnerVM(ctx context.Context, runnerID string) error {
	return m.ReleaseRunner(runnerID, true)
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
