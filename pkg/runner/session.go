package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/firecracker"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/uffd"
)

const (
	// defaultSessionDir is the fallback session directory when not configured.
	defaultSessionDir = "/tmp/fc-dev/sessions"
)

// sessionBaseDir returns the session storage directory from config, with a fallback.
func (m *Manager) sessionBaseDir() string {
	if m.config.SessionDir != "" {
		return m.config.SessionDir
	}
	// Derive from SnapshotCachePath: /mnt/data/snapshots → /mnt/data/sessions
	if m.config.SnapshotCachePath != "" {
		return filepath.Join(filepath.Dir(m.config.SnapshotCachePath), "sessions")
	}
	return defaultSessionDir
}

// PauseResult contains the result of a PauseRunner operation.
type PauseResult struct {
	SessionID         string `json:"session_id"`
	Layer             int    `json:"layer"`
	SnapshotSizeBytes int64  `json:"snapshot_size_bytes"`
}

// SessionMetadata is written to each session directory as metadata.json.
type SessionMetadata struct {
	SessionID   string    `json:"session_id"`
	WorkloadKey string    `json:"workload_key"`
	RunnerID    string    `json:"runner_id"`
	HostID      string    `json:"host_id"`
	Layers      int       `json:"layers"`
	CreatedAt   time.Time `json:"created_at"`
	PausedAt    time.Time `json:"paused_at"`
	// RootfsPath is the path to the dirty rootfs overlay for this session.
	RootfsPath string `json:"rootfs_path"`
	// RepoCacheUpperPath is the per-runner repo cache writable layer.
	RepoCacheUpperPath string `json:"repo_cache_upper_path"`
	// TTL config preserved across pause/resume
	TTLSeconds int  `json:"ttl_seconds,omitempty"`
	AutoPause  bool `json:"auto_pause,omitempty"`
}

// PauseRunner creates a diff snapshot of the runner's VM, saves session state,
// and destroys the VM. The runner transitions to StateSuspended.
func (m *Manager) PauseRunner(ctx context.Context, runnerID string) (*PauseResult, error) {
	m.mu.Lock()
	runner, exists := m.runners[runnerID]
	if !exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner not found: %s", runnerID)
	}

	if runner.SessionID == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner %s has no session_id, cannot pause", runnerID)
	}

	if runner.State == StatePausing || runner.State == StateSuspended {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner %s is already %s", runnerID, runner.State)
	}

	if atomic.LoadInt32(&runner.ActiveExecs) > 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("runner %s has active execs, cannot pause", runnerID)
	}

	vm := m.vms[runnerID]
	if vm == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("VM not found for runner %s", runnerID)
	}

	runner.State = StatePausing
	sessionID := runner.SessionID
	layerN := runner.SessionLayers
	m.mu.Unlock()

	m.logger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"session_id": sessionID,
		"layer":      layerN,
	}).Info("Pausing runner (creating diff snapshot)")

	// Create session layer directory
	layerDir := filepath.Join(m.sessionBaseDir(), sessionID, fmt.Sprintf("layer_%d", layerN))
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		m.mu.Lock()
		runner.State = StateIdle
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to create session layer dir: %w", err)
	}

	stateFile := filepath.Join(layerDir, "snapshot.state")
	memDiffFile := filepath.Join(layerDir, "mem_diff.sparse")

	// Create diff snapshot (pauses VM internally)
	if err := vm.CreateDiffSnapshot(ctx, stateFile, memDiffFile); err != nil {
		m.mu.Lock()
		runner.State = StateIdle
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to create diff snapshot: %w", err)
	}

	// Calculate snapshot size
	var snapshotSize int64
	if info, err := os.Stat(memDiffFile); err == nil {
		snapshotSize += info.Size()
	}
	if info, err := os.Stat(stateFile); err == nil {
		snapshotSize += info.Size()
	}

	// Write metadata.json
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)
	metadata := SessionMetadata{
		SessionID:          sessionID,
		WorkloadKey:        runner.WorkloadKey,
		RunnerID:           runnerID,
		HostID:             m.config.HostID,
		Layers:             layerN + 1,
		CreatedAt:          runner.CreatedAt,
		PausedAt:           time.Now(),
		RootfsPath:         runner.RootfsOverlay,
		RepoCacheUpperPath: runner.RepoCacheUpper,
		TTLSeconds:         runner.TTLSeconds,
		AutoPause:          runner.AutoPause,
	}
	metadataBytes, _ := json.MarshalIndent(metadata, "", "  ")
	if err := os.WriteFile(filepath.Join(sessionDir, "metadata.json"), metadataBytes, 0644); err != nil {
		m.logger.WithError(err).Warn("Failed to write session metadata")
	}

	// Stop VM and clean up resources (but NOT the rootfs overlay or repo cache — session needs them)
	vm.Stop()

	m.mu.Lock()
	delete(m.vms, runnerID)

	// Stop UFFD handler if one exists (from a previous resume)
	if handler, ok := m.uffdHandlers[runnerID]; ok {
		handler.Stop()
		delete(m.uffdHandlers, runnerID)
	}

	// Release network namespace / TAP slot but keep overlay and repo cache
	if m.netnsNetwork != nil {
		m.netnsNetwork.ReleaseNamespace(runnerID)
		if slot, ok := m.runnerToSlot[runnerID]; ok {
			delete(m.slotToRunner, slot)
			delete(m.runnerToSlot, runnerID)
		}
	} else if slot, ok := m.runnerToSlot[runnerID]; ok {
		m.network.ReleaseTapSlot(slot, runnerID)
		delete(m.slotToRunner, slot)
		delete(m.runnerToSlot, runnerID)
	} else {
		m.network.ReleaseTap(runnerID)
	}

	// Remove socket
	os.Remove(filepath.Join(m.config.SocketDir, runnerID+".sock"))

	// Update runner state
	runner.State = StateSuspended
	runner.SessionLayers = layerN + 1
	runner.SessionDir = sessionDir
	runner.PausedAt = time.Now()
	m.mu.Unlock()

	m.logger.WithFields(logrus.Fields{
		"runner_id":     runnerID,
		"session_id":    sessionID,
		"layer":         layerN,
		"snapshot_size": snapshotSize,
	}).Info("Runner paused successfully")

	return &PauseResult{
		SessionID:         sessionID,
		Layer:             layerN,
		SnapshotSizeBytes: snapshotSize,
	}, nil
}

