package snapshot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSnapshotMetadata_RepoFields(t *testing.T) {
	meta := SnapshotMetadata{
		Version:    "v20260221-120000-main",
		Repo:       "https://github.com/org/repo",
		RepoSlug:   "org-repo",
		CreatedAt:  time.Now(),
		KernelPath: "kernel.bin",
		RootfsPath: "rootfs.img",
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	var decoded SnapshotMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if decoded.Repo != meta.Repo {
		t.Errorf("Repo = %q, want %q", decoded.Repo, meta.Repo)
	}
	if decoded.RepoSlug != meta.RepoSlug {
		t.Errorf("RepoSlug = %q, want %q", decoded.RepoSlug, meta.RepoSlug)
	}
}

func TestSnapshotMetadata_BackwardsCompatible(t *testing.T) {
	// Old metadata without repo fields should still unmarshal
	oldJSON := `{"version":"v1","bazel_version":"7.0","created_at":"2026-01-01T00:00:00Z"}`

	var meta SnapshotMetadata
	if err := json.Unmarshal([]byte(oldJSON), &meta); err != nil {
		t.Fatalf("Failed to unmarshal old metadata: %v", err)
	}

	if meta.Version != "v1" {
		t.Errorf("Version = %q, want %q", meta.Version, "v1")
	}
	if meta.Repo != "" {
		t.Errorf("Repo should be empty for old metadata, got %q", meta.Repo)
	}
	if meta.RepoSlug != "" {
		t.Errorf("RepoSlug should be empty for old metadata, got %q", meta.RepoSlug)
	}
}

func TestChunkedSnapshotMetadata_RepoFields(t *testing.T) {
	meta := ChunkedSnapshotMetadata{
		Version:    "v20260221-120000-main",
		Repo:       "https://github.com/org/repo",
		RepoSlug:   "org-repo",
		ChunkSize:  DefaultChunkSize,
		KernelHash: "abc123",
		StateHash:  "def456",
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}

	var decoded ChunkedSnapshotMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if decoded.Repo != meta.Repo {
		t.Errorf("Repo = %q, want %q", decoded.Repo, meta.Repo)
	}
	if decoded.RepoSlug != meta.RepoSlug {
		t.Errorf("RepoSlug = %q, want %q", decoded.RepoSlug, meta.RepoSlug)
	}
	if decoded.ChunkSize != DefaultChunkSize {
		t.Errorf("ChunkSize = %d, want %d", decoded.ChunkSize, DefaultChunkSize)
	}
}

func TestChunkedSnapshotMetadata_BackwardsCompatible(t *testing.T) {
	oldJSON := `{"version":"v1","chunk_size":4194304,"kernel_hash":"abc","state_hash":"def","mem_chunks":[],"rootfs_chunks":[],"total_mem_size":0,"total_disk_size":0}`

	var meta ChunkedSnapshotMetadata
	if err := json.Unmarshal([]byte(oldJSON), &meta); err != nil {
		t.Fatalf("Failed to unmarshal old chunked metadata: %v", err)
	}

	if meta.Version != "v1" {
		t.Errorf("Version = %q, want %q", meta.Version, "v1")
	}
	if meta.Repo != "" {
		t.Errorf("Repo should be empty for old metadata, got %q", meta.Repo)
	}
	if meta.RepoSlug != "" {
		t.Errorf("RepoSlug should be empty for old metadata, got %q", meta.RepoSlug)
	}
}

func TestSnapshotDiff_Summary(t *testing.T) {
	diff := &SnapshotDiff{
		OldVersion:        "v1",
		NewVersion:        "v2",
		ChangedDiskChunks: []int{0, 5, 10},
		ChangedMemChunks:  []int{1, 2},
	}

	summary := diff.Summary()
	if summary == "" {
		t.Error("Summary should not be empty")
	}

	// Should contain version info and chunk counts
	expected := "Snapshot diff v1 -> v2: 3 disk chunks changed, 2 memory chunks changed"
	if summary != expected {
		t.Errorf("Summary = %q, want %q", summary, expected)
	}
}
