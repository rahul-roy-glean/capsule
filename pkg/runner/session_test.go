package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

func TestPauseRunner_NoSessionID(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}

	_, err := m.PauseRunner(context.Background(), "r1")
	if err == nil {
		t.Error("PauseRunner should fail without session_id")
	}
	if m.runners["r1"].State != StateIdle {
		t.Error("State should remain idle after failed pause")
	}
}

func TestPauseRunner_NotFound(t *testing.T) {
	m := newTestManager()

	_, err := m.PauseRunner(context.Background(), "nonexistent")
	if err == nil {
		t.Error("PauseRunner should fail for nonexistent runner")
	}
}

func TestPauseRunner_AlreadySuspended(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateSuspended, SessionID: "sess-1"}

	_, err := m.PauseRunner(context.Background(), "r1")
	if err == nil {
		t.Error("PauseRunner should fail for already suspended runner")
	}
}

func TestPauseRunner_AlreadyPausing(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StatePausing, SessionID: "sess-1"}

	_, err := m.PauseRunner(context.Background(), "r1")
	if err == nil {
		t.Error("PauseRunner should fail for runner already pausing")
	}
}

func TestPauseRunner_ActiveExecs(t *testing.T) {
	m := newTestManager()
	r := &Runner{ID: "r1", State: StateIdle, SessionID: "sess-1"}
	atomic.StoreInt32(&r.ActiveExecs, 2)
	m.runners["r1"] = r

	_, err := m.PauseRunner(context.Background(), "r1")
	if err == nil {
		t.Error("PauseRunner should fail when runner has active execs")
	}
	if r.State != StateIdle {
		t.Errorf("State should remain idle, got %s", r.State)
	}
}

func TestPauseRunner_NoVM(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle, SessionID: "sess-1"}
	// No VM in m.vms

	_, err := m.PauseRunner(context.Background(), "r1")
	if err == nil {
		t.Error("PauseRunner should fail when VM not found")
	}
}

func TestActiveExecTracking(t *testing.T) {
	m := newTestManager()
	r := &Runner{ID: "r1", State: StateIdle}
	m.runners["r1"] = r

	m.IncrementActiveExecs("r1")
	if atomic.LoadInt32(&r.ActiveExecs) != 1 {
		t.Errorf("ActiveExecs = %d, want 1", r.ActiveExecs)
	}

	m.IncrementActiveExecs("r1")
	if atomic.LoadInt32(&r.ActiveExecs) != 2 {
		t.Errorf("ActiveExecs = %d, want 2", r.ActiveExecs)
	}

	m.DecrementActiveExecs("r1")
	if atomic.LoadInt32(&r.ActiveExecs) != 1 {
		t.Errorf("ActiveExecs = %d, want 1 after decrement", r.ActiveExecs)
	}

	// LastExecAt should be updated
	if r.LastExecAt.IsZero() {
		t.Error("LastExecAt should be set after decrement")
	}
}

func TestActiveExecTracking_NonexistentRunner(t *testing.T) {
	m := newTestManager()

	// Should not panic
	m.IncrementActiveExecs("nonexistent")
	m.DecrementActiveExecs("nonexistent")
}

func TestResetTTL(t *testing.T) {
	m := newTestManager()
	r := &Runner{ID: "r1", State: StateIdle}
	m.runners["r1"] = r

	m.ResetTTL("r1")
	if r.LastExecAt.IsZero() {
		t.Error("LastExecAt should be set after ResetTTL")
	}

	oldTime := r.LastExecAt
	time.Sleep(2 * time.Millisecond)
	m.ResetTTL("r1")
	if !r.LastExecAt.After(oldTime) {
		t.Error("LastExecAt should advance on second ResetTTL")
	}
}

func TestResetTTL_NonexistentRunner(t *testing.T) {
	m := newTestManager()
	// Should not panic
	m.ResetTTL("nonexistent")
}