// ResumeFromSession restores a runner from a session snapshot using layered UFFD.
func (m *Manager) ResumeFromSession(ctx context.Context, sessionID, workloadKey string) (*Runner, error) {
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)

	// Read metadata
	metadataBytes, err := os.ReadFile(filepath.Join(sessionDir, "metadata.json"))
	if err != nil {
		return nil, fmt.Errorf("session %s not found: %w", sessionID, err)
	}

	var metadata SessionMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("invalid session metadata: %w", err)
	}

	if workloadKey != "" && metadata.WorkloadKey != workloadKey {
		return nil, fmt.Errorf("workload_key mismatch: session has %s, requested %s", metadata.WorkloadKey, workloadKey)
	}

	if metadata.Layers == 0 {
		return nil, fmt.Errorf("session %s has no layers", sessionID)
	}

	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return nil, fmt.Errorf("host is draining")
	}
	// Count active runners (exclude suspended ones — they don't consume VM resources)
	activeCount := 0
	for _, r := range m.runners {
		if r.State != StateSuspended {
			activeCount++
		}
	}
	if activeCount >= m.config.MaxRunners {
		m.mu.Unlock()
		return nil, fmt.Errorf("host at capacity: %d/%d runners", activeCount, m.config.MaxRunners)
	}

	// Check if there's already a runner for this session that's not suspended
	for _, r := range m.runners {
		if r.SessionID == sessionID && r.State != StateSuspended {
			m.mu.Unlock()
			return nil, fmt.Errorf("session %s already has an active runner %s in state %s", sessionID, r.ID, r.State)
		}
	}
	m.mu.Unlock()

	m.logger.WithFields(logrus.Fields{
		"session_id":   sessionID,
		"layers":       metadata.Layers,
		"workload_key": metadata.WorkloadKey,
	}).Info("Resuming runner from session snapshot")

	// Get golden snapshot paths
	snapshotPaths, err := m.snapshotCache.GetSnapshotPaths()
	if err != nil {
		return nil, fmt.Errorf("failed to get snapshot paths: %w", err)
	}

	m.mu.Lock()

	// Use the original runner ID from the session
	runnerID := metadata.RunnerID

	// Allocate network
	var tap *network.TapDevice
	var nsInfo *network.VMNamespace
	var slot int = -1
	useNetNS := m.netnsNetwork != nil

	if useNetNS {
		slot = m.findAvailableSlot()
		if slot < 0 {
			m.mu.Unlock()
			return nil, fmt.Errorf("no slots available")
		}
		nsInfo, err = m.netnsNetwork.CreateNamespaceForVM(runnerID, slot)
		if err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("failed to create network namespace: %w", err)
		}
		tap = nsInfo.GetTapDevice(m.netnsNetwork.GetSubnet())
		m.slotToRunner[slot] = runnerID
		m.runnerToSlot[runnerID] = slot
	} else {
		slot = m.findAvailableSlot()
		if slot < 0 {
			m.mu.Unlock()
			return nil, fmt.Errorf("no TAP slots available")
		}
		tap, err = m.network.GetOrCreateTapSlot(slot, runnerID)
		if err != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("failed to get TAP slot: %w", err)
		}
		m.slotToRunner[slot] = runnerID
		m.runnerToSlot[runnerID] = slot
	}
	m.mu.Unlock()

	// Use the session's rootfs overlay (preserved during pause)
	overlayPath := metadata.RootfsPath
	repoCacheUpperPath := metadata.RepoCacheUpperPath

	// Build diff layer paths (oldest first)
	var diffLayers []string
	for i := 0; i < metadata.Layers; i++ {
		layerPath := filepath.Join(sessionDir, fmt.Sprintf("layer_%d", i), "mem_diff.sparse")
		if _, err := os.Stat(layerPath); err == nil {
			diffLayers = append(diffLayers, layerPath)
		}
	}

	// Latest layer's state file
	latestStateFile := filepath.Join(sessionDir, fmt.Sprintf("layer_%d", metadata.Layers-1), "snapshot.state")

	// Start layered UFFD handler
	uffdSocketPath := filepath.Join(m.config.SocketDir, runnerID+"-uffd.sock")
	uffdHandler, err := uffd.NewLayeredHandler(uffd.LayeredHandlerConfig{
		SocketPath:    uffdSocketPath,
		GoldenMemPath: snapshotPaths.Mem,
		DiffLayers:    diffLayers,
		Logger:        m.logger.Logger,
	})
	if err != nil {
		m.mu.Lock()
		m.cleanupNetworkOnly(runnerID, tap.Name)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to create layered UFFD handler: %w", err)
	}

	if err := uffdHandler.Start(); err != nil {
		uffdHandler.Stop()
		m.mu.Lock()
		m.cleanupNetworkOnly(runnerID, tap.Name)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to start UFFD handler: %w", err)
	}

	// Set up network config
	internalIP := tap.IP
	if useNetNS && nsInfo != nil {
		internalIP = nsInfo.HostReachableIP
	}

	// Create VM config
	vmCfg := firecracker.VMConfig{
		VMID:           runnerID,
		SocketDir:      m.config.SocketDir,
		FirecrackerBin: m.config.FirecrackerBin,
		KernelPath:     snapshotPaths.Kernel,
		RootfsPath:     overlayPath,
		VCPUs:          m.config.VCPUsPerRunner,
		MemoryMB:       m.config.MemoryMBPerRunner,
		NetworkIface: &firecracker.NetworkInterface{
			IfaceID:     "eth0",
			HostDevName: tap.Name,
			GuestMAC:    tap.MAC,
		},
		Drives:  m.buildDrives(snapshotPaths.RepoCacheSeed, repoCacheUpperPath),
		LogPath: filepath.Join(m.config.LogDir, runnerID+".log"),
	}

	if useNetNS && nsInfo != nil {
		vmCfg.NetNSPath = nsInfo.GetFirecrackerNetNSPath()
	}

	vm, err := firecracker.NewVM(vmCfg, m.logger.Logger)
	if err != nil {
		uffdHandler.Stop()
		m.mu.Lock()
		m.cleanupNetworkOnly(runnerID, tap.Name)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	// Setup symlinks for snapshot restore
	m.mu.Lock()
	cleanup, symlinkErr := m.setupSnapshotSymlinks(overlayPath, repoCacheUpperPath, snapshotPaths)
	m.mu.Unlock()
	if symlinkErr != nil {
		uffdHandler.Stop()
		vm.Stop()
		m.mu.Lock()
		m.cleanupNetworkOnly(runnerID, tap.Name)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to setup snapshot symlinks: %w", symlinkErr)
	}

	// Restore from snapshot with UFFD
	var restoreErr error
	if useNetNS {
		restoreErr = vm.RestoreFromSnapshotWithUFFD(ctx, latestStateFile, uffdSocketPath, true)
		cleanup()
	} else {
		m.mu.Lock()
		tapRestore, tapErr := m.setupSnapshotTAPRename(tap.Name)
		m.mu.Unlock()
		if tapErr != nil {
			cleanup()
			uffdHandler.Stop()
			vm.Stop()
			m.mu.Lock()
			m.cleanupNetworkOnly(runnerID, tap.Name)
			m.mu.Unlock()
			return nil, fmt.Errorf("failed to setup TAP rename: %w", tapErr)
		}
		restoreErr = vm.RestoreFromSnapshotWithUFFD(ctx, latestStateFile, uffdSocketPath, true)
		tapRestore()
		cleanup()
	}

	if restoreErr != nil {
		uffdHandler.Stop()
		vm.Stop()
		m.mu.Lock()
		m.cleanupNetworkOnly(runnerID, tap.Name)
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to restore from session snapshot: %w", restoreErr)
	}

	// Set up port forwarding for netns
	if useNetNS && m.netnsNetwork != nil {
		if err := m.netnsNetwork.ForwardPort(runnerID, 8080); err != nil {
			m.logger.WithError(err).Warn("Failed to forward port 8080")
		}
		if err := m.netnsNetwork.ForwardPort(runnerID, 8081); err != nil {
			m.logger.WithError(err).Warn("Failed to forward port 8081")
		}
	}

	// Build runner record
	runner := &Runner{
		ID:              runnerID,
		HostID:          m.config.HostID,
		State:           StateIdle,
		InternalIP:      internalIP,
		TapDevice:       tap.Name,
		MAC:             tap.MAC,
		SnapshotVersion: snapshotPaths.Version,
		WorkloadKey:     metadata.WorkloadKey,
		Resources: Resources{
			VCPUs:    m.config.VCPUsPerRunner,
			MemoryMB: m.config.MemoryMBPerRunner,
		},
		CreatedAt:      metadata.CreatedAt,
		StartedAt:      time.Now(),
		SocketPath:     filepath.Join(m.config.SocketDir, runnerID+".sock"),
		LogPath:        filepath.Join(m.config.LogDir, runnerID+".log"),
		RootfsOverlay:  overlayPath,
		RepoCacheUpper: repoCacheUpperPath,
		SessionID:      sessionID,
		SessionDir:     sessionDir,
		SessionLayers:  metadata.Layers,
		TTLSeconds:     metadata.TTLSeconds,
		AutoPause:      metadata.AutoPause,
		LastExecAt:     time.Now(),
	}

	m.mu.Lock()
	// Remove any old suspended entry for this runner
	delete(m.runners, runnerID)
	m.runners[runnerID] = runner
	m.vms[runnerID] = vm
	m.uffdHandlers[runnerID] = uffdHandler
	m.mu.Unlock()

	// Inject MMDS data
	mmdsData := m.buildMMDSData(ctx, runner, tap, AllocateRequest{
		WorkloadKey: metadata.WorkloadKey,
	})
	if err := vm.SetMMDSData(ctx, mmdsData); err != nil {
		m.logger.WithError(err).Warn("Failed to set MMDS data on resumed runner")
	}

	m.logger.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"session_id": sessionID,
		"layers":     metadata.Layers,
	}).Info("Runner resumed from session snapshot successfully")

	return runner, nil
}

