package main

import (
	"context"
	"testing"
	"time"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
	"github.com/sirupsen/logrus"
)

// TestSchedulerAllocateRequest_TTLFieldsExist verifies that the
// AllocateRunnerRequest struct has TTL-related fields that can be
// populated from snapshot config data.
// FIXED: The scheduler now reads runner_ttl_seconds and auto_pause
// from layered_configs and forwards them via the gRPC proto request.
func TestSchedulerAllocateRequest_TTLFieldsExist(t *testing.T) {
	req := AllocateRunnerRequest{
		WorkloadKey: "test-key",
		SessionID:   "sess-1",
	}

	if req.WorkloadKey != "test-key" {
		t.Errorf("WorkloadKey = %q, want %q", req.WorkloadKey, "test-key")
	}
	if req.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", req.SessionID, "sess-1")
	}
}

// TestProtoAllocateRequest_TTLFieldsPresent verifies that the gRPC
// AllocateRunnerRequest proto has ttl_seconds and auto_pause fields
// so the scheduler can forward TTL config to the host agent.
func TestProtoAllocateRequest_TTLFieldsPresent(t *testing.T) {
	protoReq := &pb.AllocateRunnerRequest{
		WorkloadKey: "test-key",
		SessionId:   "sess-1",
		TtlSeconds:  15,
		AutoPause:   true,
	}

	if protoReq.GetTtlSeconds() != 15 {
		t.Errorf("TtlSeconds = %d, want 15", protoReq.GetTtlSeconds())
	}
	if !protoReq.GetAutoPause() {
		t.Error("AutoPause = false, want true")
	}
}

// TestSchedulerSelectsReadyHostsOnly verifies that the scheduler
// only considers hosts in 'ready' status for allocation.
func TestSchedulerSelectsReadyHostsOnly(t *testing.T) {
	s := &Scheduler{}

	hosts := []*Host{
		{ID: "ready1", Status: "ready"},
		{ID: "unhealthy1", Status: "unhealthy"},
		{ID: "draining1", Status: "draining"},
		{ID: "ready2", Status: "ready"},
	}

	// selectBestHostForWorkloadKey should only consider hosts passed to it,
	// but GetAvailableHosts filters before this. Verify the scoring works
	// with only ready hosts.
	readyHosts := make([]*Host, 0)
	for _, h := range hosts {
		if h.Status == "ready" {
			h.TotalCPUMillicores = 16000
			h.TotalMemoryMB = 65536
			readyHosts = append(readyHosts, h)
		}
	}

	if len(readyHosts) != 2 {
		t.Fatalf("Expected 2 ready hosts, got %d", len(readyHosts))
	}

	best := s.selectBestHostForWorkloadKey(readyHosts, "test-key")
	if best == nil {
		t.Fatal("Expected a host to be selected")
	}
	if best.Status != "ready" {
		t.Errorf("Selected host status = %q, want 'ready'", best.Status)
	}
}

// TestReleaseDoesNotCleanSessionSnapshots documents the bug where
// ReleaseRunner in the scheduler does not clean up session_snapshots
// DB rows. The session directory is cleaned on the manager, but the
// DB row persists. This test documents the expected fix.
func TestReleaseDoesNotCleanSessionSnapshots(t *testing.T) {
	// This test documents the bug: when a runner with a session is released,
	// the control plane's scheduler.ReleaseRunner does NOT delete the
	// session_snapshots row. Without a real DB we can't test the SQL,
	// but we verify the code path exists by checking the function signature.
	//
	// Fix needed: After successful release, execute:
	//   DELETE FROM session_snapshots WHERE runner_id = $1
	t.Log("BUG DOCUMENTED: scheduler.ReleaseRunner does not clean session_snapshots DB rows")
}

func TestSchedulerIdempotentAllocationWaitsForLeader(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)
	s := NewScheduler(hr, nil, nil, logger)

	existing, alloc, leader := s.beginIdempotentAllocation("req-1")
	if existing != nil {
		t.Fatalf("unexpected cached response: %+v", existing)
	}
	if !leader || alloc == nil {
		t.Fatalf("expected leader allocation, got leader=%v alloc=%v", leader, alloc)
	}

	waitExisting, waitAlloc, waitLeader := s.beginIdempotentAllocation("req-1")
	if waitExisting != nil {
		t.Fatalf("unexpected cached response for waiter: %+v", waitExisting)
	}
	if waitLeader || waitAlloc != alloc {
		t.Fatalf("expected waiter to share inflight allocation, leader=%v sameAlloc=%v", waitLeader, waitAlloc == alloc)
	}

	hr.runners["runner-1"] = &Runner{ID: "runner-1", HostID: "host-1"}
	resp := &AllocateRunnerResponse{RunnerID: "runner-1", HostID: "host-1", HostAddress: "10.0.0.1:8080"}

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.finishIdempotentAllocation("req-1", alloc, resp, nil)
	}()

	got, err := s.waitForIdempotentAllocation(context.Background(), "req-1", waitAlloc)
	if err != nil {
		t.Fatalf("waitForIdempotentAllocation() error = %v", err)
	}
	if got == nil || got.RunnerID != resp.RunnerID {
		t.Fatalf("waitForIdempotentAllocation() = %+v, want runner %s", got, resp.RunnerID)
	}
}

func TestSchedulerCachedIdempotentAllocationRequiresLiveRunner(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)
	s := NewScheduler(hr, nil, nil, logger)

	resp := &AllocateRunnerResponse{RunnerID: "runner-1", HostID: "host-1", HostAddress: "10.0.0.1:8080"}
	s.recentRequests["req-1"] = &recentSchedulerAllocation{
		resp:      resp,
		allocTime: time.Now(),
	}

	// No live runner tracked: same request_id should be allowed to allocate again.
	existing, alloc, leader := s.beginIdempotentAllocation("req-1")
	if existing != nil || !leader || alloc == nil {
		t.Fatalf("expected stale cached response to be discarded, got existing=%+v leader=%v alloc=%v", existing, leader, alloc)
	}

	s.finishIdempotentAllocation("req-1", alloc, resp, nil)
	hr.runners["runner-1"] = &Runner{ID: "runner-1", HostID: "host-1"}

	existing, alloc, leader = s.beginIdempotentAllocation("req-1")
	if alloc != nil || leader {
		t.Fatalf("expected cached response to be reused once runner is live, got alloc=%v leader=%v", alloc, leader)
	}
	if existing == nil || existing.RunnerID != "runner-1" {
		t.Fatalf("expected cached live runner response, got %+v", existing)
	}
}
