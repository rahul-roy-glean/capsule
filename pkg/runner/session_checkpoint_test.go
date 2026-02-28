package runner

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
)

func TestCheckpointResult_JSON(t *testing.T) {
	result := CheckpointResult{
		SessionID:         "sess-abc",
		Layer:             3,
		SnapshotSizeBytes: 2 * 1024 * 1024,
		Running:           true,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CheckpointResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", decoded.SessionID, "sess-abc")
	}
	if decoded.Layer != 3 {
		t.Errorf("Layer = %d, want 3", decoded.Layer)
	}
	if decoded.SnapshotSizeBytes != 2*1024*1024 {
		t.Errorf("SnapshotSizeBytes = %d, want %d", decoded.SnapshotSizeBytes, 2*1024*1024)
	}
	if !decoded.Running {
		t.Error("Running should be true")
	}
}

func TestCheckpointResult_RunningFieldPresent(t *testing.T) {
	// Ensure "running" is always present in JSON (not omitted when false)
	result := CheckpointResult{
		SessionID: "s1",
		Running:   false,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal raw failed: %v", err)
	}

	if _, ok := raw["running"]; !ok {
		t.Error("running field should be present in JSON even when false")
	}
}

func TestCheckpointRunner_NotFound(t *testing.T) {
	m := newTestManager()

	_, err := m.CheckpointRunner(context.Background(), "nonexistent")
	if err == nil {
		t.Error("CheckpointRunner should fail for nonexistent runner")
	}
}

func TestCheckpointRunner_NoSessionID(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}

	_, err := m.CheckpointRunner(context.Background(), "r1")
	if err == nil {
		t.Error("CheckpointRunner should fail without session_id")
	}
}

func TestCheckpointRunner_WrongState_Suspended(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateSuspended, SessionID: "sess-1"}

	_, err := m.CheckpointRunner(context.Background(), "r1")
	if err == nil {
		t.Error("CheckpointRunner should fail for suspended runner")
	}
}

func TestCheckpointRunner_WrongState_Pausing(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StatePausing, SessionID: "sess-1"}

	_, err := m.CheckpointRunner(context.Background(), "r1")
	if err == nil {
		t.Error("CheckpointRunner should fail for pausing runner")
	}
}

func TestCheckpointRunner_WrongState_Terminated(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateTerminated, SessionID: "sess-1"}

	_, err := m.CheckpointRunner(context.Background(), "r1")
	if err == nil {
		t.Error("CheckpointRunner should fail for terminated runner")
	}
}

func TestCheckpointRunner_AllowedStates(t *testing.T) {
	// Checkpoint should be allowed from Idle and Busy states
	for _, state := range []State{StateIdle, StateBusy} {
		m := newTestManager()
		m.runners["r1"] = &Runner{ID: "r1", State: state, SessionID: "sess-1"}
		// No VM → will fail, but should get past the state check
		_, err := m.CheckpointRunner(context.Background(), "r1")
		if err == nil {
			t.Errorf("Expected error (no VM) for state %s, but should pass state check", state)
			continue
		}
		// Should fail for "VM not found", not for state check
		if err.Error() != "VM not found for runner r1" {
			t.Errorf("State %s: expected VM-not-found error, got: %v", state, err)
		}
	}
}

func TestCheckpointRunner_NoVM(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle, SessionID: "sess-1"}

	_, err := m.CheckpointRunner(context.Background(), "r1")
	if err == nil {
		t.Error("CheckpointRunner should fail when VM not found")
	}
}

func TestCheckpointRunner_DoesNotBlockWithActiveExecs(t *testing.T) {
	// Unlike PauseRunner, CheckpointRunner should NOT reject when ActiveExecs > 0
	m := newTestManager()
	r := &Runner{ID: "r1", State: StateBusy, SessionID: "sess-1"}
	atomic.StoreInt32(&r.ActiveExecs, 5)
	m.runners["r1"] = r

	// Should fail for "VM not found" not "has active execs"
	_, err := m.CheckpointRunner(context.Background(), "r1")
	if err == nil {
		t.Error("Expected error (no VM)")
	}
	if err.Error() == "runner r1 has active execs, cannot pause" {
		t.Error("CheckpointRunner should NOT reject based on active execs")
	}
}
