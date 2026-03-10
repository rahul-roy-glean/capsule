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

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy"
	_ "github.com/rahul-roy-glean/bazel-firecracker/pkg/authproxy/providers" // register providers
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

	// sessionMemStore and sessionDiskStore are chunk stores for GCS-backed
	// session pause/resume (nil when SessionChunkBucket is not configured).
	// When non-nil, PauseRunner uploads dirty diff chunks to GCS and
	// ResumeFromSession fetches chunks lazily via UFFD from any host.
	sessionMemStore  *snapshot.ChunkStore
	sessionDiskStore *snapshot.ChunkStore

	// authProxies tracks per-VM auth proxy instances for credential injection.
	authProxies map[string]*authproxy.AuthProxy

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

	// cleanupFUSEDisks unmounts and removes all FUSE disks (rootfs + extension)
	// for a runner. Called during pause/checkpoint after getting dirty chunks
	// and after VM stop, so the next resume can create fresh FUSE mounts.
	// Nil when FUSE disks are not in use (non-chunked mode).
	cleanupFUSEDisks func(runnerID string)

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
func NewManager(ctx context.Context, cfg HostConfig, logger *logrus.Logger) (*Manager, error) {
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

	// Ensure directories exist
	for _, dir := range []string{cfg.SocketDir, cfg.WorkspaceDir, cfg.LogDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	m := &Manager{
		config:          cfg,
		runners:         make(map[string]*Runner),
		recentRequests:  make(map[string]*recentAllocation),
		vms:             make(map[string]*firecracker.VM),
		uffdHandlers:    make(map[string]uffdStopper),
		snapshotCache:   cache,
		slotToRunner:    make(map[int]string),
		runnerToSlot:    make(map[string]int),
		policyEnforcers: make(map[string]*network.PolicyEnforcer),
		authProxies:     make(map[string]*authproxy.AuthProxy),
		logger:          logger.WithField("component", "runner-manager"),
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

// SetNetNSNetwork configures the manager to use per-VM network namespaces.
// Each VM gets a namespace with point-to-point veth routing, and Firecracker
// is launched inside the namespace.
func (m *Manager) SetNetNSNetwork(netnsNet *network.NetNSNetwork) {
	m.netnsNetwork = netnsNet
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
	data.Latest.Network.IP = netCfg.IP
	data.Latest.Network.Gateway = netCfg.Gateway
	data.Latest.Network.Netmask = netCfg.Netmask
	data.Latest.Network.DNS = netCfg.DNS
	data.Latest.Network.Interface = netCfg.Interface
	data.Latest.Network.MAC = netCfg.MAC
	data.Latest.Meta.Labels = req.Labels
	data.Latest.Snapshot.Version = runner.SnapshotVersion

	// Populate start_command for user service startup
	if req.StartCommand != nil {
		data.Latest.StartCommand.Command = req.StartCommand.Command
		data.Latest.StartCommand.Port = req.StartCommand.Port
		data.Latest.StartCommand.HealthPath = req.StartCommand.HealthPath
		data.Latest.StartCommand.Env = req.StartCommand.Env
		data.Latest.StartCommand.RunAs = req.StartCommand.RunAs
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

	// Stop auth proxy if one exists
	if proxy, ok := m.authProxies[runnerID]; ok {
		proxy.Stop()
		delete(m.authProxies, runnerID)
	}

	// Suspended runners already had network released during pause; only clean up overlay/files
	if runner.State == StateSuspended {
		if runner.RootfsOverlay != "" {
			os.Remove(runner.RootfsOverlay)
		}
		os.Remove(filepath.Join(m.config.SocketDir, runnerID+".sock"))
	} else {
		m.cleanupRunner(runnerID, runner.TapDevice, runner.RootfsOverlay)
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
	snapshotVersion := r.SnapshotVersion
	m.mu.Unlock()

	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create quarantine dir: %w", err)
	}

	_ = os.Symlink(logPath, filepath.Join(quarantineDir, "runner.log"))
	_ = os.Symlink(metricsPath, filepath.Join(quarantineDir, "runner.metrics"))
	_ = os.Symlink(rootfsOverlay, filepath.Join(quarantineDir, "rootfs-overlay.img"))

	var errs []error
	egressBlocked := false
	if blockEgress {
		var blockErr error
		if m.netnsNetwork != nil {
			// Per-VM namespace mode: block by veth interface name
			blockErr = m.netnsNetwork.EmergencyBlockEgress(runnerID)
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
func (m *Manager) cleanupRunner(runnerID, tapDevice, overlayPath string) {
	if m.netnsNetwork != nil {
		// Per-VM namespace mode: release the entire namespace
		m.netnsNetwork.ReleaseNamespace(runnerID)
	}
	if slot, ok := m.runnerToSlot[runnerID]; ok {
		delete(m.slotToRunner, slot)
		delete(m.runnerToSlot, runnerID)
	}

	// Remove overlay
	if overlayPath != "" {
		os.Remove(overlayPath)
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
	var drives []firecracker.Drive

	// All drives come from extensionDrivePaths in deterministic order (sorted by driveID).
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

// to the actual host paths. Firecracker validates (opens) drive backing files during
// LoadSnapshot at the paths recorded in the snapshot state. Since the snapshot was
// built with drives at /tmp/snapshot/*.img but the host has them at different locations,
// symlinks bridge the gap.
//
// This function must be called while m.mu is held (the caller holds it),
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
	Draining           bool
	TotalCPUMillicores int
	UsedCPUMillicores  int
	TotalMemoryMB      int
	UsedMemoryMB       int
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

// Close shuts down the manager and all runners
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info("Shutting down runner manager")

	// Stop all VMs
	for id, vm := range m.vms {
		m.logger.WithField("runner_id", id).Debug("Stopping VM")
		vm.Stop()
	}

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
