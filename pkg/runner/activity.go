package runner

import (
	"fmt"
	"sync/atomic"
	"time"
)

type PeriodicCheckpointCandidate struct {
	RunnerID                     string
	SessionID                    string
	WorkloadKey                  string
	HostID                       string
	ManifestPath                 string
	Generation                   int
	RunnerTTLSeconds             int
	AutoPause                    bool
	CheckpointIntervalSeconds    int
	CheckpointQuietWindowSeconds int
	NetworkPolicyPreset          string
	NetworkPolicyJSON            string
	LastActivityAt               time.Time
	LastCheckpointAt             time.Time
}

// MarkActivity records observable runner traffic. It updates both LastActivityAt
// and LastExecAt so TTL enforcement treats proxy traffic the same as exec/file activity.
func (m *Manager) MarkActivity(runnerID string) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}
	now := time.Now()
	runner.mu.Lock()
	runner.LastActivityAt = now
	runner.LastExecAt = now
	runner.mu.Unlock()
}

func (m *Manager) TryAcquireProxyStream(runnerID string) error {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("runner not found: %s", runnerID)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	switch runner.State {
	case StatePausing, StateSuspended, StateTerminated, StateQuarantined:
		return fmt.Errorf("runner %s is %s", runnerID, runner.State)
	}
	now := time.Now()
	atomic.AddInt32(&runner.ActiveProxyStreams, 1)
	runner.LastActivityAt = now
	runner.LastExecAt = now
	return nil
}

func (m *Manager) ReleaseProxyStream(runnerID string) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}
	now := time.Now()
	runner.mu.Lock()
	atomic.AddInt32(&runner.ActiveProxyStreams, -1)
	runner.LastActivityAt = now
	runner.LastExecAt = now
	runner.mu.Unlock()
}

func (m *Manager) ListPeriodicCheckpointCandidates(now time.Time) []PeriodicCheckpointCandidate {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var candidates []PeriodicCheckpointCandidate
	for _, runner := range m.runners {
		runner.mu.Lock()
		if runner.SessionID == "" ||
			(runner.State != StateIdle && runner.State != StateBusy) ||
			runner.CheckpointIntervalSeconds <= 0 ||
			runner.CheckpointInFlight ||
			atomic.LoadInt32(&runner.ActiveExecs) > 0 ||
			atomic.LoadInt32(&runner.ActiveProxyStreams) > 0 {
			runner.mu.Unlock()
			continue
		}

		lastCheckpoint := runner.LastCheckpointAt
		if lastCheckpoint.IsZero() {
			lastCheckpoint = runner.CreatedAt
		}
		if !lastCheckpoint.IsZero() && now.Sub(lastCheckpoint) < time.Duration(runner.CheckpointIntervalSeconds)*time.Second {
			runner.mu.Unlock()
			continue
		}

		lastActivity := runner.LastActivityAt
		if lastActivity.IsZero() {
			lastActivity = runner.LastExecAt
		}
		if lastActivity.IsZero() {
			lastActivity = runner.StartedAt
		}
		if runner.CheckpointQuietWindowSeconds > 0 && !lastActivity.IsZero() &&
			now.Sub(lastActivity) < time.Duration(runner.CheckpointQuietWindowSeconds)*time.Second {
			runner.mu.Unlock()
			continue
		}

		candidates = append(candidates, PeriodicCheckpointCandidate{
			RunnerID:                     runner.ID,
			SessionID:                    runner.SessionID,
			WorkloadKey:                  runner.WorkloadKey,
			HostID:                       runner.HostID,
			ManifestPath:                 runner.SessionManifestPath,
			Generation:                   runner.SessionLayers,
			RunnerTTLSeconds:             runner.TTLSeconds,
			AutoPause:                    runner.AutoPause,
			CheckpointIntervalSeconds:    runner.CheckpointIntervalSeconds,
			CheckpointQuietWindowSeconds: runner.CheckpointQuietWindowSeconds,
			LastActivityAt:               lastActivity,
			LastCheckpointAt:             runner.LastCheckpointAt,
		})
		runner.mu.Unlock()
	}
	return candidates
}

func (m *Manager) TryBeginPeriodicCheckpoint(runnerID string) bool {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return false
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.SessionID == "" ||
		(runner.State != StateIdle && runner.State != StateBusy) ||
		runner.CheckpointIntervalSeconds <= 0 ||
		runner.CheckpointInFlight ||
		atomic.LoadInt32(&runner.ActiveExecs) > 0 ||
		atomic.LoadInt32(&runner.ActiveProxyStreams) > 0 {
		return false
	}
	runner.CheckpointInFlight = true
	return true
}

func (m *Manager) FinishPeriodicCheckpoint(runnerID string, succeeded bool, at time.Time) {
	m.mu.RLock()
	runner, exists := m.runners[runnerID]
	m.mu.RUnlock()
	if !exists {
		return
	}

	runner.mu.Lock()
	runner.CheckpointInFlight = false
	if succeeded {
		runner.LastCheckpointAt = at
	}
	runner.mu.Unlock()
}
