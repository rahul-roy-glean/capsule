package runner

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyActivityTracking(t *testing.T) {
	m := newTestManager()
	r := &Runner{ID: "r1", State: StateBusy}
	m.runners["r1"] = r

	if err := m.TryAcquireProxyStream("r1"); err != nil {
		t.Fatalf("TryAcquireProxyStream() error = %v", err)
	}
	if got := atomic.LoadInt32(&r.ActiveProxyStreams); got != 1 {
		t.Fatalf("ActiveProxyStreams = %d, want 1", got)
	}
	if r.LastActivityAt.IsZero() || r.LastExecAt.IsZero() {
		t.Fatal("proxy activity should update last activity timestamps")
	}

	m.ReleaseProxyStream("r1")
	if got := atomic.LoadInt32(&r.ActiveProxyStreams); got != 0 {
		t.Fatalf("ActiveProxyStreams = %d, want 0", got)
	}
}

func TestListPeriodicCheckpointCandidatesRequiresQuietWindow(t *testing.T) {
	m := newTestManager()
	now := time.Now()
	r := &Runner{
		ID:                           "r1",
		State:                        StateBusy,
		SessionID:                    "sess-1",
		WorkloadKey:                  "wk123",
		CheckpointIntervalSeconds:    30,
		CheckpointQuietWindowSeconds: 10,
		LastActivityAt:               now.Add(-20 * time.Second),
		LastCheckpointAt:             now.Add(-40 * time.Second),
	}
	m.runners["r1"] = r

	candidates := m.ListPeriodicCheckpointCandidates(now)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	r.LastActivityAt = now.Add(-5 * time.Second)
	candidates = m.ListPeriodicCheckpointCandidates(now)
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates inside quiet window, got %d", len(candidates))
	}
}