func TestSessionMetadata_JSON(t *testing.T) {
	meta := SessionMetadata{
		SessionID:   "sess-abc",
		WorkloadKey: "chunk123",
		RunnerID:    "runner-1",
		HostID:      "host-1",
		Layers:      2,
		CreatedAt:   time.Now().Truncate(time.Second),
		PausedAt:    time.Now().Truncate(time.Second),
		RootfsPath:  "/tmp/overlay.img",
		TTLSeconds:  30,
		AutoPause:   true,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SessionMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", decoded.SessionID, "sess-abc")
	}
	if decoded.Layers != 2 {
		t.Errorf("Layers = %d, want 2", decoded.Layers)
	}
	if decoded.TTLSeconds != 30 {
		t.Errorf("TTLSeconds = %d, want 30", decoded.TTLSeconds)
	}
	if !decoded.AutoPause {
		t.Error("AutoPause should be true")
	}
	if decoded.RootfsPath != "/tmp/overlay.img" {
		t.Errorf("RootfsPath = %q, want %q", decoded.RootfsPath, "/tmp/overlay.img")
	}
}

func TestSessionMetadata_GCSFields(t *testing.T) {
	meta := SessionMetadata{
		SessionID:           "sess-gcs",
		WorkloadKey:         "wk123",
		RunnerID:            "runner-1",
		HostID:              "host-1",
		Layers:              1,
		GCSManifestPath:     "v1/wk123/runner_state/runner-1/snapshot_manifest.json",
		GCSMemIndexObject:   "v1/wk123/runner_state/runner-1/chunked-metadata.json",
		GCSDiskIndexObjects: map[string]string{"rootfs": "v1/wk123/runner_state/runner-1/disk-chunked-metadata.json"},
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SessionMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.GCSManifestPath != meta.GCSManifestPath {
		t.Errorf("GCSManifestPath = %q, want %q", decoded.GCSManifestPath, meta.GCSManifestPath)
	}
	if decoded.GCSMemIndexObject != meta.GCSMemIndexObject {
		t.Errorf("GCSMemIndexObject = %q, want %q", decoded.GCSMemIndexObject, meta.GCSMemIndexObject)
	}
	if decoded.GCSDiskIndexObjects["rootfs"] != meta.GCSDiskIndexObjects["rootfs"] {
		t.Errorf("GCSDiskIndexObjects[rootfs] = %q, want %q", decoded.GCSDiskIndexObjects["rootfs"], meta.GCSDiskIndexObjects["rootfs"])
	}
}

func TestSessionMetadata_GCSFieldsOmittedWhenEmpty(t *testing.T) {
	// Local-only session: GCS fields should be omitted from JSON
	meta := SessionMetadata{
		SessionID:   "sess-local",
		WorkloadKey: "wk456",
		RunnerID:    "runner-2",
		HostID:      "host-1",
		Layers:      1,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	jsonStr := string(data)
	if contains(jsonStr, "gcs_manifest_path") {
		t.Error("gcs_manifest_path should be omitted when empty")
	}
	if contains(jsonStr, "gcs_mem_index_object") {
		t.Error("gcs_mem_index_object should be omitted when empty")
	}
	if contains(jsonStr, "gcs_disk_index_object") {
		t.Error("gcs_disk_index_object should be omitted when empty")
	}
}

func TestSessionMetadata_BackwardsCompatibleWithGCSFields(t *testing.T) {
	// Old metadata without GCS fields should unmarshal fine
	oldJSON := `{"session_id":"s1","workload_key":"ck","runner_id":"r1","host_id":"h1","layers":1,"rootfs_path":"/tmp/x"}`

	var meta SessionMetadata
	if err := json.Unmarshal([]byte(oldJSON), &meta); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if meta.GCSManifestPath != "" {
		t.Errorf("GCSManifestPath should be empty for old metadata, got %q", meta.GCSManifestPath)
	}
	if meta.GCSMemIndexObject != "" {
		t.Errorf("GCSMemIndexObject should be empty for old metadata, got %q", meta.GCSMemIndexObject)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestSessionMetadata_BackwardsCompatible(t *testing.T) {
	// Old metadata without TTL fields should unmarshal fine
	oldJSON := `{"session_id":"s1","workload_key":"ck","runner_id":"r1","host_id":"h1","layers":1,"rootfs_path":"/tmp/x"}`

	var meta SessionMetadata
	if err := json.Unmarshal([]byte(oldJSON), &meta); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if meta.TTLSeconds != 0 {
		t.Errorf("TTLSeconds should be 0 for old metadata, got %d", meta.TTLSeconds)
	}
	if meta.AutoPause {
		t.Error("AutoPause should be false for old metadata")
	}
}

func TestSessionBaseDir_FromConfig(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = "/custom/sessions"
	})

	if got := m.sessionBaseDir(); got != "/custom/sessions" {
		t.Errorf("sessionBaseDir() = %q, want %q", got, "/custom/sessions")
	}
}

func TestSessionBaseDir_DerivedFromSnapshotCache(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.config.SnapshotCachePath = "/mnt/data/snapshots"
	})

	want := "/mnt/data/sessions"
	if got := m.sessionBaseDir(); got != want {
		t.Errorf("sessionBaseDir() = %q, want %q", got, want)
	}
}

