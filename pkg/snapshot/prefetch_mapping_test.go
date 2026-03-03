package snapshot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPrefetchMapping_JSONRoundTrip(t *testing.T) {
	pm := &PrefetchMapping{
		Offsets:   []int64{0, 4096, 8192, 16384},
		BlockSize: 4096,
	}

	data, err := json.Marshal(pm)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded PrefetchMapping
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.BlockSize != 4096 {
		t.Errorf("BlockSize = %d, want 4096", decoded.BlockSize)
	}
	if len(decoded.Offsets) != 4 {
		t.Fatalf("len(Offsets) = %d, want 4", len(decoded.Offsets))
	}
	for i, want := range pm.Offsets {
		if decoded.Offsets[i] != want {
			t.Errorf("Offsets[%d] = %d, want %d", i, decoded.Offsets[i], want)
		}
	}
}

func TestChunkedSnapshotMetadata_PrefetchMappingOmittedWhenNil(t *testing.T) {
	meta := ChunkedSnapshotMetadata{
		Version:   "1",
		ChunkSize: DefaultChunkSize,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)

	if _, exists := raw["mem_prefetch_mapping"]; exists {
		t.Error("mem_prefetch_mapping should be omitted when nil")
	}
}

func TestChunkedSnapshotMetadata_PrefetchMappingPresent(t *testing.T) {
	meta := ChunkedSnapshotMetadata{
		Version:   "1",
		ChunkSize: DefaultChunkSize,
		MemPrefetchMapping: &PrefetchMapping{
			Offsets:   []int64{0, 4096},
			BlockSize: 4096,
		},
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ChunkedSnapshotMetadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.MemPrefetchMapping == nil {
		t.Fatal("MemPrefetchMapping should not be nil after round-trip")
	}
	if len(decoded.MemPrefetchMapping.Offsets) != 2 {
		t.Errorf("len(Offsets) = %d, want 2", len(decoded.MemPrefetchMapping.Offsets))
	}
}

func TestChunkIndex_PrefetchMappingRoundTrip(t *testing.T) {
	idx := ChunkIndex{
		Version:        "1",
		CreatedAt:      time.Now().Truncate(time.Second),
		ChunkSizeBytes: DefaultChunkSize,
		PrefetchMapping: &PrefetchMapping{
			Offsets:   []int64{8192, 0, 4096},
			BlockSize: 4096,
		},
	}
	idx.CAS.Algo = "sha256"
	idx.CAS.Kind = "mem"
	idx.Region.LogicalSizeBytes = DefaultChunkSize

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded ChunkIndex
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.PrefetchMapping == nil {
		t.Fatal("PrefetchMapping should not be nil after round-trip")
	}
	if len(decoded.PrefetchMapping.Offsets) != 3 {
		t.Fatalf("len(Offsets) = %d, want 3", len(decoded.PrefetchMapping.Offsets))
	}
	if decoded.PrefetchMapping.Offsets[0] != 8192 {
		t.Errorf("Offsets[0] = %d, want 8192", decoded.PrefetchMapping.Offsets[0])
	}
}

func TestChunkIndex_PrefetchMappingOmittedWhenNil(t *testing.T) {
	idx := ChunkIndex{Version: "1"}

	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)

	if _, exists := raw["prefetch_mapping"]; exists {
		t.Error("prefetch_mapping should be omitted when nil")
	}
}

func TestChunkIndexToMetadata_CarriesPrefetchMapping(t *testing.T) {
	idx := &ChunkIndex{
		Version:        "1",
		ChunkSizeBytes: DefaultChunkSize,
		PrefetchMapping: &PrefetchMapping{
			Offsets:   []int64{0, 4096, 8192},
			BlockSize: 4096,
		},
	}
	idx.Region.LogicalSizeBytes = DefaultChunkSize

	meta := ChunkIndexToMetadata(idx)

	if meta.MemPrefetchMapping == nil {
		t.Fatal("MemPrefetchMapping should be carried through conversion")
	}
	if len(meta.MemPrefetchMapping.Offsets) != 3 {
		t.Errorf("len(Offsets) = %d, want 3", len(meta.MemPrefetchMapping.Offsets))
	}
}

func TestChunkIndexToMetadata_NilPrefetchMapping(t *testing.T) {
	idx := &ChunkIndex{
		ChunkSizeBytes: DefaultChunkSize,
	}
	idx.Region.LogicalSizeBytes = DefaultChunkSize

	meta := ChunkIndexToMetadata(idx)

	if meta.MemPrefetchMapping != nil {
		t.Error("MemPrefetchMapping should be nil when ChunkIndex has no mapping")
	}
}
