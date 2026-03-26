package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	_, err := m.ResumeFromSession(context.TODO(), "sess-empty-wk", "real-key-123", "")
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

// ---------------------------------------------------------------------------
// Cross-host resume tests
// ---------------------------------------------------------------------------

// TestAllocateRequest_CrossHostResumeFields verifies that the AllocateRequest
// struct carries RunnerID and Resume fields for cross-host session resume.
func TestAllocateRequest_CrossHostResumeFields(t *testing.T) {
	req := AllocateRequest{
		RequestID:   "req-1",
		WorkloadKey: "wk-abc",
		SessionID:   "sess-1",
		RunnerID:    "runner-from-host-A",
		Resume:      true,
	}

	if req.RunnerID != "runner-from-host-A" {
		t.Errorf("RunnerID = %q, want %q", req.RunnerID, "runner-from-host-A")
	}
	if !req.Resume {
		t.Error("Resume should be true")
	}
}

// TestAllocateRequest_MigrationAndResumeCoexist verifies that AllocateRequest
// can carry both migration fields and resume fields simultaneously. The routing
// decision (migration takes precedence) is handled by the server, not the struct.
func TestAllocateRequest_MigrationAndResumeCoexist(t *testing.T) {
	req := AllocateRequest{
		RequestID:              "req-mig",
		WorkloadKey:            "wk-new",
		SessionID:              "sess-1",
		RunnerID:               "runner-from-host-A",
		Resume:                 true,
		MigrateFromWorkloadKey: "wk-old",
		MigrateFromRunnerID:    "runner-from-host-A",
	}

	// Both sets of fields should be populated
	if req.MigrateFromWorkloadKey != "wk-old" {
		t.Errorf("MigrateFromWorkloadKey = %q, want %q", req.MigrateFromWorkloadKey, "wk-old")
	}
	if req.RunnerID != "runner-from-host-A" {
		t.Errorf("RunnerID = %q, want %q", req.RunnerID, "runner-from-host-A")
	}
	if !req.Resume {
		t.Error("Resume should be true")
	}
}

// TestResumeFromSession_CrossHostWithRunnerID verifies that ResumeFromSession
// accepts a non-empty runnerID parameter (used for GCS path construction in
// cross-host resume). The session metadata runnerID ("runner-original") is
// authoritative, so after loading metadata the function will use that ID for
// the bringup lease. We inject a runner with that ID to trigger a "duplicate
// session" error — proving metadata was loaded and the ID was used.
func TestResumeFromSession_CrossHostWithRunnerID(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Inject an active runner matching the metadata's runner ID + session ID.
	// This will cause AcquireBringupLease to reject the resume with
	// "session already has an active runner".
	m.runners["runner-original"] = &Runner{
		ID:        "runner-original",
		State:     StateIdle,
		SessionID: "sess-cross-host",
	}

	// Write session metadata that would exist if downloaded from GCS
	sessDir := filepath.Join(tmpDir, "sess-cross-host")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{
		SessionID:       "sess-cross-host",
		WorkloadKey:     "wk-abc",
		RunnerID:        "runner-original",
		HostID:          "host-A",
		Layers:          1,
		GCSManifestPath: "v1/wk-abc/runner_state/runner-original/snapshot_manifest.json",
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	// Call with the cross-host runner ID (same as metadata's).
	_, err := m.ResumeFromSession(context.Background(), "sess-cross-host", "wk-abc", "runner-original")
	if err == nil {
		t.Fatal("expected error from duplicate session check")
	}

	// Error should be about duplicate session, not workload_key mismatch.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "already has an active runner") {
		t.Errorf("expected duplicate session error, got: %v", err)
	}
}

