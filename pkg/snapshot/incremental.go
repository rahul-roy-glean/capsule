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

// UpdateSnapshotPointer uploads the versioned metadata for a new chunked snapshot version.
// The current-pointer.json mechanism is the canonical way to resolve the active version.
func (u *IncrementalUploader) UpdateSnapshotPointer(ctx context.Context, meta *ChunkedSnapshotMetadata) error {
	builder := NewChunkedSnapshotBuilder(u.store, nil, u.logger.Logger)

	if err := builder.UploadChunkedMetadata(ctx, meta); err != nil {
		return fmt.Errorf("failed to upload versioned metadata: %w", err)
	}

	u.logger.WithField("version", meta.Version).Info("Updated snapshot pointer")
	return nil
}

// GCConfig holds configuration for garbage collection.
type GCConfig struct {
	// MinChunkAge is the minimum age a chunk must have before it can be
	// deleted by GC. Chunks younger than this are always protected.
	// Default: 24h. This is the primary safety mechanism.
	MinChunkAge time.Duration
}

// DefaultGCConfig returns a GCConfig with sensible defaults.
func DefaultGCConfig() GCConfig {
	return GCConfig{MinChunkAge: 24 * time.Hour}
}

// CollectSessionRoots returns chunk hashes referenced by active session
// snapshots. This queries the session_snapshots table for sessions that
// are not expired or deleted, and collects all chunk hashes from their
// chunk indices. Returns nil if no DB is configured.
//
// TODO: implement when session_snapshots DB table is available.
func CollectSessionRoots(ctx context.Context) (map[string]bool, error) {
	return nil, nil
}

// GarbageCollect removes chunks that are not referenced by any of the keepVersions.
// It loads all chunk manifests for the versions to keep, builds a set of referenced
// hashes, lists all chunks in GCS, and deletes unreferenced ones.
//
// IMPORTANT: keepVersions must include versions from ALL repos that share this
// chunk store. Chunks are content-addressed and shared globally — a chunk
// referenced by repo B must not be deleted just because repo A deprecated its
// snapshot. The caller is responsible for collecting versions across all repos.
// Use GarbageCollectAllRepos for the safe multi-repo variant.
func (u *IncrementalUploader) GarbageCollect(ctx context.Context, workloadKey string, keepVersions []string, gcCfg GCConfig) error {
	u.logger.WithField("keep_versions", keepVersions).Info("Starting garbage collection")

	if len(keepVersions) == 0 {
		u.logger.Warn("No versions to keep, skipping GC")
		return nil
	}

	// 1. Load metadata for all versions to keep
	referencedHashes := make(map[string]bool)
	for _, version := range keepVersions {
		meta, err := u.store.LoadChunkedMetadata(ctx, workloadKey, version)
		if err != nil {
			u.logger.WithError(err).WithField("version", version).Warn("Failed to load metadata for GC, skipping version")
			continue
		}

		collectReferencedHashes(meta, referencedHashes)

		u.logger.WithFields(logrus.Fields{
			"version":           version,
			"referenced_chunks": len(referencedHashes),
		}).Debug("Loaded chunk references for version")
	}

	return u.deleteUnreferencedChunks(ctx, referencedHashes, gcCfg)
}

