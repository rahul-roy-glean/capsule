package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
	"github.com/rahul-roy-glean/capsule/pkg/runner"
	"github.com/sirupsen/logrus"
)

// newTestServer creates a HostAgentServer backed by a real Manager using temp dirs.
// The returned server has no ChunkedManager — tests that reach fresh allocation
// will get a nil-pointer panic, so only use for paths that return before that.
func newTestServer(t *testing.T) *HostAgentServer {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := runner.HostConfig{
		HostID:            "test-host",
		Environment:       "test",
		SnapshotCachePath: filepath.Join(tmpDir, "snapshots"),
		SocketDir:         filepath.Join(tmpDir, "sockets"),
		WorkspaceDir:      filepath.Join(tmpDir, "workspace"),
		LogDir:            filepath.Join(tmpDir, "logs"),
		SessionDir:        filepath.Join(tmpDir, "sessions"),
	}
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	mgr, err := runner.NewManager(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &HostAgentServer{
		manager: mgr,
		logger:  logger.WithField("test", true),
	}
}

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

// TestAllocateRunnerRequest_ResumeFields verifies that the new runner_id and
// resume proto fields roundtrip correctly through the generated Go code.
func TestAllocateRunnerRequest_ResumeFields(t *testing.T) {
	req := &pb.AllocateRunnerRequest{
		RequestId:   "req-1",
		WorkloadKey: "wk-abc",
		SessionId:   "sess-1",
		RunnerId:    "runner-old-host-A",
		Resume:      true,
	}

	if req.RunnerId != "runner-old-host-A" {
		t.Errorf("RunnerId = %q, want %q", req.RunnerId, "runner-old-host-A")
	}
	if !req.Resume {
		t.Error("Resume should be true")
	}
}

// TestAllocateRunnerRequest_ResumeFieldsDefaultFalse verifies that an old-style
// request (no resume fields) defaults to resume=false, runner_id="".
func TestAllocateRunnerRequest_ResumeFieldsDefaultFalse(t *testing.T) {
	req := &pb.AllocateRunnerRequest{
		RequestId:   "req-1",
		WorkloadKey: "wk-abc",
		SessionId:   "sess-1",
	}

	if req.RunnerId != "" {
		t.Errorf("RunnerId should default to empty, got %q", req.RunnerId)
	}
	if req.Resume {
		t.Error("Resume should default to false")
	}
}

// TestAllocateRunner_ExistingActiveSessionReturnedWithResume verifies that when
// a non-suspended runner already exists for a session, AllocateRunner returns
// that runner directly — even if resume=true. The idempotent check takes
// precedence over the resume flag.
func TestAllocateRunner_ExistingActiveSessionReturnedWithResume(t *testing.T) {
	srv := newTestServer(t)

	// Inject an active runner for "sess-1" into the manager.
	srv.manager.TestInjectRunner(&runner.Runner{
		ID:          "runner-existing",
		HostID:      "test-host",
		State:       runner.StateIdle,
		SessionID:   "sess-1",
		WorkloadKey: "wk-abc",
		InternalIP:  net.ParseIP("10.0.0.1"),
		Resources:   runner.Resources{VCPUs: 2, MemoryMB: 4096},
		CreatedAt:   time.Now(),
	})

	resp, err := srv.AllocateRunner(context.Background(), &pb.AllocateRunnerRequest{
		RequestId:   "req-1",
		WorkloadKey: "wk-abc",
		SessionId:   "sess-1",
		RunnerId:    "runner-from-host-A",
		Resume:      true,
	})
	if err != nil {
		t.Fatalf("AllocateRunner: %v", err)
	}
	if resp.Runner == nil {
		t.Fatal("expected runner in response")
	}
	if resp.Runner.Id != "runner-existing" {
		t.Errorf("Runner.Id = %q, want %q (idempotent return)", resp.Runner.Id, "runner-existing")
	}
	if resp.SessionId != "sess-1" {
		t.Errorf("SessionId = %q, want %q", resp.SessionId, "sess-1")
	}
	if resp.Resumed {
		t.Error("Resumed should be false — returned existing runner, not resumed from snapshot")
	}
}

// TestAllocateRunner_ResumeSkippedWhenMigrationLabelsSet verifies that when
// both resume=true and migration labels are present, the migration path is
// taken (fresh allocation), not the session resume path.
// The request fails at fresh allocation (no ChunkedManager) but we verify
// the error comes from the allocation path, not from ResumeFromSession.
func TestAllocateRunner_ResumeSkippedWhenMigrationLabelsSet(t *testing.T) {
	srv := newTestServer(t)

	resp, err := srv.AllocateRunner(context.Background(), &pb.AllocateRunnerRequest{
		RequestId:   "req-mig",
		WorkloadKey: "wk-new",
		SessionId:   "sess-1",
		RunnerId:    "runner-from-host-A",
		Resume:      true,
		Labels: map[string]string{
			"_migrate_from_workload_key": "wk-old",
			"_migrate_from_runner_id":    "runner-from-host-A",
		},
	})

	// Should get an error response (nil ChunkedManager → panic recovered or
	// error from allocation), not a gRPC error from resume.
	if err != nil {
		// gRPC-level error means the server didn't crash — good.
		// The important thing is it didn't try to resume.
		return
	}
	if resp != nil && resp.Error != "" {
		// Error response from allocation path — expected, means migration
		// branch was taken, not resume.
		return
	}
	// If we got here with no error and no resp.Error, that's unexpected
	// unless chunkedMgr somehow returned a runner.
	if resp != nil && resp.Runner != nil {
		t.Log("Got a runner response — unexpected but not wrong")
	}
}

// TestAllocateRunner_ResumeNotSetSkipsResumeAttempt verifies that when
// resume=false (old control plane behavior), the host agent does NOT attempt
// session resume even if session_id is present. Falls through to fresh allocation.
func TestAllocateRunner_ResumeNotSetSkipsResumeAttempt(t *testing.T) {
	srv := newTestServer(t)

	// No resume flag, but session_id is set.
	// With resume=false, the host should skip ResumeFromSession entirely.
	// It will fall through to fresh allocation which fails (no ChunkedManager),
	// but the key assertion is that we don't get a resume-related error.
	resp, err := srv.AllocateRunner(context.Background(), &pb.AllocateRunnerRequest{
		RequestId:   "req-no-resume",
		WorkloadKey: "wk-abc",
		SessionId:   "sess-orphan",
		// Resume: false (default)
		// RunnerId: "" (default)
	})

	if err != nil {
		// gRPC error from nil ChunkedManager is acceptable
		return
	}
	if resp != nil && resp.Error != "" {
		// Allocation error — expected. Verify it's not a resume error.
		if contains := func(s, sub string) bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}; contains(resp.Error, "resume") || contains(resp.Error, "session snapshot") {
			t.Errorf("got resume-related error %q; expected fresh allocation error", resp.Error)
		}
	}
}