func TestSessionBaseDir_Fallback(t *testing.T) {
	m := newTestManager()
	// No SessionDir, no SnapshotCachePath

	if got := m.sessionBaseDir(); got != defaultSessionDir {
		t.Errorf("sessionBaseDir() = %q, want %q", got, defaultSessionDir)
	}
}

func TestSessionBaseDir_ConfigTakesPrecedence(t *testing.T) {
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = "/explicit/path"
		m.config.SnapshotCachePath = "/mnt/data/snapshots"
	})

	// Explicit config should win over derived path
	if got := m.sessionBaseDir(); got != "/explicit/path" {
		t.Errorf("sessionBaseDir() = %q, want %q", got, "/explicit/path")
	}
}

func TestSessionExists(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// No session dir yet
	if m.SessionExists("sess-1") {
		t.Error("SessionExists should be false for nonexistent session")
	}

	// Create session metadata
	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", Layers: 1}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	if !m.SessionExists("sess-1") {
		t.Error("SessionExists should be true after creating metadata")
	}
}

func TestGetSessionMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Write metadata
	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{
		SessionID:   "sess-1",
		WorkloadKey: "ck123",
		RunnerID:    "runner-1",
		Layers:      3,
		TTLSeconds:  60,
		AutoPause:   true,
	}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	got, err := m.GetSessionMetadata("sess-1")
	if err != nil {
		t.Fatalf("GetSessionMetadata failed: %v", err)
	}
	if got.WorkloadKey != "ck123" {
		t.Errorf("WorkloadKey = %q, want %q", got.WorkloadKey, "ck123")
	}
	if got.Layers != 3 {
		t.Errorf("Layers = %d, want 3", got.Layers)
	}
	if got.TTLSeconds != 60 {
		t.Errorf("TTLSeconds = %d, want 60", got.TTLSeconds)
	}
}

func TestGetSessionMetadata_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	_, err := m.GetSessionMetadata("nonexistent")
	if err == nil {
		t.Error("GetSessionMetadata should fail for nonexistent session")
	}
}

func TestCleanupSession(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Create session dir with files
	sessDir := filepath.Join(tmpDir, "sess-1")
	layerDir := filepath.Join(sessDir, "layer_0")
	os.MkdirAll(layerDir, 0755)
	os.WriteFile(filepath.Join(layerDir, "mem_diff.sparse"), []byte("data"), 0644)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), []byte("{}"), 0644)

	err := m.CleanupSession("sess-1")
	if err != nil {
		t.Fatalf("CleanupSession failed: %v", err)
	}

	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Error("Session dir should be removed after cleanup")
	}
}

func TestCleanupSession_Nonexistent(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Should not error for nonexistent session
	err := m.CleanupSession("nonexistent")
	if err != nil {
		t.Errorf("CleanupSession should not error for nonexistent session: %v", err)
	}
}

func TestFindRunnerBySessionID(t *testing.T) {
	m := newTestManager()
	m.runners["r1"] = &Runner{ID: "r1", SessionID: "sess-1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", SessionID: "sess-2", State: StateSuspended}
	m.runners["r3"] = &Runner{ID: "r3", State: StateIdle} // no session

	got := m.FindRunnerBySessionID("sess-1")
	if got == nil || got.ID != "r1" {
		t.Errorf("FindRunnerBySessionID(sess-1) = %v, want r1", got)
	}

	got = m.FindRunnerBySessionID("sess-2")
	if got == nil || got.ID != "r2" {
		t.Errorf("FindRunnerBySessionID(sess-2) = %v, want r2", got)
	}

	got = m.FindRunnerBySessionID("nonexistent")
	if got != nil {
		t.Errorf("FindRunnerBySessionID(nonexistent) = %v, want nil", got)
	}
}