// cleanupNetworkOnly releases network resources without touching overlay or repo cache.
func (m *Manager) cleanupNetworkOnly(runnerID, _ string) {
	if m.netnsNetwork != nil {
		m.netnsNetwork.ReleaseNamespace(runnerID)
		if slot, ok := m.runnerToSlot[runnerID]; ok {
			delete(m.slotToRunner, slot)
			delete(m.runnerToSlot, runnerID)
		}
	} else if slot, ok := m.runnerToSlot[runnerID]; ok {
		m.network.ReleaseTapSlot(slot, runnerID)
		delete(m.slotToRunner, slot)
		delete(m.runnerToSlot, runnerID)
	} else {
		m.network.ReleaseTap(runnerID)
	}
	os.Remove(filepath.Join(m.config.SocketDir, runnerID+".sock"))
}

// IncrementActiveExecs atomically increments the active exec count for a runner.
func (m *Manager) IncrementActiveExecs(runnerID string) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}
	atomic.AddInt32(&runner.ActiveExecs, 1)
}

// DecrementActiveExecs atomically decrements the active exec count and updates LastExecAt.
func (m *Manager) DecrementActiveExecs(runnerID string) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}
	atomic.AddInt32(&runner.ActiveExecs, -1)
	m.mu.Lock()
	runner.LastExecAt = time.Now()
	m.mu.Unlock()
}

