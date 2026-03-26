package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSessionMetadataForTest(t *testing.T, dir string, meta SessionMetadata) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0644); err != nil {
		t.Fatalf("WriteFile metadata.json: %v", err)
	}
}

func TestCleanupExpiredSessions_RemovesInactiveExpiredSession(t *testing.T) {
	now := time.Now().UTC()
	sessionRoot := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = sessionRoot
		m.config.SessionDefaultMaxAge = time.Hour
	})

	writeSessionMetadataForTest(t, filepath.Join(sessionRoot, "sess-old"), SessionMetadata{
		SessionID:            "sess-old",
		RunnerID:             "runner-old",
		Layers:               1,
		PausedAt:             now.Add(-2 * time.Hour),
		SessionMaxAgeSeconds: 3600,
	})

	cleaned, err := m.CleanupExpiredSessions(now)
	if err != nil {
		t.Fatalf("CleanupExpiredSessions() error = %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("CleanupExpiredSessions() cleaned = %d, want 1", cleaned)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, "sess-old")); !os.IsNotExist(err) {
		t.Fatalf("expired session still exists, err=%v", err)
	}
}

func TestCleanupExpiredSessions_SkipsActiveSession(t *testing.T) {
	now := time.Now().UTC()
	sessionRoot := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = sessionRoot
		m.config.SessionDefaultMaxAge = time.Hour
	})
	m.runners["runner-active"] = &Runner{ID: "runner-active", SessionID: "sess-active"}

	writeSessionMetadataForTest(t, filepath.Join(sessionRoot, "sess-active"), SessionMetadata{
		SessionID:            "sess-active",
		RunnerID:             "runner-active",
		Layers:               1,
		PausedAt:             now.Add(-2 * time.Hour),
		SessionMaxAgeSeconds: 3600,
	})

	cleaned, err := m.CleanupExpiredSessions(now)
	if err != nil {
		t.Fatalf("CleanupExpiredSessions() error = %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("CleanupExpiredSessions() cleaned = %d, want 0", cleaned)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, "sess-active")); err != nil {
		t.Fatalf("active session should remain, stat error = %v", err)
	}
}

