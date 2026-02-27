package snapshot

import (
	"testing"
	"time"
)

func TestBuildDerivedMetadata_PreservesBaseKernelAndState(t *testing.T) {
	base := &ChunkedSnapshotMetadata{
		Version:       "1",
		WorkloadKey:   "base123",
		CreatedAt:     time.Now(),
		ChunkSize:     DefaultChunkSize,
		KernelHash:    "kernel-hash-abc",
		StateHash:     "state-hash-def",
		TotalMemSize:  512 * 1024 * 1024,
		TotalDiskSize: 10 * 1024 * 1024 * 1024,
		MemChunks:     []ChunkRef{{Offset: 0, Size: DefaultChunkSize, Hash: "mem-chunk-1"}},
		RootfsChunks:  []ChunkRef{{Offset: 0, Size: DefaultChunkSize, Hash: "disk-chunk-1"}},
	}

	derived := BuildDerivedMetadata(base, "derived456", base.MemChunks, base.RootfsChunks, nil)

	if derived.KernelHash != base.KernelHash {
		t.Errorf("KernelHash = %q, want %q", derived.KernelHash, base.KernelHash)
	}
	if derived.StateHash != base.StateHash {
		t.Errorf("StateHash = %q, want %q", derived.StateHash, base.StateHash)
	}
	if derived.WorkloadKey != "derived456" {
		t.Errorf("WorkloadKey = %q, want %q", derived.WorkloadKey, "derived456")
	}
	if derived.ChunkSize != base.ChunkSize {
		t.Errorf("ChunkSize = %d, want %d", derived.ChunkSize, base.ChunkSize)
	}
}

func TestBuildDerivedMetadata_MergedMemoryChunks(t *testing.T) {
	chunkSize := int64(DefaultChunkSize)
	// Base has 10 memory chunks.
	baseMemChunks := make([]ChunkRef, 10)
	for i := range baseMemChunks {
		baseMemChunks[i] = ChunkRef{
			Offset: int64(i) * chunkSize,
			Size:   chunkSize,
			Hash:   "base-mem-" + string(rune('a'+i)),
		}
	}

	base := &ChunkedSnapshotMetadata{
		Version:      "1",
		ChunkSize:    chunkSize,
		TotalMemSize: int64(len(baseMemChunks)) * chunkSize,
		MemChunks:    baseMemChunks,
	}

	// Mini-VM dirtied chunks 2, 5, and 7 — new hashes.
	mergedChunks := make([]ChunkRef, 10)
	copy(mergedChunks, baseMemChunks)
	mergedChunks[2] = ChunkRef{Offset: 2 * chunkSize, Size: chunkSize, Hash: "dirty-2"}
	mergedChunks[5] = ChunkRef{Offset: 5 * chunkSize, Size: chunkSize, Hash: "dirty-5"}
	mergedChunks[7] = ChunkRef{Offset: 7 * chunkSize, Size: chunkSize, Hash: "dirty-7"}

	derived := BuildDerivedMetadata(base, "derived", mergedChunks, nil, nil)

	if len(derived.MemChunks) != 10 {
		t.Fatalf("MemChunks count = %d, want 10", len(derived.MemChunks))
	}

	// Verify 3 dirty chunks have new hashes.
	dirtyExpected := map[int]string{2: "dirty-2", 5: "dirty-5", 7: "dirty-7"}
	for idx, wantHash := range dirtyExpected {
		if derived.MemChunks[idx].Hash != wantHash {
			t.Errorf("MemChunks[%d].Hash = %q, want %q", idx, derived.MemChunks[idx].Hash, wantHash)
		}
	}

	// Verify 7 unchanged chunks retained original hashes.
	for i, c := range derived.MemChunks {
		if _, dirty := dirtyExpected[i]; dirty {
			continue
		}
		if c.Hash != baseMemChunks[i].Hash {
			t.Errorf("MemChunks[%d].Hash = %q, want %q (unchanged)", i, c.Hash, baseMemChunks[i].Hash)
		}
	}
}