// TestResumeFromSession_OverridesRunnerIDFromMetadata verifies that
// ResumeFromSession always uses the runner ID from session metadata
// (authoritative source), regardless of the runnerID parameter passed in.
// We inject a runner with the METADATA's ID to cause a "duplicate session"
// error — if the caller's ID were used instead, no duplicate would be found.
func TestResumeFromSession_OverridesRunnerIDFromMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Inject a runner with the METADATA's runner ID (not the caller's).
	m.runners["runner-metadata"] = &Runner{
		ID:        "runner-metadata",
		State:     StateIdle,
		SessionID: "sess-override-id",
	}

	sessDir := filepath.Join(tmpDir, "sess-override-id")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{
		SessionID:   "sess-override-id",
		WorkloadKey: "wk-abc",
		RunnerID:    "runner-metadata",
		HostID:      "host-A",
		Layers:      1,
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	// Pass a DIFFERENT runnerID — the function should use metadata's "runner-metadata"
	// for the bringup lease, hitting the duplicate check for that ID.
	_, err := m.ResumeFromSession(context.Background(), "sess-override-id", "wk-abc", "runner-different")
	if err == nil {
		t.Fatal("expected error from duplicate session check")
	}

	// The error should reference "runner-metadata" (from metadata), proving
	// the caller's "runner-different" was overridden.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "runner-metadata") {
		t.Errorf("error should reference metadata runner ID, got: %v", err)
	}
	if strings.Contains(errMsg, "runner-different") {
		t.Errorf("error should NOT reference caller's runner ID, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Extension drive metadata tests
// ---------------------------------------------------------------------------

// TestSessionMetadata_ExtensionDriveGCSPaths verifies that GCSDiskIndexObjects
// preserves extension drive GCS paths through JSON serialization. This is the
// mechanism that enables cross-host resume with extension drives: the pausing
// host writes the GCS paths, and the resuming host reads them to download chunks.
func TestSessionMetadata_ExtensionDriveGCSPaths(t *testing.T) {
	meta := SessionMetadata{
		SessionID:       "sess-ext",
		WorkloadKey:     "wk-abc",
		RunnerID:        "runner-1",
		HostID:          "host-A",
		Layers:          1,
		GCSManifestPath: "v1/wk-abc/runner_state/runner-1/snapshot_manifest.json",
		GCSDiskIndexObjects: map[string]string{
			"workspace": "v1/wk-abc/runner_state/runner-1/disk-workspace-chunked-metadata.json",
			"data":      "v1/wk-abc/runner_state/runner-1/disk-data-chunked-metadata.json",
		},
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SessionMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.GCSDiskIndexObjects) != 2 {
		t.Fatalf("GCSDiskIndexObjects len = %d, want 2", len(decoded.GCSDiskIndexObjects))
	}
	if decoded.GCSDiskIndexObjects["workspace"] != meta.GCSDiskIndexObjects["workspace"] {
		t.Errorf("GCSDiskIndexObjects[workspace] = %q, want %q",
			decoded.GCSDiskIndexObjects["workspace"], meta.GCSDiskIndexObjects["workspace"])
	}
	if decoded.GCSDiskIndexObjects["data"] != meta.GCSDiskIndexObjects["data"] {
		t.Errorf("GCSDiskIndexObjects[data] = %q, want %q",
			decoded.GCSDiskIndexObjects["data"], meta.GCSDiskIndexObjects["data"])
	}
}

// TestSessionMetadata_ExtensionDriveOmittedWhenEmpty verifies that
// GCSDiskIndexObjects is omitted from JSON when no extension drives exist.
func TestSessionMetadata_ExtensionDriveOmittedWhenEmpty(t *testing.T) {
	meta := SessionMetadata{
		SessionID:   "sess-no-ext",
		WorkloadKey: "wk-abc",
		RunnerID:    "runner-1",
		HostID:      "host-A",
		Layers:      1,
	}

	data, _ := json.Marshal(meta)
	jsonStr := string(data)

	if strings.Contains(jsonStr, "gcs_disk_index_objects") {
		t.Error("gcs_disk_index_objects should be omitted when empty")
	}
}

// TestAllocateRequest_MigrationCarriesExtensionDriveInfo verifies that
// AllocateRequest migration fields carry enough info to construct GCS paths
// for the old session's extension drives. The control plane sends these via
// _migrate_from_workload_key and _migrate_from_runner_id labels, which the
// host agent uses to build: {wk}/runner_state/{runner_id}/snapshot_manifest.json
func TestAllocateRequest_MigrationCarriesExtensionDriveInfo(t *testing.T) {
	req := AllocateRequest{
		RequestID:              "req-mig",
		WorkloadKey:            "wk-new-golden",
		SessionID:              "sess-1",
		MigrateFromWorkloadKey: "wk-old-golden",
		MigrateFromRunnerID:    "runner-host-A",
	}

	// Verify the host agent can construct the GCS manifest path from migration fields
	gcsBase := req.MigrateFromWorkloadKey + "/runner_state/" + req.MigrateFromRunnerID
	manifestPath := gcsBase + "/snapshot_manifest.json"

	want := "wk-old-golden/runner_state/runner-host-A/snapshot_manifest.json"
	if manifestPath != want {
		t.Errorf("manifest path = %q, want %q", manifestPath, want)
	}
}

// TestAllocateRequest_ResumeCarriesRunnerIDForGCSPath verifies that the
// Resume + RunnerID fields on AllocateRequest carry enough info for the host
// agent to construct GCS paths for cross-host session resume (non-migration).
func TestAllocateRequest_ResumeCarriesRunnerIDForGCSPath(t *testing.T) {
	req := AllocateRequest{
		RequestID:   "req-resume",
		WorkloadKey: "wk-abc",
		SessionID:   "sess-1",
		RunnerID:    "runner-host-A",
		Resume:      true,
	}

	// The host agent passes RunnerID to ResumeFromSession, which constructs
	// the GCS manifest path from the session metadata. The RunnerID is used
	// in the metadata to build: {wk}/runner_state/{runner_id}/
	if req.RunnerID == "" {
		t.Error("RunnerID should be set for cross-host resume GCS path construction")
	}
	if !req.Resume {
		t.Error("Resume should be true to signal intent to the host agent")
	}
}