// ResetTTL updates LastExecAt for a runner, extending its idle TTL timer.
func (m *Manager) ResetTTL(runnerID string) {
	m.mu.Lock()
	runner, exists := m.runners[runnerID]
	if exists {
		runner.LastExecAt = time.Now()
	}
	m.mu.Unlock()
}

// StartTTLEnforcement starts a background goroutine that auto-pauses or destroys
// idle runners whose TTL has expired.
func (m *Manager) StartTTLEnforcement(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.enforceTTLs(ctx)
			}
		}
	}()
}

// enforceTTLs checks all runners for TTL expiry and pauses/destroys as needed.
func (m *Manager) enforceTTLs(ctx context.Context) {
	m.mu.RLock()
	var candidates []struct {
		id        string
		autoPause bool
	}
	for _, r := range m.runners {
		if r.TTLSeconds <= 0 {
			continue
		}
		if r.State != StateIdle {
			continue
		}
		if atomic.LoadInt32(&r.ActiveExecs) > 0 {
			continue
		}
		// Skip if LastExecAt was never set (runner never executed anything)
		if r.LastExecAt.IsZero() {
			continue
		}
		if time.Since(r.LastExecAt) < time.Duration(r.TTLSeconds)*time.Second {
			continue
		}
		candidates = append(candidates, struct {
			id        string
			autoPause bool
		}{id: r.ID, autoPause: r.AutoPause})
	}
	m.mu.RUnlock()

	for _, c := range candidates {
		if c.autoPause {
			m.logger.WithField("runner_id", c.id).Info("TTL expired, auto-pausing runner")
			if _, err := m.PauseRunner(ctx, c.id); err != nil {
				m.logger.WithError(err).WithField("runner_id", c.id).Warn("Failed to auto-pause runner on TTL")
			}
		} else {
			m.logger.WithField("runner_id", c.id).Info("TTL expired, destroying runner")
			if err := m.ReleaseRunner(c.id, true); err != nil {
				m.logger.WithError(err).WithField("runner_id", c.id).Warn("Failed to destroy runner on TTL")
			}
		}
	}
}

// SessionExists checks if a session snapshot exists on disk.
func (m *Manager) SessionExists(sessionID string) bool {
	metadataPath := filepath.Join(m.sessionBaseDir(), sessionID, "metadata.json")
	_, err := os.Stat(metadataPath)
	return err == nil
}

// GetSessionMetadata reads the metadata for a session snapshot.
func (m *Manager) GetSessionMetadata(sessionID string) (*SessionMetadata, error) {
	metadataPath := filepath.Join(m.sessionBaseDir(), sessionID, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, err
	}
	var meta SessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// CleanupSession removes a session's snapshot files from disk.
func (m *Manager) CleanupSession(sessionID string) error {
	sessionDir := filepath.Join(m.sessionBaseDir(), sessionID)
	return os.RemoveAll(sessionDir)
}

// FindRunnerBySessionID returns a runner with the given session ID, if any.
func (m *Manager) FindRunnerBySessionID(sessionID string) *Runner {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.runners {
		if r.SessionID == sessionID {
			return r
		}
	}
	return nil
}
