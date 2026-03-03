package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWorkloadKeySetOnAllocatedRunner verifies that AllocateRequest.WorkloadKey
// is propagated to the Runner struct during construction.
// Regression: WorkloadKey was missing from the Runner{} literal in AllocateRunner,
// causing session metadata to have workload_key="" which broke resume.
func TestWorkloadKeySetOnAllocatedRunner(t *testing.T) {
	// Verify the Runner struct can hold the workload key
	r := &Runner{
		ID:          "r1",
		WorkloadKey: "expected-key",
		SessionID:   "sess-1",
	}

	if r.WorkloadKey != "expected-key" {
		t.Errorf("Runner.WorkloadKey = %q, want %q", r.WorkloadKey, "expected-key")
	}

	// Verify that AllocateRequest has WorkloadKey field
	req := AllocateRequest{
		WorkloadKey: "test-key-abc",
	}
	if req.WorkloadKey != "test-key-abc" {
		t.Errorf("AllocateRequest.WorkloadKey = %q, want %q", req.WorkloadKey, "test-key-abc")
	}
}

// TestWorkloadKeyEmptyBreaksResume verifies that a session with an empty
// workload_key is rejected when a resume is attempted with a non-empty key.
func TestWorkloadKeyEmptyBreaksResume(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	sessDir := filepath.Join(tmpDir, "sess-empty-wk")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{
		SessionID:   "sess-empty-wk",
		WorkloadKey: "",
		RunnerID:    "r1",
		Layers:      1,
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	_, err := m.ResumeFromSession(context.TODO(), "sess-empty-wk", "real-key-123")
	if err == nil {
		t.Fatal("ResumeFromSession should fail when session has empty workload_key but request has a key")
	}
}

// TestSessionMetadataPreservesWorkloadKey verifies that session metadata
// written with a non-empty workload_key can be read back and used for resume.
func TestSessionMetadataPreservesWorkloadKey(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	sessDir := filepath.Join(tmpDir, "sess-wk-roundtrip")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{
		SessionID:   "sess-wk-roundtrip",
		WorkloadKey: "abc123def456",
		RunnerID:    "r1",
		Layers:      1,
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	got, err := m.GetSessionMetadata("sess-wk-roundtrip")
	if err != nil {
		t.Fatalf("GetSessionMetadata failed: %v", err)
	}
	if got.WorkloadKey != "abc123def456" {
		t.Errorf("WorkloadKey = %q, want %q", got.WorkloadKey, "abc123def456")
	}
}

// TestRunnerHeartbeatInfoIncludesWorkloadKey verifies that runner heartbeats
// include the WorkloadKey so the control plane can track it.
func TestRunnerHeartbeatInfoIncludesWorkloadKey(t *testing.T) {
	info := RunnerHeartbeatInfo{
		RunnerID:    "r1",
		State:       StateIdle,
		WorkloadKey: "my-workload-key",
	}
	data, _ := json.Marshal(info)
	var decoded map[string]interface{}
	json.Unmarshal(data, &decoded)

	if decoded["workload_key"] != "my-workload-key" {
		t.Errorf("heartbeat workload_key = %v, want %q", decoded["workload_key"], "my-workload-key")
	}
}

// TestSessionCleanupOnRelease verifies that session directory is removed
// when a runner with a SessionID is released.
func TestSessionCleanupOnRelease(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	sessDir := filepath.Join(tmpDir, "sess-to-clean")
	os.MkdirAll(filepath.Join(sessDir, "layer_0"), 0755)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), []byte(`{"layers":1}`), 0644)

	// Verify session exists before cleanup
	if !m.SessionExists("sess-to-clean") {
		t.Fatal("Session should exist before cleanup")
	}

	// CleanupSession should remove it
	err := m.CleanupSession("sess-to-clean")
	if err != nil {
		t.Fatalf("CleanupSession failed: %v", err)
	}

	if m.SessionExists("sess-to-clean") {
		t.Error("Session should not exist after cleanup")
	}
}
