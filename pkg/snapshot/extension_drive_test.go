package snapshot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestExtensionDrive_ChunkedMetadataRoundtrip(t *testing.T) {
	meta := &ChunkedSnapshotMetadata{
		Version:   "1",
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		ChunkSize: DefaultChunkSize,
		ExtensionDrives: map[string]ExtensionDrive{
			"git_drive": {
				ReadOnly:  false,
				SizeBytes: 10 * 1024 * 1024 * 1024,
				Chunks: []ChunkRef{
					{Offset: 0, Size: DefaultChunkSize, Hash: "abc123"},
					{Offset: DefaultChunkSize, Size: DefaultChunkSize, Hash: "def456"},
				},
			},
			"bazel_cache": {
				ReadOnly:  true,
				SizeBytes: 20 * 1024 * 1024 * 1024,
				Chunks: []ChunkRef{
					{Offset: 0, Size: DefaultChunkSize, Hash: "ghi789"},
				},
			},
		},
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var restored ChunkedSnapshotMetadata
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(restored.ExtensionDrives) != 2 {
		t.Fatalf("ExtensionDrives count = %d, want 2", len(restored.ExtensionDrives))
	}

	gitDrive, ok := restored.ExtensionDrives["git_drive"]
	if !ok {
		t.Fatal("git_drive not found after roundtrip")
	}
	if gitDrive.ReadOnly {
		t.Error("git_drive.ReadOnly should be false")
	}
	if gitDrive.SizeBytes != 10*1024*1024*1024 {
		t.Errorf("git_drive.SizeBytes = %d, want %d", gitDrive.SizeBytes, 10*1024*1024*1024)
	}
	if len(gitDrive.Chunks) != 2 {
		t.Errorf("git_drive.Chunks count = %d, want 2", len(gitDrive.Chunks))
	}
	if gitDrive.Chunks[0].Hash != "abc123" {
		t.Errorf("git_drive.Chunks[0].Hash = %q, want %q", gitDrive.Chunks[0].Hash, "abc123")
	}

	bazelDrive, ok := restored.ExtensionDrives["bazel_cache"]
	if !ok {
		t.Fatal("bazel_cache not found after roundtrip")
	}
	if !bazelDrive.ReadOnly {
		t.Error("bazel_cache.ReadOnly should be true")
	}
}

func TestExtensionDrive_ManifestRoundtrip(t *testing.T) {
	man := &SnapshotManifest{
		Version:     "1",
		SnapshotID:  "test-id",
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
		WorkloadKey: "abc123",
		ExtensionDisks: map[string]DiskSection{
			"git_drive": {
				Mode:             "chunked",
				TotalSizeBytes:   10 * 1024 * 1024 * 1024,
				ChunkIndexObject: "v1/abc123/runner_state/r1/git_drive-disk.json",
			},
			"bazel_cache": {
				Mode:             "chunked",
				TotalSizeBytes:   20 * 1024 * 1024 * 1024,
				ChunkIndexObject: "v1/abc123/runner_state/r1/bazel_cache-disk.json",
			},
		},
	}
	man.Firecracker.VMStateObject = "v1/abc123/runner_state/r1/snapshot.state"
	man.Memory.Mode = "chunked"
	man.Memory.ChunkIndexObject = "v1/abc123/runner_state/r1/chunked-metadata.json"
	man.Integrity.Algo = "sha256"

	data, err := json.Marshal(man)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var restored SnapshotManifest
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(restored.ExtensionDisks) != 2 {
		t.Fatalf("ExtensionDisks count = %d, want 2", len(restored.ExtensionDisks))
	}

	gitDisk, ok := restored.ExtensionDisks["git_drive"]
	if !ok {
		t.Fatal("git_drive not found in ExtensionDisks after roundtrip")
	}
	if gitDisk.Mode != "chunked" {
		t.Errorf("git_drive.Mode = %q, want %q", gitDisk.Mode, "chunked")
	}
	if gitDisk.ChunkIndexObject != "v1/abc123/runner_state/r1/git_drive-disk.json" {
		t.Errorf("git_drive.ChunkIndexObject = %q, unexpected", gitDisk.ChunkIndexObject)
	}

	bazelDisk, ok := restored.ExtensionDisks["bazel_cache"]
	if !ok {
		t.Fatal("bazel_cache not found in ExtensionDisks after roundtrip")
	}
	if bazelDisk.TotalSizeBytes != 20*1024*1024*1024 {
		t.Errorf("bazel_cache.TotalSizeBytes = %d, want %d", bazelDisk.TotalSizeBytes, 20*1024*1024*1024)
	}
}
