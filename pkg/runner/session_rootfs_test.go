package runner

import (
	"encoding/json"
	"testing"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

func TestBuildRootfsDriveBaseIndex_NilMeta(t *testing.T) {
	idx := buildRootfsDriveBaseIndex(nil)

	if idx.Version != "1" {
		t.Errorf("Version = %q, want %q", idx.Version, "1")
	}
	if idx.CAS.Algo != "sha256" {
		t.Errorf("CAS.Algo = %q, want %q", idx.CAS.Algo, "sha256")
	}
	if idx.CAS.Kind != "disk" {
		t.Errorf("CAS.Kind = %q, want %q", idx.CAS.Kind, "disk")
	}
	if idx.CAS.Layout != "chunks/disk/{p0}/{hash}" {
		t.Errorf("CAS.Layout = %q, want %q", idx.CAS.Layout, "chunks/disk/{p0}/{hash}")
	}
	if idx.Region.Name != "__rootfs__" {
		t.Errorf("Region.Name = %q, want %q", idx.Region.Name, "__rootfs__")
	}
	if idx.Region.Coverage != "sparse" {
		t.Errorf("Region.Coverage = %q, want %q", idx.Region.Coverage, "sparse")
	}
	if idx.Region.DefaultFill != "zero" {
		t.Errorf("Region.DefaultFill = %q, want %q", idx.Region.DefaultFill, "zero")
	}
	if len(idx.Region.Extents) != 0 {
		t.Errorf("Extents should be empty for nil meta, got %d", len(idx.Region.Extents))
	}
	if idx.ChunkSizeBytes != snapshot.DefaultChunkSize {
		t.Errorf("ChunkSizeBytes = %d, want %d", idx.ChunkSizeBytes, snapshot.DefaultChunkSize)
	}
}

func TestBuildRootfsDriveBaseIndex_WithMeta(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize:     1024 * 1024, // 1MB
		TotalDiskSize: 3 * 1024 * 1024,
		RootfsChunks: []snapshot.ChunkRef{
			{Offset: 0, Size: 1024 * 1024, Hash: "rootfs-chunk-0", CompressedSize: 500},
			{Offset: 1024 * 1024, Size: 1024 * 1024, Hash: snapshot.ZeroChunkHash}, // zero → skipped
			{Offset: 2 * 1024 * 1024, Size: 1024 * 1024, Hash: "rootfs-chunk-2", CompressedSize: 700},
		},
	}

	idx := buildRootfsDriveBaseIndex(meta)

	if idx.ChunkSizeBytes != 1024*1024 {
		t.Errorf("ChunkSizeBytes = %d, want %d", idx.ChunkSizeBytes, 1024*1024)
	}
	if idx.Region.LogicalSizeBytes != 3*1024*1024 {
		t.Errorf("LogicalSizeBytes = %d, want %d", idx.Region.LogicalSizeBytes, 3*1024*1024)
	}

	// Should have 2 extents (zero chunk skipped)
	if len(idx.Region.Extents) != 2 {
		t.Fatalf("Extents count = %d, want 2", len(idx.Region.Extents))
	}
	if idx.Region.Extents[0].Hash != "rootfs-chunk-0" {
		t.Errorf("Extent[0].Hash = %q, want %q", idx.Region.Extents[0].Hash, "rootfs-chunk-0")
	}
	if idx.Region.Extents[0].StoredLength != 500 {
		t.Errorf("Extent[0].StoredLength = %d, want 500", idx.Region.Extents[0].StoredLength)
	}
	if idx.Region.Extents[1].Hash != "rootfs-chunk-2" {
		t.Errorf("Extent[1].Hash = %q, want %q", idx.Region.Extents[1].Hash, "rootfs-chunk-2")
	}
}

func TestBuildRootfsDriveBaseIndex_EmptyRootfsChunks(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize:     snapshot.DefaultChunkSize,
		TotalDiskSize: 10 * 1024 * 1024,
		RootfsChunks:  nil, // no rootfs chunks
	}

	idx := buildRootfsDriveBaseIndex(meta)

	if idx.Region.LogicalSizeBytes != 10*1024*1024 {
		t.Errorf("LogicalSizeBytes = %d, want %d", idx.Region.LogicalSizeBytes, 10*1024*1024)
	}
	if len(idx.Region.Extents) != 0 {
		t.Errorf("Extents should be empty for no rootfs chunks, got %d", len(idx.Region.Extents))
	}
}

func TestBuildRootfsDriveBaseIndex_AllZeroChunks(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize:     snapshot.DefaultChunkSize,
		TotalDiskSize: 2 * snapshot.DefaultChunkSize,
		RootfsChunks: []snapshot.ChunkRef{
			{Offset: 0, Size: snapshot.DefaultChunkSize, Hash: snapshot.ZeroChunkHash},
			{Offset: snapshot.DefaultChunkSize, Size: snapshot.DefaultChunkSize, Hash: snapshot.ZeroChunkHash},
		},
	}

	idx := buildRootfsDriveBaseIndex(meta)

	if len(idx.Region.Extents) != 0 {
		t.Errorf("All-zero chunks should produce 0 extents, got %d", len(idx.Region.Extents))
	}
}

