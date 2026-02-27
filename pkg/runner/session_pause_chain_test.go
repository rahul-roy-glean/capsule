package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// TestBuildExtensionDriveBaseIndex verifies that the helper constructs
// a valid ChunkIndex from golden metadata for a specific drive.
func TestBuildExtensionDriveBaseIndex_DriveFound(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize: snapshot.DefaultChunkSize,
		ExtensionDrives: map[string]snapshot.ExtensionDrive{
			"git_drive": {
				SizeBytes: 10 * 1024 * 1024 * 1024,
				Chunks: []snapshot.ChunkRef{
					{Offset: 0, Size: snapshot.DefaultChunkSize, Hash: "chunk-abc"},
					{Offset: snapshot.DefaultChunkSize, Size: snapshot.DefaultChunkSize, Hash: snapshot.ZeroChunkHash},
					{Offset: 2 * snapshot.DefaultChunkSize, Size: snapshot.DefaultChunkSize, Hash: "chunk-def"},
				},
			},
		},
	}

	idx := buildExtensionDriveBaseIndex(meta, "git_drive")

	if idx.ChunkSizeBytes != snapshot.DefaultChunkSize {
		t.Errorf("ChunkSizeBytes = %d, want %d", idx.ChunkSizeBytes, snapshot.DefaultChunkSize)
	}
	if idx.Region.LogicalSizeBytes != 10*1024*1024*1024 {
		t.Errorf("LogicalSizeBytes = %d, want %d", idx.Region.LogicalSizeBytes, 10*1024*1024*1024)
	}
	if idx.CAS.Kind != "disk" {
		t.Errorf("CAS.Kind = %q, want %q", idx.CAS.Kind, "disk")
	}
	// Zero chunks are excluded from extents.
	if len(idx.Region.Extents) != 2 {
		t.Errorf("Extents count = %d, want 2 (zero chunk excluded)", len(idx.Region.Extents))
	}
	if idx.Region.Extents[0].Hash != "chunk-abc" {
		t.Errorf("Extents[0].Hash = %q, want %q", idx.Region.Extents[0].Hash, "chunk-abc")
	}
	if idx.Region.Extents[1].Hash != "chunk-def" {
		t.Errorf("Extents[1].Hash = %q, want %q", idx.Region.Extents[1].Hash, "chunk-def")
	}
}

// TestBuildExtensionDriveBaseIndex_DriveNotFound verifies that a missing
// drive returns an empty base index (not an error).
func TestBuildExtensionDriveBaseIndex_DriveNotFound(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize:       snapshot.DefaultChunkSize,
		ExtensionDrives: map[string]snapshot.ExtensionDrive{},
	}

	idx := buildExtensionDriveBaseIndex(meta, "nonexistent_drive")

	if idx == nil {
		t.Fatal("Expected non-nil ChunkIndex for missing drive")
	}
	if len(idx.Region.Extents) != 0 {
		t.Errorf("Expected 0 extents for missing drive, got %d", len(idx.Region.Extents))
	}
	if idx.Region.LogicalSizeBytes != 0 {
		t.Errorf("LogicalSizeBytes = %d, want 0 for missing drive", idx.Region.LogicalSizeBytes)
	}
}

// TestBuildExtensionDriveBaseIndex_NilMeta verifies that nil metadata
// returns a valid empty base index.
func TestBuildExtensionDriveBaseIndex_NilMeta(t *testing.T) {
	idx := buildExtensionDriveBaseIndex(nil, "git_drive")

	if idx == nil {
		t.Fatal("Expected non-nil ChunkIndex for nil meta")
	}
	if len(idx.Region.Extents) != 0 {
		t.Errorf("Expected 0 extents for nil meta, got %d", len(idx.Region.Extents))
	}
}

