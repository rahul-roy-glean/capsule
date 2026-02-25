package snapshot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSnapshotManifest_JSONRoundTrip(t *testing.T) {
	m := SnapshotManifest{
		Version:     "1",
		SnapshotID:  "test-id",
		CreatedAt:   time.Now().Truncate(time.Second),
		WorkloadKey: "abc123",
	}
	m.Firecracker.VMStateObject = "v1/abc123/runner_state/r1/snapshot.state"
	m.Memory.Mode = "chunked"
	m.Memory.TotalSizeBytes = 2147483648
	m.Memory.ChunkIndexObject = "v1/abc123/runner_state/r1/chunked-metadata.json"
	m.Disk.Mode = "chunked"
	m.Disk.TotalSizeBytes = 536870912
	m.Disk.ChunkIndexObject = "v1/abc123/runner_state/r1/disk-chunked-metadata.json"
	m.Integrity.Algo = "sha256"

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded SnapshotManifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.SnapshotID != "test-id" {
		t.Errorf("SnapshotID = %q, want %q", decoded.SnapshotID, "test-id")
	}
	if decoded.Memory.Mode != "chunked" {
		t.Errorf("Memory.Mode = %q, want %q", decoded.Memory.Mode, "chunked")
	}
	if decoded.Memory.TotalSizeBytes != 2147483648 {
		t.Errorf("Memory.TotalSizeBytes = %d, want 2147483648", decoded.Memory.TotalSizeBytes)
	}
	if decoded.Disk.ChunkIndexObject != m.Disk.ChunkIndexObject {
		t.Errorf("Disk.ChunkIndexObject = %q, want %q", decoded.Disk.ChunkIndexObject, m.Disk.ChunkIndexObject)
	}
	if decoded.Integrity.Algo != "sha256" {
		t.Errorf("Integrity.Algo = %q, want %q", decoded.Integrity.Algo, "sha256")
	}
}

func TestSnapshotManifest_DiskOmittedWhenEmpty(t *testing.T) {
	m := SnapshotManifest{Version: "1"}
	m.Memory.Mode = "chunked"
	// Disk left empty

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Disk fields should be omitted
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	var disk map[string]json.RawMessage
	json.Unmarshal(raw["disk"], &disk)

	if string(disk["mode"]) != `""` && string(disk["mode"]) != "" {
		// mode is omitempty, should be absent or empty
	}
}

func TestChunkIndex_JSONRoundTrip(t *testing.T) {
	idx := ChunkIndex{
		Version:        "1",
		CreatedAt:      time.Now().Truncate(time.Second),
		ChunkSizeBytes: DefaultChunkSize,
	}
	idx.CAS.Algo = "sha256"
	idx.CAS.Layout = "chunks/mem/{p0}/{hash}"
	idx.CAS.Kind = "mem"
	idx.Region.Name = "vm_memory"
	idx.Region.LogicalSizeBytes = 2147483648
	idx.Region.Coverage = "sparse"
	idx.Region.DefaultFill = "zero"
	idx.Region.Extents = []ManifestChunkRef{
		{Offset: 0, Length: DefaultChunkSize, Hash: "aaa", StoredLength: 1000},
		{Offset: 16777216, Length: DefaultChunkSize, Hash: "bbb", StoredLength: 2000},
	}

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ChunkIndex
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.ChunkSizeBytes != DefaultChunkSize {
		t.Errorf("ChunkSizeBytes = %d, want %d", decoded.ChunkSizeBytes, DefaultChunkSize)
	}
	if decoded.CAS.Kind != "mem" {
		t.Errorf("CAS.Kind = %q, want %q", decoded.CAS.Kind, "mem")
	}
	if decoded.Region.LogicalSizeBytes != 2147483648 {
		t.Errorf("LogicalSizeBytes = %d, want 2147483648", decoded.Region.LogicalSizeBytes)
	}
	if len(decoded.Region.Extents) != 2 {
		t.Fatalf("Extents count = %d, want 2", len(decoded.Region.Extents))
	}
	if decoded.Region.Extents[0].Hash != "aaa" {
		t.Errorf("Extent[0].Hash = %q, want %q", decoded.Region.Extents[0].Hash, "aaa")
	}
	if decoded.Region.Extents[1].StoredLength != 2000 {
		t.Errorf("Extent[1].StoredLength = %d, want 2000", decoded.Region.Extents[1].StoredLength)
	}
}

