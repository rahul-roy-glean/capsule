package snapshot

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// IncrementalUploader handles incremental snapshot uploads
// It only uploads chunks that have changed since the last snapshot
type IncrementalUploader struct {
	store  *ChunkStore
	logger *logrus.Entry
}

// NewIncrementalUploader creates a new incremental uploader
func NewIncrementalUploader(store *ChunkStore, logger *logrus.Logger) *IncrementalUploader {
	return &IncrementalUploader{
		store:  store,
		logger: logger.WithField("component", "incremental-uploader"),
	}
}

// DirtyChunks represents chunks that have been modified during a workload
type DirtyChunks struct {
	// ChunkIndex -> modified data
	Data map[int][]byte
}

// UploadIncrementalSnapshot creates a new snapshot version with only dirty chunks uploaded
func (u *IncrementalUploader) UploadIncrementalSnapshot(
	ctx context.Context,
	baseMeta *ChunkedSnapshotMetadata,
	dirtyDiskChunks map[int][]byte, // Dirty rootfs chunks from FUSE disk
	dirtyMemChunks map[int][]byte, // Dirty memory chunks (if doing memory diff)
	newVersion string,
) (*ChunkedSnapshotMetadata, error) {
	u.logger.WithFields(logrus.Fields{
		"base_version":      baseMeta.Version,
		"new_version":       newVersion,
		"dirty_disk_chunks": len(dirtyDiskChunks),
		"dirty_mem_chunks":  len(dirtyMemChunks),
	}).Info("Creating incremental snapshot")

	start := time.Now()

	// Create new metadata based on base
	newMeta := &ChunkedSnapshotMetadata{
		Version:       newVersion,
		BazelVersion:  baseMeta.BazelVersion,
		RepoCommit:    baseMeta.RepoCommit,
		CreatedAt:     time.Now(),
		ChunkSize:     baseMeta.ChunkSize,
		KernelHash:    baseMeta.KernelHash, // Kernel doesn't change
		StateHash:     baseMeta.StateHash,  // State might need update for paused state
		TotalMemSize:  baseMeta.TotalMemSize,
		TotalDiskSize: baseMeta.TotalDiskSize,
	}

	// Process disk chunks - copy base and update dirty ones
	newMeta.RootfsChunks = make([]ChunkRef, len(baseMeta.RootfsChunks))
	copy(newMeta.RootfsChunks, baseMeta.RootfsChunks)

	dirtyDiskCount := 0
	for idx, data := range dirtyDiskChunks {
		if idx >= len(newMeta.RootfsChunks) {
			// Extend if needed
			for len(newMeta.RootfsChunks) <= idx {
				newMeta.RootfsChunks = append(newMeta.RootfsChunks, ChunkRef{
					Offset: int64(len(newMeta.RootfsChunks)) * baseMeta.ChunkSize,
					Size:   baseMeta.ChunkSize,
				})
			}
		}

		// Upload dirty chunk
		hash, compressedSize, err := u.store.StoreChunk(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("failed to store dirty disk chunk %d: %w", idx, err)
		}

		newMeta.RootfsChunks[idx].Hash = hash
		newMeta.RootfsChunks[idx].CompressedSize = compressedSize
		dirtyDiskCount++

		u.logger.WithFields(logrus.Fields{
			"chunk_idx": idx,
			"hash":      hash[:12],
		}).Debug("Uploaded dirty disk chunk")
	}

	// Process memory chunks - copy base and update dirty ones
	newMeta.MemChunks = make([]ChunkRef, len(baseMeta.MemChunks))
	copy(newMeta.MemChunks, baseMeta.MemChunks)

	dirtyMemCount := 0
	for idx, data := range dirtyMemChunks {
		if idx >= len(newMeta.MemChunks) {
			// Extend if needed
			for len(newMeta.MemChunks) <= idx {
				newMeta.MemChunks = append(newMeta.MemChunks, ChunkRef{
					Offset: int64(len(newMeta.MemChunks)) * baseMeta.ChunkSize,
					Size:   baseMeta.ChunkSize,
				})
			}
		}

		// Upload dirty chunk
		hash, compressedSize, err := u.store.StoreChunk(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("failed to store dirty memory chunk %d: %w", idx, err)
		}

		newMeta.MemChunks[idx].Hash = hash
		newMeta.MemChunks[idx].CompressedSize = compressedSize
		dirtyMemCount++

		u.logger.WithFields(logrus.Fields{
			"chunk_idx": idx,
			"hash":      hash[:12],
		}).Debug("Uploaded dirty memory chunk")
	}

	// Copy repo cache seed chunks (unchanged)
	if len(baseMeta.RepoCacheSeedChunks) > 0 {
		newMeta.RepoCacheSeedChunks = make([]ChunkRef, len(baseMeta.RepoCacheSeedChunks))
		copy(newMeta.RepoCacheSeedChunks, baseMeta.RepoCacheSeedChunks)
	}

	duration := time.Since(start)

	// Calculate deduplication ratio
	totalChunks := len(newMeta.RootfsChunks) + len(newMeta.MemChunks)
	uploadedChunks := dirtyDiskCount + dirtyMemCount
	dedupRatio := 1.0
	if totalChunks > 0 {
		dedupRatio = 1.0 - float64(uploadedChunks)/float64(totalChunks)
	}

	u.logger.WithFields(logrus.Fields{
		"new_version":  newVersion,
		"duration":     duration,
		"dirty_disk":   dirtyDiskCount,
		"dirty_mem":    dirtyMemCount,
		"total_chunks": totalChunks,
		"dedup_ratio":  fmt.Sprintf("%.1f%%", dedupRatio*100),
	}).Info("Incremental snapshot created successfully")

	return newMeta, nil
}