// TestGCSDiskIndexObjects_PreviousChaining tests that GCSDiskIndexObjects
// is properly chained across pauses by simulating the metadata write/read cycle.
func TestGCSDiskIndexObjects_PreviousChaining(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "sess-chain-test"
	sessDir := filepath.Join(tmpDir, sessionID)
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatalf("Failed to create session dir: %v", err)
	}

	// Write pause 1 metadata with disk index objects.
	meta1 := SessionMetadata{
		SessionID:   sessionID,
		WorkloadKey: "wk123",
		RunnerID:    "runner-1",
		HostID:      "host-1",
		Layers:      1,
		GCSManifestPath:   "v1/wk123/runner_state/runner-1/snapshot_manifest.json",
		GCSMemIndexObject: "v1/wk123/runner_state/runner-1/chunked-metadata.json",
		GCSDiskIndexObjects: map[string]string{
			"git_drive":   "v1/wk123/runner_state/runner-1/git_drive-disk.json",
			"bazel_cache": "v1/wk123/runner_state/runner-1/bazel_cache-disk.json",
		},
	}

	data1, err := json.MarshalIndent(meta1, "", "  ")
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "metadata.json"), data1, 0644); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Simulate what PauseRunner does: read previous metadata and extract disk index objects.
	var prevGCSDiskIndexObjects map[string]string
	if prevData, readErr := os.ReadFile(filepath.Join(sessDir, "metadata.json")); readErr == nil {
		var prev SessionMetadata
		if json.Unmarshal(prevData, &prev) == nil {
			prevGCSDiskIndexObjects = prev.GCSDiskIndexObjects
		}
	}

	// Verify that the chaining reads the correct previous disk index paths.
	if prevGCSDiskIndexObjects == nil {
		t.Fatal("Expected non-nil prevGCSDiskIndexObjects")
	}
	if prevGCSDiskIndexObjects["git_drive"] != meta1.GCSDiskIndexObjects["git_drive"] {
		t.Errorf("git_drive path = %q, want %q",
			prevGCSDiskIndexObjects["git_drive"],
			meta1.GCSDiskIndexObjects["git_drive"])
	}
	if prevGCSDiskIndexObjects["bazel_cache"] != meta1.GCSDiskIndexObjects["bazel_cache"] {
		t.Errorf("bazel_cache path = %q, want %q",
			prevGCSDiskIndexObjects["bazel_cache"],
			meta1.GCSDiskIndexObjects["bazel_cache"])
	}

	// Simulate pause 2: only git_drive is dirty, bazel_cache is clean.
	// The manager must carry forward bazel_cache's disk index from pause 1
	// so that a future pause 3 can use it as base instead of falling back
	// to the golden snapshot (which would cause a full re-upload).
	newExtDiskIndexes := map[string]bool{
		"git_drive": true, // dirty this pause — gets a new index path
	}
	meta2 := SessionMetadata{
		SessionID:           sessionID,
		WorkloadKey:         "wk123",
		RunnerID:            "runner-1",
		HostID:              "host-1",
		Layers:              2,
		GCSManifestPath:     "v1/wk123/runner_state/runner-1/snapshot_manifest.json",
		GCSMemIndexObject:   "v1/wk123/runner_state/runner-1/chunked-metadata.json",
		GCSDiskIndexObjects: make(map[string]string),
	}

	// Carry forward previous disk index objects for drives not dirty this pause.
	for driveID, path := range prevGCSDiskIndexObjects {
		if !newExtDiskIndexes[driveID] {
			meta2.GCSDiskIndexObjects[driveID] = path
		}
	}
	// Record newly uploaded disk indexes.
	for driveID := range newExtDiskIndexes {
		meta2.GCSDiskIndexObjects[driveID] = "v1/wk123/runner_state/runner-1/pause2-" + driveID + "-disk.json"
	}

	data2, _ := json.MarshalIndent(meta2, "", "  ")
	if err := os.WriteFile(filepath.Join(sessDir, "metadata.json"), data2, 0644); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Verify that pause 2 metadata carries forward bazel_cache from pause 1
	// and has an updated path for git_drive.
	if prevData, readErr := os.ReadFile(filepath.Join(sessDir, "metadata.json")); readErr == nil {
		var prev SessionMetadata
		if json.Unmarshal(prevData, &prev) == nil {
			if len(prev.GCSDiskIndexObjects) != 2 {
				t.Fatalf("Expected 2 GCSDiskIndexObjects in pause 2 metadata, got %d: %v", len(prev.GCSDiskIndexObjects), prev.GCSDiskIndexObjects)
			}
			// bazel_cache should be carried forward from pause 1 (unchanged).
			if prev.GCSDiskIndexObjects["bazel_cache"] != meta1.GCSDiskIndexObjects["bazel_cache"] {
				t.Errorf("bazel_cache should be carried forward from pause 1: got %q, want %q",
					prev.GCSDiskIndexObjects["bazel_cache"], meta1.GCSDiskIndexObjects["bazel_cache"])
			}
			// git_drive should have the new pause 2 path.
			if prev.GCSDiskIndexObjects["git_drive"] != "v1/wk123/runner_state/runner-1/pause2-git_drive-disk.json" {
				t.Errorf("git_drive should have pause 2 path: got %q", prev.GCSDiskIndexObjects["git_drive"])
			}
		}
	}
}

// TestGCSDiskIndexObjects_MultiDrive tests that multiple drives get independent
// disk index tracking in session metadata.
func TestGCSDiskIndexObjects_MultiDrive(t *testing.T) {
	meta := SessionMetadata{
		SessionID:   "sess-multi",
		WorkloadKey: "wk123",
		RunnerID:    "runner-1",
		GCSDiskIndexObjects: map[string]string{
			"git_drive":   "v1/wk/r1/git_drive-disk.json",
			"bazel_cache": "v1/wk/r1/bazel_cache-disk.json",
			"artifacts":   "v1/wk/r1/artifacts-disk.json",
		},
	}

	// Marshal and unmarshal roundtrip.
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var restored SessionMetadata
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(restored.GCSDiskIndexObjects) != 3 {
		t.Fatalf("GCSDiskIndexObjects count = %d, want 3", len(restored.GCSDiskIndexObjects))
	}

	drives := []string{"git_drive", "bazel_cache", "artifacts"}
	for _, drive := range drives {
		if restored.GCSDiskIndexObjects[drive] != meta.GCSDiskIndexObjects[drive] {
			t.Errorf("drive %s: path = %q, want %q",
				drive, restored.GCSDiskIndexObjects[drive], meta.GCSDiskIndexObjects[drive])
		}
	}
}