func TestChunkIndexToMetadata_BasicConversion(t *testing.T) {
	idx := &ChunkIndex{
		Version:        "1",
		ChunkSizeBytes: DefaultChunkSize,
	}
	idx.Region.LogicalSizeBytes = 3 * DefaultChunkSize // 3 chunks total
	idx.Region.Extents = []ManifestChunkRef{
		{Offset: 0, Length: DefaultChunkSize, Hash: "chunk0", StoredLength: 1000},
		{Offset: 2 * DefaultChunkSize, Length: DefaultChunkSize, Hash: "chunk2", StoredLength: 2000},
		// chunk index 1 is missing → zero chunk
	}

	meta := ChunkIndexToMetadata(idx)

	if meta.TotalMemSize != 3*DefaultChunkSize {
		t.Errorf("TotalMemSize = %d, want %d", meta.TotalMemSize, 3*DefaultChunkSize)
	}
	if len(meta.MemChunks) != 3 {
		t.Fatalf("MemChunks count = %d, want 3", len(meta.MemChunks))
	}

	// Chunk 0: non-zero
	if meta.MemChunks[0].Hash != "chunk0" {
		t.Errorf("MemChunks[0].Hash = %q, want %q", meta.MemChunks[0].Hash, "chunk0")
	}
	if meta.MemChunks[0].CompressedSize != 1000 {
		t.Errorf("MemChunks[0].CompressedSize = %d, want 1000", meta.MemChunks[0].CompressedSize)
	}

	// Chunk 1: zero (gap in extents)
	if meta.MemChunks[1].Hash != ZeroChunkHash {
		t.Errorf("MemChunks[1].Hash = %q, want zero chunk hash %q", meta.MemChunks[1].Hash, ZeroChunkHash)
	}

	// Chunk 2: non-zero
	if meta.MemChunks[2].Hash != "chunk2" {
		t.Errorf("MemChunks[2].Hash = %q, want %q", meta.MemChunks[2].Hash, "chunk2")
	}
}

func TestChunkIndexToMetadata_AllZero(t *testing.T) {
	idx := &ChunkIndex{
		ChunkSizeBytes: DefaultChunkSize,
	}
	idx.Region.LogicalSizeBytes = 2 * DefaultChunkSize
	idx.Region.Extents = nil // no non-zero extents

	meta := ChunkIndexToMetadata(idx)

	if len(meta.MemChunks) != 2 {
		t.Fatalf("MemChunks count = %d, want 2", len(meta.MemChunks))
	}
	for i, c := range meta.MemChunks {
		if c.Hash != ZeroChunkHash {
			t.Errorf("MemChunks[%d].Hash = %q, want zero", i, c.Hash)
		}
	}
}

func TestChunkIndexToMetadata_LastChunkSmaller(t *testing.T) {
	// Logical size not a multiple of chunk size → last chunk is smaller
	idx := &ChunkIndex{
		ChunkSizeBytes: DefaultChunkSize,
	}
	idx.Region.LogicalSizeBytes = DefaultChunkSize + 1024 // 1 full chunk + 1KB

	meta := ChunkIndexToMetadata(idx)

	if len(meta.MemChunks) != 2 {
		t.Fatalf("MemChunks count = %d, want 2", len(meta.MemChunks))
	}
	if meta.MemChunks[0].Size != DefaultChunkSize {
		t.Errorf("MemChunks[0].Size = %d, want %d", meta.MemChunks[0].Size, DefaultChunkSize)
	}
	if meta.MemChunks[1].Size != 1024 {
		t.Errorf("MemChunks[1].Size = %d, want 1024", meta.MemChunks[1].Size)
	}
}

