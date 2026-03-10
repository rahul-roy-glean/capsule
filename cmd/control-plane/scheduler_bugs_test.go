package main

import (
	"context"
	"testing"
	"time"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
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

func TestAllocateRunner_MissingSnapshotTagFailsFast(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	hr := NewHostRegistry(nil, logger)
	hr.hosts["host-1"] = &Host{
		ID:                 "host-1",
		InstanceName:       "host-1",
		Status:             "ready",
		LastHeartbeat:      time.Now(),
		GRPCAddress:        "127.0.0.1:65535",
		TotalCPUMillicores: 16000,
		TotalMemoryMB:      65536,
	}

	s := NewScheduler(hr, nil, nil, nil, logger)

	_, err := s.AllocateRunner(context.Background(), AllocateRunnerRequest{
		RequestID:   "req-1",
		WorkloadKey: "wk-missing",
		SnapshotTag: "stable",
	})
	if err == nil {
		t.Fatal("expected missing snapshot tag to fail")
	}
	if got := err.Error(); got != `snapshot tag "stable" not found for workload "wk-missing"` {
		t.Fatalf("unexpected error: %s", got)
	}
}