func TestBuildExtensionDriveBaseIndex_DriveNotInMeta(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize: snapshot.DefaultChunkSize,
		ExtensionDrives: map[string]snapshot.ExtensionDrive{
			"other-drive": {SizeBytes: 1024},
		},
	}

	idx := buildExtensionDriveBaseIndex(meta, "missing-drive")

	if len(idx.Region.Extents) != 0 {
		t.Errorf("Extents should be empty for missing drive")
	}
}

// TestSessionMetadata_RootfsDiskIndex verifies that the __rootfs__ key
// in GCSDiskIndexObjects round-trips through JSON correctly and doesn't
// collide with extension drive keys.
func TestSessionMetadata_RootfsDiskIndex(t *testing.T) {
	meta := SessionMetadata{
		SessionID:   "sess-rootfs",
		WorkloadKey: "wk123",
		RunnerID:    "r1",
		HostID:      "h1",
		Layers:      1,
		GCSDiskIndexObjects: map[string]string{
			"__rootfs__": "v1/wk123/runner_state/r1/__rootfs__-disk.json",
			"ext-drive1": "v1/wk123/runner_state/r1/ext-drive1-disk.json",
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

	if decoded.GCSDiskIndexObjects["__rootfs__"] != meta.GCSDiskIndexObjects["__rootfs__"] {
		t.Errorf("__rootfs__ disk index = %q, want %q",
			decoded.GCSDiskIndexObjects["__rootfs__"],
			meta.GCSDiskIndexObjects["__rootfs__"])
	}
	if decoded.GCSDiskIndexObjects["ext-drive1"] != meta.GCSDiskIndexObjects["ext-drive1"] {
		t.Errorf("ext-drive1 disk index = %q, want %q",
			decoded.GCSDiskIndexObjects["ext-drive1"],
			meta.GCSDiskIndexObjects["ext-drive1"])
	}
	if len(decoded.GCSDiskIndexObjects) != 2 {
		t.Errorf("GCSDiskIndexObjects count = %d, want 2", len(decoded.GCSDiskIndexObjects))
	}
}

// TestDiskIndexCarryForward verifies the carry-forward logic for disk index
// objects doesn't lose rootfs or extension drive entries across pauses.
func TestDiskIndexCarryForward(t *testing.T) {
	// Simulate carry-forward: previous pause had rootfs + ext1 dirty,
	// this pause only ext2 is dirty. Rootfs and ext1 should carry forward.
	prevDiskIndexObjects := map[string]string{
		"__rootfs__": "v1/old-rootfs-disk.json",
		"ext1":       "v1/old-ext1-disk.json",
	}
	newDirtyDiskIndexes := map[string]bool{
		"ext2": true, // only ext2 is dirty this time
	}

	// Simulate carry-forward logic (mirrors session.go)
	result := make(map[string]string)
	for driveID, path := range prevDiskIndexObjects {
		if !newDirtyDiskIndexes[driveID] {
			result[driveID] = path
		}
	}
	for driveID := range newDirtyDiskIndexes {
		result[driveID] = "v1/new-" + driveID + "-disk.json"
	}

	// __rootfs__ and ext1 should carry forward, ext2 should be new
	if result["__rootfs__"] != "v1/old-rootfs-disk.json" {
		t.Errorf("__rootfs__ should carry forward, got %q", result["__rootfs__"])
	}
	if result["ext1"] != "v1/old-ext1-disk.json" {
		t.Errorf("ext1 should carry forward, got %q", result["ext1"])
	}
	if result["ext2"] != "v1/new-ext2-disk.json" {
		t.Errorf("ext2 should be new, got %q", result["ext2"])
	}
}

// TestDiskIndexCarryForward_RootfsDirtyOverwritesPrevious verifies that
// when rootfs is dirty in the current pause, it overwrites the previous entry.
func TestDiskIndexCarryForward_RootfsDirtyOverwritesPrevious(t *testing.T) {
	prevDiskIndexObjects := map[string]string{
		"__rootfs__": "v1/old-rootfs-disk.json",
	}
	newDirtyDiskIndexes := map[string]bool{
		"__rootfs__": true, // rootfs dirty this time
	}

	result := make(map[string]string)
	for driveID, path := range prevDiskIndexObjects {
		if !newDirtyDiskIndexes[driveID] {
			result[driveID] = path
		}
	}
	for driveID := range newDirtyDiskIndexes {
		result[driveID] = "v1/new-" + driveID + "-disk.json"
	}

	if result["__rootfs__"] != "v1/new-__rootfs__-disk.json" {
		t.Errorf("dirty rootfs should overwrite previous, got %q", result["__rootfs__"])
	}
}