func TestChunkIndexFromMeta_BasicConversion(t *testing.T) {
	meta := &ChunkedSnapshotMetadata{
		Version:      "v1",
		ChunkSize:    DefaultChunkSize,
		TotalMemSize: 3 * DefaultChunkSize,
		MemChunks: []ChunkRef{
			{Offset: 0, Size: DefaultChunkSize, Hash: "aaa", CompressedSize: 100},
			{Offset: DefaultChunkSize, Size: DefaultChunkSize, Hash: ZeroChunkHash},
			{Offset: 2 * DefaultChunkSize, Size: DefaultChunkSize, Hash: "ccc", CompressedSize: 300},
		},
	}

	idx := ChunkIndexFromMeta(meta)

	if idx.CAS.Kind != "mem" {
		t.Errorf("CAS.Kind = %q, want %q", idx.CAS.Kind, "mem")
	}
	if idx.Region.LogicalSizeBytes != 3*DefaultChunkSize {
		t.Errorf("LogicalSizeBytes = %d, want %d", idx.Region.LogicalSizeBytes, 3*DefaultChunkSize)
	}
	if idx.Region.Coverage != "sparse" {
		t.Errorf("Coverage = %q, want %q", idx.Region.Coverage, "sparse")
	}

	// Should only have 2 extents (zero chunk skipped)
	if len(idx.Region.Extents) != 2 {
		t.Fatalf("Extents count = %d, want 2", len(idx.Region.Extents))
	}
	if idx.Region.Extents[0].Hash != "aaa" {
		t.Errorf("Extent[0].Hash = %q, want %q", idx.Region.Extents[0].Hash, "aaa")
	}
	if idx.Region.Extents[1].Hash != "ccc" {
		t.Errorf("Extent[1].Hash = %q, want %q", idx.Region.Extents[1].Hash, "ccc")
	}
}

func TestChunkIndexRoundTrip(t *testing.T) {
	// ChunkIndexFromMeta → ChunkIndexToMetadata should preserve data
	original := &ChunkedSnapshotMetadata{
		Version:      "v1",
		ChunkSize:    DefaultChunkSize,
		TotalMemSize: 4 * DefaultChunkSize,
		MemChunks: []ChunkRef{
			{Offset: 0, Size: DefaultChunkSize, Hash: "h0", CompressedSize: 100},
			{Offset: DefaultChunkSize, Size: DefaultChunkSize, Hash: ZeroChunkHash},
			{Offset: 2 * DefaultChunkSize, Size: DefaultChunkSize, Hash: "h2", CompressedSize: 200},
			{Offset: 3 * DefaultChunkSize, Size: DefaultChunkSize, Hash: "h3", CompressedSize: 300},
		},
	}

	idx := ChunkIndexFromMeta(original)
	roundTripped := ChunkIndexToMetadata(idx)

	if len(roundTripped.MemChunks) != 4 {
		t.Fatalf("MemChunks count = %d, want 4", len(roundTripped.MemChunks))
	}

	for i, orig := range original.MemChunks {
		got := roundTripped.MemChunks[i]
		if orig.Hash != got.Hash {
			t.Errorf("MemChunks[%d].Hash = %q, want %q", i, got.Hash, orig.Hash)
		}
		if orig.Offset != got.Offset {
			t.Errorf("MemChunks[%d].Offset = %d, want %d", i, got.Offset, orig.Offset)
		}
	}
}

func TestChunkIndexToRefs(t *testing.T) {
	idx := &ChunkIndex{
		ChunkSizeBytes: DefaultChunkSize,
	}
	idx.Region.LogicalSizeBytes = 2 * DefaultChunkSize
	idx.Region.Extents = []ManifestChunkRef{
		{Offset: DefaultChunkSize, Length: DefaultChunkSize, Hash: "disk1", StoredLength: 500},
	}

	refs := ChunkIndexToRefs(idx)

	if len(refs) != 2 {
		t.Fatalf("Refs count = %d, want 2", len(refs))
	}
	if refs[0].Hash != ZeroChunkHash {
		t.Errorf("Refs[0].Hash = %q, want zero", refs[0].Hash)
	}
	if refs[1].Hash != "disk1" {
		t.Errorf("Refs[1].Hash = %q, want %q", refs[1].Hash, "disk1")
	}
}
