package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestForkSession_CreatesForkedMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	sourceDir := filepath.Join(tmpDir, "sess-source")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatalf("MkdirAll sourceDir: %v", err)
	}
	source := SessionMetadata{
		SessionID:       "sess-source",
		WorkloadKey:     "wk123",
		RunnerID:        "runner-source",
		HostID:          "host-1",
		Layers:          3,
		GCSManifestPath: "v1/wk123/runner_state/runner-source/snapshot_manifest.json",
	}
	data, _ := json.Marshal(source)
	if err := os.WriteFile(filepath.Join(sourceDir, "metadata.json"), data, 0644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	forked, err := m.ForkSession("sess-source", "sess-fork", "runner-fork", "runner-live")
	if err != nil {
		t.Fatalf("ForkSession failed: %v", err)
	}
	if forked.SessionID != "sess-fork" {
		t.Fatalf("SessionID = %q, want %q", forked.SessionID, "sess-fork")
	}
	if forked.ParentSessionID != "sess-source" {
		t.Fatalf("ParentSessionID = %q, want %q", forked.ParentSessionID, "sess-source")
	}
	if forked.RunnerID != "runner-fork" {
		t.Fatalf("RunnerID = %q, want %q", forked.RunnerID, "runner-fork")
	}
	if forked.ForkedFromRunnerID != "runner-live" {
		t.Fatalf("ForkedFromRunnerID = %q, want %q", forked.ForkedFromRunnerID, "runner-live")
	}

	onDisk, err := m.GetSessionMetadata("sess-fork")
	if err != nil {
		t.Fatalf("GetSessionMetadata(fork): %v", err)
	}
	if onDisk.ParentSessionID != "sess-source" {
		t.Fatalf("disk ParentSessionID = %q, want %q", onDisk.ParentSessionID, "sess-source")
	}
	if onDisk.GCSManifestPath != source.GCSManifestPath {
		t.Fatalf("disk GCSManifestPath = %q, want %q", onDisk.GCSManifestPath, source.GCSManifestPath)
	}
}

func TestForkSession_RejectsLocalOnlySessions(t *testing.T) {
	tmpDir := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = tmpDir
	})

	sourceDir := filepath.Join(tmpDir, "sess-local")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatalf("MkdirAll sourceDir: %v", err)
	}
	source := SessionMetadata{
		SessionID:   "sess-local",
		WorkloadKey: "wk123",
		RunnerID:    "runner-source",
		HostID:      "host-1",
		Layers:      1,
	}
	data, _ := json.Marshal(source)
	if err := os.WriteFile(filepath.Join(sourceDir, "metadata.json"), data, 0644); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	if _, err := m.ForkSession("sess-local", "sess-fork", "runner-fork", "runner-live"); err == nil {
		t.Fatal("ForkSession should reject local-only sessions")
	}
}