func TestCleanupStaleSessionState_RemovesInactiveFiles(t *testing.T) {
	now := time.Now().UTC()
	socketRoot := t.TempDir()
	stateDir := filepath.Join(socketRoot, "session-state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("MkdirAll(stateDir): %v", err)
	}

	staleFile := filepath.Join(stateDir, "runner-stale.state")
	activeFile := filepath.Join(stateDir, "runner-active.state")
	if err := os.WriteFile(staleFile, []byte("stale"), 0644); err != nil {
		t.Fatalf("WriteFile stale: %v", err)
	}
	if err := os.WriteFile(activeFile, []byte("active"), 0644); err != nil {
		t.Fatalf("WriteFile active: %v", err)
	}

	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(staleFile, old, old); err != nil {
		t.Fatalf("Chtimes stale: %v", err)
	}
	if err := os.Chtimes(activeFile, old, old); err != nil {
		t.Fatalf("Chtimes active: %v", err)
	}

	m := newTestManager(func(m *Manager) {
		m.config.SocketDir = socketRoot
		m.config.SessionStateMaxAge = time.Hour
	})
	m.runners["runner-active"] = &Runner{ID: "runner-active"}

	cleaned, err := m.CleanupStaleSessionState(now)
	if err != nil {
		t.Fatalf("CleanupStaleSessionState() error = %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("CleanupStaleSessionState() cleaned = %d, want 1", cleaned)
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Fatalf("stale state file still exists, err=%v", err)
	}
	if _, err := os.Stat(activeFile); err != nil {
		t.Fatalf("active state file should remain, stat error = %v", err)
	}
}

func TestCleanupLocalChunkCache_PrunesOldestFilesToLowWatermark(t *testing.T) {
	now := time.Now().UTC()
	cacheRoot := filepath.Join(t.TempDir(), "chunks")
	m := newTestManager(func(m *Manager) {
		m.config.SnapshotCachePath = filepath.Dir(cacheRoot)
		m.config.ChunkCacheMaxBytes = 100
		m.config.ChunkCacheLowWatermark = 0.5
	})

	files := []struct {
		rel     string
		size    int
		modTime time.Time
	}{
		{rel: filepath.Join("aa", "oldest"), size: 40, modTime: now.Add(-4 * time.Hour)},
		{rel: filepath.Join("bb", "older"), size: 35, modTime: now.Add(-3 * time.Hour)},
		{rel: filepath.Join("cc", "newer"), size: 25, modTime: now.Add(-2 * time.Hour)},
		{rel: filepath.Join("dd", "newest"), size: 20, modTime: now.Add(-1 * time.Hour)},
	}
	for _, file := range files {
		path := filepath.Join(cacheRoot, file.rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(path, make([]byte, file.size), 0644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		if err := os.Chtimes(path, file.modTime, file.modTime); err != nil {
			t.Fatalf("Chtimes(%q): %v", path, err)
		}
	}

	bytesFreed, filesRemoved, err := m.CleanupLocalChunkCache(now)
	if err != nil {
		t.Fatalf("CleanupLocalChunkCache() error = %v", err)
	}
	if filesRemoved != 2 {
		t.Fatalf("CleanupLocalChunkCache() filesRemoved = %d, want 2", filesRemoved)
	}
	if bytesFreed != 75 {
		t.Fatalf("CleanupLocalChunkCache() bytesFreed = %d, want 75", bytesFreed)
	}

	for _, removed := range []string{
		filepath.Join(cacheRoot, "aa", "oldest"),
		filepath.Join(cacheRoot, "bb", "older"),
	} {
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("expected removed chunk %q to be gone, err=%v", removed, err)
		}
	}
	for _, kept := range []string{
		filepath.Join(cacheRoot, "cc", "newer"),
		filepath.Join(cacheRoot, "dd", "newest"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("expected chunk %q to remain, stat err=%v", kept, err)
		}
	}
}

func TestCleanupStaleLogs_RemovesInactiveArtifacts(t *testing.T) {
	now := time.Now().UTC()
	logRoot := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.LogDir = logRoot
		m.config.LogMaxAge = 2 * time.Hour
	})
	m.runners["runner-active"] = &Runner{ID: "runner-active"}

	files := []struct {
		name    string
		modTime time.Time
		keep    bool
	}{
		{name: "runner-stale.log", modTime: now.Add(-4 * time.Hour), keep: false},
		{name: "runner-stale.console.log", modTime: now.Add(-4 * time.Hour), keep: false},
		{name: "runner-stale.metrics", modTime: now.Add(-4 * time.Hour), keep: false},
		{name: "runner-active.log", modTime: now.Add(-4 * time.Hour), keep: true},
		{name: "runner-fresh.log", modTime: now.Add(-30 * time.Minute), keep: true},
	}
	for _, file := range files {
		path := filepath.Join(logRoot, file.name)
		if err := os.WriteFile(path, []byte(file.name), 0644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		if err := os.Chtimes(path, file.modTime, file.modTime); err != nil {
			t.Fatalf("Chtimes(%q): %v", path, err)
		}
	}

	cleaned, err := m.CleanupStaleLogs(now)
	if err != nil {
		t.Fatalf("CleanupStaleLogs() error = %v", err)
	}
	if cleaned != 3 {
		t.Fatalf("CleanupStaleLogs() cleaned = %d, want 3", cleaned)
	}

	for _, file := range files {
		_, err := os.Stat(filepath.Join(logRoot, file.name))
		if file.keep && err != nil {
			t.Fatalf("expected %q to remain, stat err=%v", file.name, err)
		}
		if !file.keep && !os.IsNotExist(err) {
			t.Fatalf("expected %q to be removed, err=%v", file.name, err)
		}
	}
}

func TestCleanupExpiredQuarantine_RemovesInactiveDirs(t *testing.T) {
	now := time.Now().UTC()
	quarantineRoot := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.QuarantineDir = quarantineRoot
		m.config.QuarantineMaxAge = 2 * time.Hour
	})
	m.runners["runner-active"] = &Runner{ID: "runner-active"}

	staleDir := filepath.Join(quarantineRoot, "runner-stale")
	activeDir := filepath.Join(quarantineRoot, "runner-active")
	freshDir := filepath.Join(quarantineRoot, "runner-fresh")
	for _, dir := range []string{staleDir, activeDir, freshDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}

	old := now.Add(-4 * time.Hour)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatalf("Chtimes staleDir: %v", err)
	}
	if err := os.Chtimes(activeDir, old, old); err != nil {
		t.Fatalf("Chtimes activeDir: %v", err)
	}

	cleaned, err := m.CleanupExpiredQuarantine(now)
	if err != nil {
		t.Fatalf("CleanupExpiredQuarantine() error = %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("CleanupExpiredQuarantine() cleaned = %d, want 1", cleaned)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("expected stale quarantine dir removed, err=%v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("expected active quarantine dir to remain, err=%v", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Fatalf("expected fresh quarantine dir to remain, err=%v", err)
	}
}

func TestCollectGCUsage_ReportsArtifactBytes(t *testing.T) {
	baseDir := t.TempDir()
	sessionDir := filepath.Join(baseDir, "sessions")
	socketDir := filepath.Join(baseDir, "sockets")
	logDir := filepath.Join(baseDir, "logs")
	quarantineDir := filepath.Join(baseDir, "quarantine")
	chunkDir := filepath.Join(baseDir, "snapshots", "chunks")

	m := newTestManager(func(m *Manager) {
		m.config.SessionDir = sessionDir
		m.config.SocketDir = socketDir
		m.config.LogDir = logDir
		m.config.QuarantineDir = quarantineDir
		m.config.SnapshotCachePath = filepath.Join(baseDir, "snapshots")
	})

	mustWriteSizedFile := func(path string, size int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	mustWriteSizedFile(filepath.Join(sessionDir, "sess-1", "metadata.json"), 11)
	mustWriteSizedFile(filepath.Join(socketDir, "session-state", "runner-1.state"), 13)
	mustWriteSizedFile(filepath.Join(logDir, "runner-1.log"), 17)
	mustWriteSizedFile(filepath.Join(quarantineDir, "runner-1", "manifest.json"), 19)
	mustWriteSizedFile(filepath.Join(chunkDir, "ab", "chunk1"), 23)

	usage := m.CollectGCUsage()
	if usage.SessionsBytes != 11 {
		t.Fatalf("SessionsBytes = %d, want 11", usage.SessionsBytes)
	}
	if usage.SessionStateBytes != 13 {
		t.Fatalf("SessionStateBytes = %d, want 13", usage.SessionStateBytes)
	}
	if usage.LogBytes != 17 {
		t.Fatalf("LogBytes = %d, want 17", usage.LogBytes)
	}
	if usage.QuarantineBytes != 19 {
		t.Fatalf("QuarantineBytes = %d, want 19", usage.QuarantineBytes)
	}
	if usage.ChunkCacheBytes != 23 {
		t.Fatalf("ChunkCacheBytes = %d, want 23", usage.ChunkCacheBytes)
	}
}

func TestCleanupFunctions_UpdateGCMetricsSnapshot(t *testing.T) {
	now := time.Now().UTC()
	logRoot := t.TempDir()
	m := newTestManager(func(m *Manager) {
		m.config.LogDir = logRoot
		m.config.LogMaxAge = time.Hour
	})

	logPath := filepath.Join(logRoot, "runner-stale.log")
	if err := os.WriteFile(logPath, []byte("12345"), 0644); err != nil {
		t.Fatalf("WriteFile(%q): %v", logPath, err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatalf("Chtimes(%q): %v", logPath, err)
	}

	cleaned, err := m.CleanupStaleLogs(now)
	if err != nil {
		t.Fatalf("CleanupStaleLogs() error = %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("CleanupStaleLogs() cleaned = %d, want 1", cleaned)
	}

	stats := m.GCMetricsSnapshot()
	if stats.Logs.BytesReclaimed != 5 {
		t.Fatalf("Logs.BytesReclaimed = %d, want 5", stats.Logs.BytesReclaimed)
	}
	if stats.Logs.FilesRemoved != 1 {
		t.Fatalf("Logs.FilesRemoved = %d, want 1", stats.Logs.FilesRemoved)
	}
}