func TestRunnerStates_IncludesNewStates(t *testing.T) {
	// Verify new states are defined and distinct
	states := map[State]bool{
		StatePausing:   true,
		StateSuspended: true,
	}

	if !states[StatePausing] {
		t.Error("StatePausing should be defined")
	}
	if !states[StateSuspended] {
		t.Error("StateSuspended should be defined")
	}
	if StatePausing == StateSuspended {
		t.Error("StatePausing and StateSuspended should be distinct")
	}
	if StatePausing == StatePaused {
		t.Error("StatePausing and StatePaused should be distinct")
	}
}

func TestPauseResult_JSON(t *testing.T) {
	result := PauseResult{
		SessionID:         "sess-abc",
		Layer:             2,
		SnapshotSizeBytes: 1024 * 1024,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded PauseResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", decoded.SessionID, "sess-abc")
	}
	if decoded.Layer != 2 {
		t.Errorf("Layer = %d, want 2", decoded.Layer)
	}
	if decoded.SnapshotSizeBytes != 1024*1024 {
		t.Errorf("SnapshotSizeBytes = %d, want %d", decoded.SnapshotSizeBytes, 1024*1024)
	}
}

func TestResumeFromSession_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	_, err := m.ResumeFromSession(context.Background(), "nonexistent", "")
	if err == nil {
		t.Error("ResumeFromSession should fail for nonexistent session")
	}
}

func TestResumeFromSession_WorkloadKeyMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Write metadata with workload_key "abc"
	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", WorkloadKey: "abc", Layers: 1}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	_, err := m.ResumeFromSession(context.Background(), "sess-1", "xyz")
	if err == nil {
		t.Error("ResumeFromSession should fail on workload_key mismatch")
	}
}

func TestResumeFromSession_NoLayers(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", WorkloadKey: "abc", Layers: 0}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	_, err := m.ResumeFromSession(context.Background(), "sess-1", "")
	if err == nil {
		t.Error("ResumeFromSession should fail with zero layers")
	}
}

func TestResumeFromSession_Draining(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})
	m.draining = true

	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", WorkloadKey: "abc", Layers: 1}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	_, err := m.ResumeFromSession(context.Background(), "sess-1", "")
	if err == nil {
		t.Error("ResumeFromSession should fail when draining")
	}
}

func TestResumeFromSession_AtCapacity(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
		m.config.MaxRunners = 2
	})

	// Fill with active runners
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy}

	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", WorkloadKey: "abc", RunnerID: "r3", Layers: 1}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	_, err := m.ResumeFromSession(context.Background(), "sess-1", "")
	if err == nil {
		t.Error("ResumeFromSession should fail at capacity")
	}
}

func TestResumeFromSession_SuspendedNotCountedForCapacity(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
		m.config.MaxRunners = 2
	})

	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", WorkloadKey: "abc", RunnerID: "r3", Layers: 1}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	// Case 1: Two active runners → should be rejected at capacity
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle}
	m.runners["r2"] = &Runner{ID: "r2", State: StateBusy}

	_, err := m.ResumeFromSession(context.Background(), "sess-1", "")
	if err == nil {
		t.Fatal("Expected error with 2 active runners")
	}
	if !strings.Contains(err.Error(), "at capacity") {
		t.Errorf("Expected capacity error with 2 active runners, got: %v", err)
	}

	// Case 2: One active + one suspended → should pass capacity check
	// (will fail later on snapshotCache, but capacity check should pass)
	m.runners["r2"].State = StateSuspended

	func() {
		defer func() {
			if r := recover(); r != nil {
				// Expected: nil pointer on snapshotCache.GetSnapshotPaths()
				// This proves we got PAST the capacity check.
				t.Logf("Got expected panic past capacity check: %v", r)
			}
		}()
		_, err = m.ResumeFromSession(context.Background(), "sess-1", "")
		if err != nil && strings.Contains(err.Error(), "at capacity") {
			t.Error("Suspended runners should not count toward capacity")
		}
	}()
}

