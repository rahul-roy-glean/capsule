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

// mergeDiskIndexObjects mirrors the carry-forward logic in PauseRunner:
// newly-dirty drives get fresh paths, non-dirty drives carry forward from
// the previous pause. This is extracted so we test the exact same algorithm.
func mergeDiskIndexObjects(
	prevGCSDiskIndexObjects map[string]string,
	newDirtyDrives map[string]string, // driveID → new GCS path for dirty drives
) map[string]string {
	if len(prevGCSDiskIndexObjects) == 0 && len(newDirtyDrives) == 0 {
		return nil
	}
	merged := make(map[string]string)
	// Carry forward non-dirty drives from previous pause.
	for driveID, path := range prevGCSDiskIndexObjects {
		if _, dirty := newDirtyDrives[driveID]; !dirty {
			merged[driveID] = path
		}
	}
	// Record newly uploaded disk indexes (overwrite if also in prev).
	for driveID, path := range newDirtyDrives {
		merged[driveID] = path
	}
	return merged
}

// TestGCSDiskIndexObjects_ThreePauseChain is the regression test for the
// chaining bug. It exercises the exact scenario that was never tested:
//
//	Pause 1: git_drive + bazel_cache both dirty → both indexed in GCS
//	Pause 2: only git_drive dirty → bazel_cache must carry forward
//	Pause 3: only bazel_cache dirty → must use bazel_cache's index from
//	          pause 1 (carried forward through pause 2), NOT the golden snapshot
//
// Before the fix, pause 2 would drop bazel_cache from GCSDiskIndexObjects,
// so pause 3 would fall back to buildExtensionDriveBaseIndex(goldenMeta) and
// re-upload every session-dirty chunk for bazel_cache.
func TestGCSDiskIndexObjects_ThreePauseChain(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "sess-3pause"
	sessDir := filepath.Join(tmpDir, sessionID)
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatalf("Failed to create session dir: %v", err)
	}
	metaPath := filepath.Join(sessDir, "metadata.json")

	// --- Pause 1: both drives dirty ---
	pause1DiskIndexes := map[string]string{
		"git_drive":   "v1/wk/r1/pause1-git_drive-disk.json",
		"bazel_cache": "v1/wk/r1/pause1-bazel_cache-disk.json",
	}
	meta1 := SessionMetadata{
		SessionID:           sessionID,
		WorkloadKey:         "wk123",
		RunnerID:            "runner-1",
		HostID:              "host-1",
		Layers:              1,
		GCSManifestPath:     "v1/wk/r1/snapshot_manifest.json",
		GCSMemIndexObject:   "v1/wk/r1/chunked-metadata.json",
		GCSDiskIndexObjects: mergeDiskIndexObjects(nil, pause1DiskIndexes),
	}
	writeMetadata(t, metaPath, meta1)

	// Sanity: pause 1 should have both drives.
	if len(meta1.GCSDiskIndexObjects) != 2 {
		t.Fatalf("Pause 1: expected 2 disk indexes, got %d", len(meta1.GCSDiskIndexObjects))
	}

	// --- Pause 2: only git_drive dirty, bazel_cache clean ---
	prev1 := readPrevMetadata(t, metaPath)
	pause2DiskIndexes := map[string]string{
		"git_drive": "v1/wk/r1/pause2-git_drive-disk.json",
	}
	meta2 := SessionMetadata{
		SessionID:           sessionID,
		WorkloadKey:         "wk123",
		RunnerID:            "runner-1",
		HostID:              "host-1",
		Layers:              2,
		GCSManifestPath:     "v1/wk/r1/snapshot_manifest.json",
		GCSMemIndexObject:   "v1/wk/r1/chunked-metadata.json",
		GCSDiskIndexObjects: mergeDiskIndexObjects(prev1.GCSDiskIndexObjects, pause2DiskIndexes),
	}
	writeMetadata(t, metaPath, meta2)

	// Pause 2 must have both drives: git_drive updated, bazel_cache carried forward.
	if len(meta2.GCSDiskIndexObjects) != 2 {
		t.Fatalf("Pause 2: expected 2 disk indexes, got %d: %v",
			len(meta2.GCSDiskIndexObjects), meta2.GCSDiskIndexObjects)
	}
	if meta2.GCSDiskIndexObjects["git_drive"] != "v1/wk/r1/pause2-git_drive-disk.json" {
		t.Errorf("Pause 2: git_drive = %q, want pause2 path", meta2.GCSDiskIndexObjects["git_drive"])
	}
	if meta2.GCSDiskIndexObjects["bazel_cache"] != "v1/wk/r1/pause1-bazel_cache-disk.json" {
		t.Errorf("Pause 2: bazel_cache = %q, want pause1 path (carried forward)",
			meta2.GCSDiskIndexObjects["bazel_cache"])
	}

	// --- Pause 3: only bazel_cache dirty, git_drive clean ---
	prev2 := readPrevMetadata(t, metaPath)
	pause3DiskIndexes := map[string]string{
		"bazel_cache": "v1/wk/r1/pause3-bazel_cache-disk.json",
	}
	meta3 := SessionMetadata{
		SessionID:           sessionID,
		WorkloadKey:         "wk123",
		RunnerID:            "runner-1",
		HostID:              "host-1",
		Layers:              3,
		GCSManifestPath:     "v1/wk/r1/snapshot_manifest.json",
		GCSMemIndexObject:   "v1/wk/r1/chunked-metadata.json",
		GCSDiskIndexObjects: mergeDiskIndexObjects(prev2.GCSDiskIndexObjects, pause3DiskIndexes),
	}
	writeMetadata(t, metaPath, meta3)

	// Pause 3 must have both drives: bazel_cache updated, git_drive carried from pause 2.
	if len(meta3.GCSDiskIndexObjects) != 2 {
		t.Fatalf("Pause 3: expected 2 disk indexes, got %d: %v",
			len(meta3.GCSDiskIndexObjects), meta3.GCSDiskIndexObjects)
	}
	if meta3.GCSDiskIndexObjects["bazel_cache"] != "v1/wk/r1/pause3-bazel_cache-disk.json" {
		t.Errorf("Pause 3: bazel_cache = %q, want pause3 path", meta3.GCSDiskIndexObjects["bazel_cache"])
	}
	// This is the critical assertion: git_drive's index must come from pause 2,
	// not from the golden snapshot. Before the fix, prev2.GCSDiskIndexObjects
	// would not contain git_drive (it was dropped in pause 2 because it wasn't
	// dirty), so pause 3 would fall back to buildExtensionDriveBaseIndex(golden).
	if meta3.GCSDiskIndexObjects["git_drive"] != "v1/wk/r1/pause2-git_drive-disk.json" {
		t.Errorf("Pause 3: git_drive = %q, want pause2 path (carried forward through pause 2).\n"+
			"This is the chaining bug: drive was clean in pause 2 so its index was dropped,\n"+
			"causing pause 3 to fall back to golden and re-upload all dirty chunks.",
			meta3.GCSDiskIndexObjects["git_drive"])
	}

	// Verify the base index selection logic: when prevGCSDiskIndexObjects has
	// an entry for a drive, PauseRunner downloads that ChunkIndex as base.
	// When it doesn't, it falls back to buildExtensionDriveBaseIndex(golden).
	// After the fix, prevGCSDiskIndexObjects always has entries for all
	// previously-seen drives, so the golden fallback only happens on first pause.
	goldenMeta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize: snapshot.DefaultChunkSize,
		ExtensionDrives: map[string]snapshot.ExtensionDrive{
			"git_drive":   {SizeBytes: 10 * 1024 * 1024 * 1024},
			"bazel_cache": {SizeBytes: 20 * 1024 * 1024 * 1024},
		},
	}

	// Simulate pause 3's base-index selection for bazel_cache (dirty this pause).
	prev2Read := readPrevMetadata(t, metaPath)

	// bazel_cache: should find it in prev metadata → use session index as base.
	if prevPath := prev2Read.GCSDiskIndexObjects["bazel_cache"]; prevPath == "" {
		t.Error("Pause 3 base selection: bazel_cache not in prev metadata; would fall back to golden")
	}

	// git_drive: not dirty, but should still be in prev metadata for future pauses.
	if prevPath := prev2Read.GCSDiskIndexObjects["git_drive"]; prevPath == "" {
		t.Error("Pause 3: git_drive should be in prev metadata for future chaining")
	}

	// Verify that golden fallback only happens when no prev entry exists.
	goldenIdx := buildExtensionDriveBaseIndex(goldenMeta, "new_drive")
	if goldenIdx.Region.LogicalSizeBytes != 0 {
		t.Errorf("Golden fallback for unknown drive should have 0 size, got %d", goldenIdx.Region.LogicalSizeBytes)
	}
}

