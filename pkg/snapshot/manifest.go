package snapshot

import "time"

// DiffSegment describes a contiguous data region in a memory diff blob.
type DiffSegment struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// SnapshotManifest is the top-level restore contract written during session pause.
// It lives at {gcsBase}/snapshot_manifest.json and references all other objects.
type SnapshotManifest struct {
	Version     string    `json:"version"`
	SnapshotID  string    `json:"snapshot_id"`
	CreatedAt   time.Time `json:"created_at"`
	WorkloadKey string    `json:"workload_key"`
	Firecracker struct {
		VMStateObject string `json:"vmstate_object"`
	} `json:"firecracker"`
	Memory struct {
		Mode                  string        `json:"mode"`
		TotalSizeBytes        int64         `json:"total_size_bytes"`
		ChunkIndexObject      string        `json:"chunk_index_object"`                // chunked mode
		DiffBlobObject        string        `json:"diff_blob_object,omitempty"`        // diff_file mode
		DiffSegments          []DiffSegment `json:"diff_segments,omitempty"`           // diff_file mode
		BaseChunkIndexObject  string        `json:"base_chunk_index_object,omitempty"` // diff_file mode
		PrefetchMappingObject string        `json:"prefetch_mapping_object,omitempty"` // diff_file mode
	} `json:"memory"`
	// Disk covers the rootfs dirty overlay.
	Disk DiskSection `json:"disk"`
	// ExtensionDisks covers per-drive dirty overlays for writable extension drives.
	ExtensionDisks map[string]DiskSection `json:"extension_disks,omitempty"`
	Integrity      struct {
		Algo string `json:"algo"`
	} `json:"integrity"`
}

// DiskSection describes a single disk region in a SnapshotManifest.
type DiskSection struct {
	Mode             string `json:"mode,omitempty"`
	TotalSizeBytes   int64  `json:"total_size_bytes,omitempty"`
	ChunkIndexObject string `json:"chunk_index_object,omitempty"`
}

// ChunkIndex is the session-specific memory/disk index written during pause.
// It is self-contained: only dirty/non-zero extents are listed; holes are implicit zeros.
// Lives at {gcsBase}/chunked-metadata.json (mem) or {gcsBase}/disk-chunked-metadata.json (disk).
type ChunkIndex struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	CAS       struct {
		Algo   string `json:"algo"`
		Layout string `json:"layout"` // e.g. "chunks/mem/{p0}/{hash}"
		Kind   string `json:"kind"`   // "mem" or "disk"
	} `json:"cas"`
	ChunkSizeBytes int64 `json:"chunk_size_bytes"`
	Region         struct {
		Name             string             `json:"name"`
		LogicalSizeBytes int64              `json:"logical_size_bytes"`
		Coverage         string             `json:"coverage"`     // "sparse"
		DefaultFill      string             `json:"default_fill"` // "zero"
		Extents          []ManifestChunkRef `json:"extents"`
	} `json:"region"`
	// PrefetchMapping records page fault access order from a previous run
	// for replay during subsequent resumes (access-pattern prefetching).
	PrefetchMapping *PrefetchMapping `json:"prefetch_mapping,omitempty"`
}

// ManifestChunkRef is a reference to a single extent in a ChunkIndex.
type ManifestChunkRef struct {
	Offset       int64  `json:"offset"`
	Length       int64  `json:"length"`
	Hash         string `json:"hash"`
	StoredLength int64  `json:"stored_length,omitempty"`
}

// ChunkIndexToMetadata converts a session ChunkIndex into ChunkedSnapshotMetadata
// so the existing uffd.Handler can be reused without modification.
// The resulting metadata has MemChunks populated; all other fields are zero/empty.
func ChunkIndexToMetadata(idx *ChunkIndex) *ChunkedSnapshotMetadata {
	meta := &ChunkedSnapshotMetadata{
		Version:      idx.Version,
		CreatedAt:    idx.CreatedAt,
		ChunkSize:    idx.ChunkSizeBytes,
		TotalMemSize: idx.Region.LogicalSizeBytes,
	}

	// Build a dense MemChunks array covering the full logical size.
	// Extents from ChunkIndex cover only dirty/non-zero regions; gaps between
	// extents become zero chunks (Hash="").
	totalSize := idx.Region.LogicalSizeBytes
	chunkSize := idx.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	numChunks := (totalSize + chunkSize - 1) / chunkSize
	refs := make([]ChunkRef, numChunks)

	// Fill all slots as zero chunks first.
	for i := int64(0); i < numChunks; i++ {
		offset := i * chunkSize
		size := chunkSize
		if offset+size > totalSize {
			size = totalSize - offset
		}
		refs[i] = ChunkRef{
			Offset: offset,
			Size:   size,
			Hash:   ZeroChunkHash,
		}
	}

	// Overlay the non-zero extents from the ChunkIndex.
	for _, ext := range idx.Region.Extents {
		if ext.Hash == ZeroChunkHash {
			continue
		}
		chunkIdx := ext.Offset / chunkSize
		if chunkIdx >= numChunks {
			continue
		}
		refs[chunkIdx] = ChunkRef{
			Offset:         ext.Offset,
			Size:           ext.Length,
			CompressedSize: ext.StoredLength,
			Hash:           ext.Hash,
		}
	}

	meta.MemChunks = refs
	meta.MemPrefetchMapping = idx.PrefetchMapping
	return meta
}

// ChunkIndexFromMeta builds a base ChunkIndex from an existing ChunkedSnapshotMetadata.
// Used as the base when no previous session ChunkIndex exists.
// All non-zero MemChunks are included as extents.
func ChunkIndexFromMeta(meta *ChunkedSnapshotMetadata) *ChunkIndex {
	idx := &ChunkIndex{
		Version:        meta.Version,
		CreatedAt:      meta.CreatedAt,
		ChunkSizeBytes: meta.ChunkSize,
	}
	idx.CAS.Algo = "sha256"
	idx.CAS.Layout = "chunks/mem/{p0}/{hash}"
	idx.CAS.Kind = "mem"
	idx.Region.Name = "vm_memory"
	idx.Region.LogicalSizeBytes = meta.TotalMemSize
	idx.Region.Coverage = "sparse"
	idx.Region.DefaultFill = "zero"

	for _, ref := range meta.MemChunks {
		if ref.Hash == ZeroChunkHash {
			continue
		}
		idx.Region.Extents = append(idx.Region.Extents, ManifestChunkRef{
			Offset:       ref.Offset,
			Length:       ref.Size,
			Hash:         ref.Hash,
			StoredLength: ref.CompressedSize,
		})
	}

	return idx
}

// ChunkIndexToRefs converts a ChunkIndex's sparse extents into a dense
// []ChunkRef array covering the full logical size. Gaps are zero chunks.
// This is the underlying conversion used by ChunkIndexToMetadata; use this
// when you need []ChunkRef directly (e.g. for FUSE disk setup).
func ChunkIndexToRefs(idx *ChunkIndex) []ChunkRef {
	return ChunkIndexToMetadata(idx).MemChunks
}