func TestResumeFromSession_DuplicateActiveSession(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	// Active runner already exists for this session
	m.runners["r1"] = &Runner{ID: "r1", State: StateIdle, SessionID: "sess-1"}

	sessDir := filepath.Join(tmpDir, "sess-1")
	os.MkdirAll(sessDir, 0755)
	meta := SessionMetadata{SessionID: "sess-1", WorkloadKey: "abc", RunnerID: "r1", Layers: 1}
	data, _ := json.Marshal(meta)
	os.WriteFile(filepath.Join(sessDir, "metadata.json"), data, 0644)

	_, err := m.ResumeFromSession(context.Background(), "sess-1", "")
	if err == nil {
		t.Error("ResumeFromSession should fail when session already has an active runner")
	}
}

func TestForwardResumePorts_FailsOnDebugPort(t *testing.T) {
	m := newTestManager()
	var forwarded []int
	m.forwardPortFn = func(_ string, port int) error {
		forwarded = append(forwarded, port)
		if port == snapshot.ThawAgentDebugPort {
			return errors.New("debug forward failed")
		}
		return nil
	}

	err := m.forwardResumePorts("runner-1", 8080)
	if err == nil {
		t.Fatal("forwardResumePorts() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "debug port") {
		t.Fatalf("forwardResumePorts() error = %v, want debug port context", err)
	}
	if len(forwarded) != 2 || forwarded[0] != snapshot.ThawAgentHealthPort || forwarded[1] != snapshot.ThawAgentDebugPort {
		t.Fatalf("forwarded ports = %v, want [%d %d]", forwarded, snapshot.ThawAgentHealthPort, snapshot.ThawAgentDebugPort)
	}
}

func TestForwardResumePorts_FailsOnServicePort(t *testing.T) {
	m := newTestManager()
	var forwarded []int
	m.forwardPortFn = func(_ string, port int) error {
		forwarded = append(forwarded, port)
		if port == 8080 {
			return errors.New("service forward failed")
		}
		return nil
	}

	err := m.forwardResumePorts("runner-1", 8080)
	if err == nil {
		t.Fatal("forwardResumePorts() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "service port 8080") {
		t.Fatalf("forwardResumePorts() error = %v, want service port context", err)
	}
	want := []int{snapshot.ThawAgentHealthPort, snapshot.ThawAgentDebugPort, 8080}
	if len(forwarded) != len(want) {
		t.Fatalf("forwarded ports len = %d, want %d (%v)", len(forwarded), len(want), forwarded)
	}
	for i, port := range want {
		if forwarded[i] != port {
			t.Fatalf("forwarded[%d] = %d, want %d (full=%v)", i, forwarded[i], port, forwarded)
		}
	}
}

func TestWaitForResumedRunnerReachability_UsesExecProbe(t *testing.T) {
	m := newTestManager()
	called := false
	m.waitForExecReadyFn = func(_ context.Context, ip string, timeout time.Duration) error {
		called = true
		if ip != "10.200.1.2" {
			t.Fatalf("ip = %q, want %q", ip, "10.200.1.2")
		}
		if timeout != 30*time.Second {
			t.Fatalf("timeout = %s, want %s", timeout, 30*time.Second)
		}
		return nil
	}

	if err := m.waitForResumedRunnerReachability(context.Background(), "10.200.1.2", 30*time.Second); err != nil {
		t.Fatalf("waitForResumedRunnerReachability() error = %v", err)
	}
	if !called {
		t.Fatal("waitForResumedRunnerReachability() did not call exec readiness probe")
	}
}

func TestActiveExecTracking_Concurrent(t *testing.T) {
	m := newTestManager()
	r := &Runner{ID: "r1", State: StateIdle}
	m.runners["r1"] = r

	done := make(chan struct{})
	// 100 concurrent increments
	for i := 0; i < 100; i++ {
		go func() {
			m.IncrementActiveExecs("r1")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}

	if got := atomic.LoadInt32(&r.ActiveExecs); got != 100 {
		t.Errorf("ActiveExecs = %d, want 100 after concurrent increments", got)
	}

	// 100 concurrent decrements
	for i := 0; i < 100; i++ {
		go func() {
			m.DecrementActiveExecs("r1")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}

	if got := atomic.LoadInt32(&r.ActiveExecs); got != 0 {
		t.Errorf("ActiveExecs = %d, want 0 after concurrent decrements", got)
	}
}