// GarbageCollectAllRepos is the safe multi-repo variant of GarbageCollect.
// It accepts a map of workload_key → []version_paths (GCS metadata paths) and
// builds the referenced set across ALL repos before deleting anything.
// This prevents repo A's GC from deleting chunks still referenced by repo B.
func (u *IncrementalUploader) GarbageCollectAllRepos(ctx context.Context, repoVersions map[string][]string, gcCfg GCConfig) error {
	u.logger.WithField("repos", len(repoVersions)).Info("Starting multi-repo garbage collection")

	referencedHashes := make(map[string]bool)
	totalVersions := 0

	for workloadKey, versions := range repoVersions {
		for _, version := range versions {
			meta, err := u.store.LoadChunkedMetadata(ctx, workloadKey, version)
			if err != nil {
				u.logger.WithError(err).WithFields(logrus.Fields{
					"workload_key": workloadKey,
					"version":      version,
				}).Warn("Failed to load metadata for GC, skipping version")
				continue
			}

			collectReferencedHashes(meta, referencedHashes)
			totalVersions++
		}
	}

	u.logger.WithFields(logrus.Fields{
		"repos":            len(repoVersions),
		"versions_scanned": totalVersions,
		"total_referenced": len(referencedHashes),
	}).Info("Built referenced chunk set across all repos")

	return u.deleteUnreferencedChunks(ctx, referencedHashes, gcCfg)
}

// collectReferencedHashes adds all chunk hashes from a metadata to the set.
func collectReferencedHashes(meta *ChunkedSnapshotMetadata, hashes map[string]bool) {
	if meta.KernelHash != "" {
		hashes[meta.KernelHash] = true
	}
	if meta.StateHash != "" {
		hashes[meta.StateHash] = true
	}
	for _, chunk := range meta.MemChunks {
		if chunk.Hash != "" && chunk.Hash != ZeroChunkHash {
			hashes[chunk.Hash] = true
		}
	}
	for _, chunk := range meta.RootfsChunks {
		if chunk.Hash != "" && chunk.Hash != ZeroChunkHash {
			hashes[chunk.Hash] = true
		}
	}
	for _, chunk := range meta.RepoCacheSeedChunks {
		if chunk.Hash != "" && chunk.Hash != ZeroChunkHash {
			hashes[chunk.Hash] = true
		}
	}
}

// deleteUnreferencedChunks lists all chunks in storage and deletes those
// not in the referenced set. Chunks younger than gcCfg.MinChunkAge are
// always protected regardless of reference status.
func (u *IncrementalUploader) deleteUnreferencedChunks(ctx context.Context, referencedHashes map[string]bool, gcCfg GCConfig) error {
	u.logger.WithField("total_referenced", len(referencedHashes)).Info("Built referenced chunk set")

	allChunks, err := u.store.ListChunks(ctx)
	if err != nil {
		return fmt.Errorf("failed to list chunks: %w", err)
	}

	u.logger.WithField("total_chunks", len(allChunks)).Info("Listed all chunks in storage")

	var toDelete []string
	for _, hash := range allChunks {
		if !referencedHashes[hash] {
			toDelete = append(toDelete, hash)
		}
	}

	if len(toDelete) == 0 {
		u.logger.Info("No unreferenced chunks to delete")
		return nil
	}

	u.logger.WithFields(logrus.Fields{
		"unreferenced": len(toDelete),
		"referenced":   len(referencedHashes),
		"total":        len(allChunks),
	}).Info("Deleting unreferenced chunks")

	deleted := 0
	skippedAge := 0
	for _, hash := range toDelete {
		// Check chunk age before deleting — young chunks are protected.
		if gcCfg.MinChunkAge > 0 {
			created, err := u.store.GetChunkCreationTime(ctx, hash)
			if err != nil {
				u.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to get chunk attrs, skipping deletion")
				skippedAge++
				continue
			}
			age := time.Since(created)
			if age < gcCfg.MinChunkAge {
				u.logger.WithFields(logrus.Fields{
					"hash": hash[:12],
					"age":  age,
				}).Debug("Skipping young chunk (below MinChunkAge)")
				skippedAge++
				continue
			}
		}

		if err := u.store.DeleteChunk(ctx, hash); err != nil {
			u.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to delete chunk")
			continue
		}
		deleted++
	}

	u.logger.WithFields(logrus.Fields{
		"deleted":     deleted,
		"skipped_age": skippedAge,
		"failed":      len(toDelete) - deleted - skippedAge,
	}).Info("Garbage collection complete")

	return nil
}