func TestBuildDerivedMetadata_ExtensionDrives(t *testing.T) {
	base := &ChunkedSnapshotMetadata{
		Version:   "1",
		ChunkSize: DefaultChunkSize,
	}

	extensions := map[string]ExtensionDrive{
		"git_drive": {
			ReadOnly:  false,
			SizeBytes: 10 * 1024 * 1024 * 1024,
			Chunks:    []ChunkRef{{Offset: 0, Size: DefaultChunkSize, Hash: "git-chunk-1"}},
		},
	}

	derived := BuildDerivedMetadata(base, "derived", nil, nil, extensions)

	if len(derived.ExtensionDrives) != 1 {
		t.Fatalf("ExtensionDrives count = %d, want 1", len(derived.ExtensionDrives))
	}
	gitDrive, ok := derived.ExtensionDrives["git_drive"]
	if !ok {
		t.Fatal("git_drive not found in derived ExtensionDrives")
	}
	if len(gitDrive.Chunks) != 1 {
		t.Errorf("git_drive.Chunks count = %d, want 1", len(gitDrive.Chunks))
	}
	if gitDrive.Chunks[0].Hash != "git-chunk-1" {
		t.Errorf("git_drive.Chunks[0].Hash = %q, want %q", gitDrive.Chunks[0].Hash, "git-chunk-1")
	}
}

func TestBuildDerivedMetadata_TotalSizesFromChunks(t *testing.T) {
	chunkSize := int64(DefaultChunkSize)
	memChunks := []ChunkRef{
		{Offset: 0, Size: chunkSize, Hash: "m0"},
		{Offset: chunkSize, Size: chunkSize, Hash: "m1"},
		{Offset: 2 * chunkSize, Size: 512, Hash: "m2-tail"}, // partial last chunk
	}
	rootfsChunks := []ChunkRef{
		{Offset: 0, Size: chunkSize, Hash: "d0"},
		{Offset: chunkSize, Size: 256, Hash: "d1-tail"}, // partial last chunk
	}
	base := &ChunkedSnapshotMetadata{Version: "1", ChunkSize: chunkSize}

	derived := BuildDerivedMetadata(base, "d", memChunks, rootfsChunks, nil)

	wantMem := 2*chunkSize + 512
	if derived.TotalMemSize != wantMem {
		t.Errorf("TotalMemSize = %d, want %d", derived.TotalMemSize, wantMem)
	}
	wantDisk := chunkSize + 256
	if derived.TotalDiskSize != wantDisk {
		t.Errorf("TotalDiskSize = %d, want %d", derived.TotalDiskSize, wantDisk)
	}
}

func TestComputeDerivedWorkloadKey_Deterministic(t *testing.T) {
	driveSpecs1 := []DriveSpec{
		{DriveID: "git_drive", Label: "GIT", SizeGB: 10},
		{DriveID: "bazel_cache", Label: "BAZEL", SizeGB: 20},
	}
	// Same specs in different order.
	driveSpecs2 := []DriveSpec{
		{DriveID: "bazel_cache", Label: "BAZEL", SizeGB: 20},
		{DriveID: "git_drive", Label: "GIT", SizeGB: 10},
	}

	key1 := ComputeDerivedWorkloadKey("base123", driveSpecs1)
	key2 := ComputeDerivedWorkloadKey("base123", driveSpecs2)

	if key1 != key2 {
		t.Errorf("Keys differ with same content in different order: %q vs %q", key1, key2)
	}

	// Different content → different key.
	driveSpecs3 := []DriveSpec{
		{DriveID: "git_drive", Label: "GIT", SizeGB: 20}, // SizeGB changed
	}
	key3 := ComputeDerivedWorkloadKey("base123", driveSpecs3)
	if key1 == key3 {
		t.Error("Expected different keys for different drive specs")
	}

	// Different base key → different key.
	key4 := ComputeDerivedWorkloadKey("otherbase", driveSpecs1)
	if key1 == key4 {
		t.Error("Expected different keys for different base workload keys")
	}

	// Key length is always 16 chars.
	if len(key1) != 16 {
		t.Errorf("Key length = %d, want 16", len(key1))
	}
}

// rune helper to make the test compile without importing unicode packages.
var _ = rune(0)
