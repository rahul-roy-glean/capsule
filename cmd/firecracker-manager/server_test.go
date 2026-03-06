package main

import (
	"net"
	"testing"
	"time"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/runner"
)

func TestRunnerToProto_AllStates(t *testing.T) {
	tests := []struct {
		state     runner.State
		wantState string // proto enum name for comparison
	}{
		{runner.StateCold, "RUNNER_STATE_COLD"},
		{runner.StateBooting, "RUNNER_STATE_BOOTING"},
		{runner.StateInitializing, "RUNNER_STATE_INITIALIZING"},
		{runner.StateIdle, "RUNNER_STATE_IDLE"},
		{runner.StateBusy, "RUNNER_STATE_BUSY"},
		{runner.StateDraining, "RUNNER_STATE_DRAINING"},
		{runner.StateQuarantined, "RUNNER_STATE_QUARANTINED"},
		{runner.StateRetiring, "RUNNER_STATE_RETIRING"},
		{runner.StateTerminated, "RUNNER_STATE_TERMINATED"},
		{runner.StatePaused, "RUNNER_STATE_PAUSED"},
		{runner.StatePausing, "RUNNER_STATE_PAUSING"},
		{runner.StateSuspended, "RUNNER_STATE_SUSPENDED"},
		{runner.State("unknown"), "RUNNER_STATE_UNSPECIFIED"},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			r := &runner.Runner{
				ID:         "test-id",
				HostID:     "test-host",
				State:      tt.state,
				InternalIP: net.ParseIP("172.16.0.2"),
				Resources:  runner.Resources{VCPUs: 4, MemoryMB: 8192},
				CreatedAt:  time.Now(),
			}

			proto := runnerToProto(r)
			if proto.State.String() != tt.wantState {
				t.Errorf("runnerToProto(%q).State = %q, want %q",
					tt.state, proto.State.String(), tt.wantState)
			}
		})
	}
}

func TestRunnerToProto_Resources(t *testing.T) {
	r := &runner.Runner{
		ID:         "test-id",
		HostID:     "host-1",
		State:      runner.StateIdle,
		InternalIP: net.ParseIP("10.0.0.1"),
		Resources:  runner.Resources{VCPUs: 8, MemoryMB: 16384, DiskGB: 100},
		CreatedAt:  time.Now(),
	}

	proto := runnerToProto(r)

	if proto.Resources == nil {
		t.Fatal("Resources is nil")
	}
	if proto.Resources.Vcpus != 8 {
		t.Errorf("Vcpus = %d, want 8", proto.Resources.Vcpus)
	}
	if proto.Resources.MemoryMb != 16384 {
		t.Errorf("MemoryMb = %d, want 16384", proto.Resources.MemoryMb)
	}
	if proto.Resources.DiskGb != 100 {
		t.Errorf("DiskGb = %d, want 100", proto.Resources.DiskGb)
	}
}

func TestRunnerToProto_Fields(t *testing.T) {
	now := time.Now()
	started := now.Add(-5 * time.Minute)
	r := &runner.Runner{
		ID:              "runner-abc",
		HostID:          "host-xyz",
		State:           runner.StateBusy,
		InternalIP:      net.ParseIP("172.16.0.5"),
		JobID:           "job-456",
		SnapshotVersion: "v2.0",
		Resources:       runner.Resources{VCPUs: 4, MemoryMB: 8192},
		CreatedAt:       now,
		StartedAt:       started,
	}

	proto := runnerToProto(r)

	if proto.Id != "runner-abc" {
		t.Errorf("Id = %q, want %q", proto.Id, "runner-abc")
	}
	if proto.HostId != "host-xyz" {
		t.Errorf("HostId = %q, want %q", proto.HostId, "host-xyz")
	}
	if proto.InternalIp != "172.16.0.5" {
		t.Errorf("InternalIp = %q, want %q", proto.InternalIp, "172.16.0.5")
	}
	if proto.JobId != "job-456" {
		t.Errorf("JobId = %q, want %q", proto.JobId, "job-456")
	}
	if proto.SnapshotVersion != "v2.0" {
		t.Errorf("SnapshotVersion = %q, want %q", proto.SnapshotVersion, "v2.0")
	}
	if proto.CreatedAt == nil {
		t.Error("CreatedAt is nil")
	}
	if proto.StartedAt == nil {
		t.Error("StartedAt should be set for non-zero time")
	}
}

func TestRunnerToProto_TimestampFields(t *testing.T) {
	// Test that zero StartedAt doesn't produce a timestamp
	r := &runner.Runner{
		ID:         "test-id",
		HostID:     "test-host",
		State:      runner.StateBooting,
		InternalIP: net.ParseIP("10.0.0.1"),
		Resources:  runner.Resources{VCPUs: 2, MemoryMB: 4096},
		CreatedAt:  time.Now(),
		// StartedAt is zero value
	}

	proto := runnerToProto(r)

	if proto.CreatedAt == nil {
		t.Error("CreatedAt should always be set")
	}
	if proto.StartedAt != nil {
		t.Error("StartedAt should be nil for zero time")
	}
}

func TestRunnerToProto_NilIP(t *testing.T) {
	// Suspended runners have nil InternalIP — should not panic
	r := &runner.Runner{
		ID:        "test-id",
		HostID:    "test-host",
		State:     runner.StateSuspended,
		Resources: runner.Resources{VCPUs: 2, MemoryMB: 4096},
		CreatedAt: time.Now(),
		// InternalIP is nil
	}

	proto := runnerToProto(r)

	if proto.InternalIp != "" {
		t.Errorf("InternalIp should be empty for nil IP, got %q", proto.InternalIp)
	}
	if proto.State.String() != "RUNNER_STATE_SUSPENDED" {
		t.Errorf("State = %q, want RUNNER_STATE_SUSPENDED", proto.State.String())
	}
}
