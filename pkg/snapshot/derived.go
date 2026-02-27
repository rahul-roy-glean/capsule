package snapshot

import "time"

// BuildDerivedMetadata creates a derived ChunkedSnapshotMetadata by combining
// base kernel/state, merged memory chunks, merged rootfs chunks, and new extension drives.
//
// The derived metadata shares the kernel and state hashes from the base but has
// fresh memory/rootfs chunk lists (which include dirty pages from the mini-VM run
// merged on top of the base pages) and a new set of extension drives.
//
// memChunks and rootfsChunks must be fully-merged dense arrays covering the full
// logical size (e.g. produced by SessionChunkUploader.MergeAndUploadMem/Disk).
// extensions is the complete set of extension drives for the derived workload.
func BuildDerivedMetadata(
	base *ChunkedSnapshotMetadata,
	derivedWorkloadKey string,
	memChunks []ChunkRef,
	rootfsChunks []ChunkRef,
	extensions map[string]ExtensionDrive,
) *ChunkedSnapshotMetadata {
	// Compute total sizes from the chunk arrays.
	var totalMem, totalDisk int64
	for _, c := range memChunks {
		if end := c.Offset + c.Size; end > totalMem {
			totalMem = end
		}
	}
	for _, c := range rootfsChunks {
		if end := c.Offset + c.Size; end > totalDisk {
			totalDisk = end
		}
	}

	derived := &ChunkedSnapshotMetadata{
		Version:     base.Version,
		WorkloadKey: derivedWorkloadKey,
		CreatedAt:   time.Now(),
		ChunkSize:   base.ChunkSize,
		// Kernel and state are inherited from the base snapshot (unchanged).
		KernelHash:      base.KernelHash,
		StateHash:       base.StateHash,
		TotalMemSize:    totalMem,
		TotalDiskSize:   totalDisk,
		MemChunks:       memChunks,
		RootfsChunks:    rootfsChunks,
		ExtensionDrives: extensions,
	}

	return derived
}