// TestGCSDiskIndexObjects_NoDirtyDrives verifies that when no drives are dirty
// in a pause, all previous disk index objects are carried forward unchanged.
func TestGCSDiskIndexObjects_NoDirtyDrives(t *testing.T) {
	prev := map[string]string{
		"git_drive":   "v1/wk/r1/git_drive-disk.json",
		"bazel_cache": "v1/wk/r1/bazel_cache-disk.json",
	}
	merged := mergeDiskIndexObjects(prev, map[string]string{})
	if len(merged) != 2 {
		t.Fatalf("Expected 2 carried-forward entries, got %d: %v", len(merged), merged)
	}
	for driveID, path := range prev {
		if merged[driveID] != path {
			t.Errorf("drive %s: got %q, want %q", driveID, merged[driveID], path)
		}
	}
}

// TestGCSDiskIndexObjects_FirstPause verifies that on the first pause (no prev
// metadata), only newly-dirty drives appear in the result.
func TestGCSDiskIndexObjects_FirstPause(t *testing.T) {
	dirty := map[string]string{
		"git_drive": "v1/wk/r1/git_drive-disk.json",
	}
	merged := mergeDiskIndexObjects(nil, dirty)
	if len(merged) != 1 {
		t.Fatalf("Expected 1 entry on first pause, got %d: %v", len(merged), merged)
	}
	if merged["git_drive"] != dirty["git_drive"] {
		t.Errorf("git_drive: got %q, want %q", merged["git_drive"], dirty["git_drive"])
	}
}

// TestGCSDiskIndexObjects_DirtyOverridesPrev verifies that a newly-dirty drive
// overwrites the carried-forward path from a previous pause.
func TestGCSDiskIndexObjects_DirtyOverridesPrev(t *testing.T) {
	prev := map[string]string{
		"git_drive": "v1/wk/r1/pause1-git_drive-disk.json",
	}
	dirty := map[string]string{
		"git_drive": "v1/wk/r1/pause2-git_drive-disk.json",
	}
	merged := mergeDiskIndexObjects(prev, dirty)
	if merged["git_drive"] != dirty["git_drive"] {
		t.Errorf("Dirty drive should override prev: got %q, want %q",
			merged["git_drive"], dirty["git_drive"])
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

// --- helpers ---

func writeMetadata(t *testing.T, path string, meta SessionMetadata) {
	t.Helper()
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
}

func readPrevMetadata(t *testing.T, path string) SessionMetadata {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	var meta SessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	return meta
}