// CompareSnapshots compares two snapshots and returns which chunks differ
func (u *IncrementalUploader) CompareSnapshots(
	ctx context.Context,
	oldMeta, newMeta *ChunkedSnapshotMetadata,
) (*SnapshotDiff, error) {
	diff := &SnapshotDiff{
		OldVersion: oldMeta.Version,
		NewVersion: newMeta.Version,
	}

	// Compare disk chunks
	maxDisk := len(oldMeta.RootfsChunks)
	if len(newMeta.RootfsChunks) > maxDisk {
		maxDisk = len(newMeta.RootfsChunks)
	}

	for i := 0; i < maxDisk; i++ {
		var oldHash, newHash string
		if i < len(oldMeta.RootfsChunks) {
			oldHash = oldMeta.RootfsChunks[i].Hash
		}
		if i < len(newMeta.RootfsChunks) {
			newHash = newMeta.RootfsChunks[i].Hash
		}

		if oldHash != newHash {
			diff.ChangedDiskChunks = append(diff.ChangedDiskChunks, i)
		}
	}

	// Compare memory chunks
	maxMem := len(oldMeta.MemChunks)
	if len(newMeta.MemChunks) > maxMem {
		maxMem = len(newMeta.MemChunks)
	}

	for i := 0; i < maxMem; i++ {
		var oldHash, newHash string
		if i < len(oldMeta.MemChunks) {
			oldHash = oldMeta.MemChunks[i].Hash
		}
		if i < len(newMeta.MemChunks) {
			newHash = newMeta.MemChunks[i].Hash
		}

		if oldHash != newHash {
			diff.ChangedMemChunks = append(diff.ChangedMemChunks, i)
		}
	}

	return diff, nil
}

// SnapshotDiff represents the difference between two snapshots
type SnapshotDiff struct {
	OldVersion        string
	NewVersion        string
	ChangedDiskChunks []int
	ChangedMemChunks  []int
}

// Summary returns a human-readable summary of the diff
func (d *SnapshotDiff) Summary() string {
	return fmt.Sprintf("Snapshot diff %s -> %s: %d disk chunks changed, %d memory chunks changed",
		d.OldVersion, d.NewVersion, len(d.ChangedDiskChunks), len(d.ChangedMemChunks))
}

// UpdateSnapshotPointer updates the "current" pointer to a new chunked snapshot version
func (u *IncrementalUploader) UpdateSnapshotPointer(ctx context.Context, meta *ChunkedSnapshotMetadata) error {
	// Upload metadata to both version-specific and "current" locations
	builder := NewChunkedSnapshotBuilder(u.store, u.logger.Logger)

	// Upload to versioned location
	if err := builder.UploadChunkedMetadata(ctx, meta); err != nil {
		return fmt.Errorf("failed to upload versioned metadata: %w", err)
	}

	// Upload to "current" location
	currentMeta := *meta
	currentMeta.Version = "current"
	if err := builder.UploadChunkedMetadata(ctx, &currentMeta); err != nil {
		return fmt.Errorf("failed to upload current metadata: %w", err)
	}

	u.logger.WithField("version", meta.Version).Info("Updated current snapshot pointer")
	return nil
}

// GarbageCollect removes chunks that are not referenced by any snapshot
// This should be called periodically to clean up old chunks
func (u *IncrementalUploader) GarbageCollect(ctx context.Context, keepVersions []string) error {
	u.logger.WithField("keep_versions", keepVersions).Info("Starting garbage collection")

	// TODO: Implement garbage collection
	// 1. Load metadata for all versions to keep
	// 2. Build set of all referenced chunk hashes
	// 3. List all chunks in storage
	// 4. Delete chunks not in referenced set

	u.logger.Warn("Garbage collection not yet implemented")
	return nil
}
