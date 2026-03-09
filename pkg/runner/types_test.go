package runner

import (
	"encoding/json"
	"testing"
)

func TestMMDSData_JobID(t *testing.T) {
	var data MMDSData
	data.Latest.Meta.RunnerID = "runner-1"
	data.Latest.Meta.HostID = "host-1"
	data.Latest.Meta.JobID = "gh-12345"
	data.Latest.Meta.Environment = "test"
	data.Latest.Snapshot.Version = "v1"

	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("Failed to marshal MMDSData: %v", err)
	}

	var decoded MMDSData
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal MMDSData: %v", err)
	}

	if decoded.Latest.Meta.JobID != "gh-12345" {
		t.Errorf("JobID = %q, want %q", decoded.Latest.Meta.JobID, "gh-12345")
	}
}

func TestMMDSData_BackwardsCompatible(t *testing.T) {
	// Old MMDS data without JobID should still unmarshal
	oldJSON := `{"latest":{"meta":{"runner_id":"r1","host_id":"h1","environment":"prod"},"network":{"ip":"10.0.0.1","gateway":"10.0.0.254","netmask":"255.255.255.0","dns":"8.8.8.8","interface":"eth0","mac":"aa:bb:cc:dd:ee:ff"},"snapshot":{"version":"v1"}}}`

	var data MMDSData
	if err := json.Unmarshal([]byte(oldJSON), &data); err != nil {
		t.Fatalf("Failed to unmarshal old MMDS data: %v", err)
	}

	if data.Latest.Meta.RunnerID != "r1" {
		t.Errorf("RunnerID = %q, want %q", data.Latest.Meta.RunnerID, "r1")
	}
	if data.Latest.Meta.JobID != "" {
		t.Errorf("JobID should be empty for old data, got %q", data.Latest.Meta.JobID)
	}
}

func TestAllocateRequest_WorkloadKey(t *testing.T) {
	req := AllocateRequest{
		RequestID:   "req-1",
		WorkloadKey: "abc1234567890abc",
	}

	if req.WorkloadKey != "abc1234567890abc" {
		t.Errorf("WorkloadKey = %q, want %q", req.WorkloadKey, "abc1234567890abc")
	}
}

func TestRunnerStates(t *testing.T) {
	// Verify all expected states are defined
	states := []State{
		StateCold, StateBooting, StateInitializing, StateIdle,
		StateBusy, StateDraining, StateQuarantined, StateRetiring,
		StateTerminated, StatePaused,
	}

	seen := make(map[State]bool)
	for _, s := range states {
		if seen[s] {
			t.Errorf("Duplicate state: %s", s)
		}
		seen[s] = true
		if s == "" {
			t.Error("State should not be empty")
		}
	}
}
