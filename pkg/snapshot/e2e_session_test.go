//go:build linux
// +build linux

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// memoryChunkStore is an in-memory ChunkStorer for testing.
// It records store counts so tests can verify dedup behavior.
type memoryChunkStore struct {
	mu          sync.Mutex
	stored      map[string][]byte // hash → uncompressed data
	storeCounts map[string]int    // hash → number of StoreChunk calls
}

func newMemoryChunkStore() *memoryChunkStore {
	return &memoryChunkStore{
		stored:      make(map[string][]byte),
		storeCounts: make(map[string]int),
	}
}

func (m *memoryChunkStore) StoreChunk(_ context.Context, data []byte) (string, int64, error) {
	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storeCounts[hash]++
	if _, exists := m.stored[hash]; !exists {
		cp := make([]byte, len(data))
		copy(cp, data)
		m.stored[hash] = cp
	}
	// Return data length as compressed size (no real compression in test).
	return hash, int64(len(data)), nil
}

func (m *memoryChunkStore) GetChunk(_ context.Context, hash string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.stored[hash]
	if !ok {
		return nil, fmt.Errorf("chunk %s not found", hash)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

// totalStores returns the total number of StoreChunk calls across all hashes.
func (m *memoryChunkStore) totalStores() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.storeCounts {
		n += c
	}
	return n
}

// storesSince returns how many new StoreChunk calls occurred since snapshot.
func (m *memoryChunkStore) storesSince(snapshot int) int {
	return m.totalStores() - snapshot
}

// maxStoreCount returns the max number of times any single hash was stored.
func (m *memoryChunkStore) maxStoreCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	max := 0
	for _, c := range m.storeCounts {
		if c > max {
			max = c
		}
	}
	return max
}

// makeChunkData returns a deterministic non-zero byte slice of the given size,
// seeded by the provided id so that different ids produce different hashes.
func makeChunkData(size int64, id string) []byte {
	data := make([]byte, size)
	seed := []byte(id)
	for i := range data {
		data[i] = seed[i%len(seed)] ^ byte(i)
	}
	return data
}

// seedStore pre-populates the memoryChunkStore with chunks and returns their
// ManifestChunkRef entries at sequential offsets.
func seedStore(store *memoryChunkStore, chunkSize int64, ids []string) []ManifestChunkRef {
	ctx := context.Background()
	extents := make([]ManifestChunkRef, len(ids))
	for i, id := range ids {
		data := makeChunkData(chunkSize, id)
		hash, compSz, _ := store.StoreChunk(ctx, data)
		extents[i] = ManifestChunkRef{
			Offset:       int64(i) * chunkSize,
			Length:       chunkSize,
			Hash:         hash,
			StoredLength: compSz,
		}
	}
	return extents
}

// makeBaseChunkIndex builds a ChunkIndex from extents.
func makeBaseChunkIndex(chunkSize int64, totalSize int64, extents []ManifestChunkRef, kind string) *ChunkIndex {
	idx := &ChunkIndex{
		Version:        "1",
		CreatedAt:      time.Now(),
		ChunkSizeBytes: chunkSize,
	}
	idx.CAS.Algo = "sha256"
	if kind == "disk" {
		idx.CAS.Layout = "chunks/disk/{p0}/{hash}"
		idx.CAS.Kind = "disk"
		idx.Region.Name = "vm_disk"
	} else {
		idx.CAS.Layout = "chunks/mem/{p0}/{hash}"
		idx.CAS.Kind = "mem"
		idx.Region.Name = "vm_memory"
	}
	idx.Region.LogicalSizeBytes = totalSize
	idx.Region.Coverage = "sparse"
	idx.Region.DefaultFill = "zero"
	idx.Region.Extents = extents
	return idx
}

func testLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	l.SetOutput(os.Stderr)
	return l
}

// --- Test 1: Disk dedup across multi-pause sessions ---

func TestE2E_SessionPauseChaining_DiskDedup(t *testing.T) {
	ctx := context.Background()
	chunkSize := int64(DefaultChunkSize)
	store := newMemoryChunkStore()

	// Pre-seed 5 base chunks.
	baseIDs := []string{"base-0", "base-1", "base-2", "base-3", "base-4"}
	baseExtents := seedStore(store, chunkSize, baseIDs)
	baseStores := store.totalStores()

	totalSize := int64(len(baseIDs)) * chunkSize
	baseIndex := makeBaseChunkIndex(chunkSize, totalSize, baseExtents, "disk")

	uploader := &SessionChunkUploader{
		memStore:  store,
		diskStore: store,
		logger:    testLogger().WithField("test", "disk-dedup"),
	}

	// --- Pause 1: 3 new dirty chunks at offsets 5, 6, 7 ---
	dirty1 := map[int][]byte{
		5: makeChunkData(chunkSize, "dirty-p1-5"),
		6: makeChunkData(chunkSize, "dirty-p1-6"),
		7: makeChunkData(chunkSize, "dirty-p1-7"),
	}
	// Extend logical size to cover new chunks.
	baseIndex.Region.LogicalSizeBytes = 8 * chunkSize

	idx1, err := uploader.MergeAndUploadDisk(ctx, dirty1, baseIndex)
	if err != nil {
		t.Fatalf("Pause 1 MergeAndUploadDisk: %v", err)
	}
	newStores1 := store.storesSince(baseStores)
	if newStores1 != 3 {
		t.Errorf("Pause 1: expected 3 new stores, got %d", newStores1)
	}
	if len(idx1.Region.Extents) != 8 {
		t.Errorf("Pause 1: expected 8 extents, got %d", len(idx1.Region.Extents))
	}

	// --- Pause 2: 1 new dirty chunk, use pause 1 result as base ---
	snapshot2 := store.totalStores()
	dirty2 := map[int][]byte{
		8: makeChunkData(chunkSize, "dirty-p2-8"),
	}
	idx1.Region.LogicalSizeBytes = 9 * chunkSize

	idx2, err := uploader.MergeAndUploadDisk(ctx, dirty2, idx1)
	if err != nil {
		t.Fatalf("Pause 2 MergeAndUploadDisk: %v", err)
	}
	newStores2 := store.storesSince(snapshot2)
	if newStores2 != 1 {
		t.Errorf("Pause 2: expected 1 new store, got %d", newStores2)
	}
	if len(idx2.Region.Extents) != 9 {
		t.Errorf("Pause 2: expected 9 extents, got %d", len(idx2.Region.Extents))
	}

	// --- Pause 3: 0 dirty chunks ---
	snapshot3 := store.totalStores()
	dirty3 := map[int][]byte{}
	idx3, err := uploader.MergeAndUploadDisk(ctx, dirty3, idx2)
	if err != nil {
		t.Fatalf("Pause 3 MergeAndUploadDisk: %v", err)
	}
	newStores3 := store.storesSince(snapshot3)
	if newStores3 != 0 {
		t.Errorf("Pause 3: expected 0 new stores, got %d", newStores3)
	}
	if len(idx3.Region.Extents) != 9 {
		t.Errorf("Pause 3: expected 9 extents, got %d", len(idx3.Region.Extents))
	}

	// Verify no hash was stored more than once.
	if max := store.maxStoreCount(); max > 1 {
		t.Errorf("Dedup failure: some hash was stored %d times (want max 1)", max)
	}
}

// --- Test 2: Multi-drive dedup ---

func TestE2E_SessionPauseChaining_MultiDriveDedup(t *testing.T) {
	ctx := context.Background()
	chunkSize := int64(DefaultChunkSize)

	gitStore := newMemoryChunkStore()
	bazelStore := newMemoryChunkStore()

	// git_drive: 5 base extents
	gitBaseExtents := seedStore(gitStore, chunkSize, []string{"git-0", "git-1", "git-2", "git-3", "git-4"})
	gitBaseIndex := makeBaseChunkIndex(chunkSize, 5*chunkSize, gitBaseExtents, "disk")

	// bazel_cache: 3 base extents
	bazelBaseExtents := seedStore(bazelStore, chunkSize, []string{"bazel-0", "bazel-1", "bazel-2"})
	bazelBaseIndex := makeBaseChunkIndex(chunkSize, 3*chunkSize, bazelBaseExtents, "disk")

	gitUploader := &SessionChunkUploader{
		diskStore: gitStore,
		logger:    testLogger().WithField("test", "git-drive"),
	}
	bazelUploader := &SessionChunkUploader{
		diskStore: bazelStore,
		logger:    testLogger().WithField("test", "bazel-drive"),
	}

	// --- Pause 1: 2 dirty on git, 1 dirty on bazel ---
	gitSnap1 := gitStore.totalStores()
	gitDirty1 := map[int][]byte{
		5: makeChunkData(chunkSize, "git-dirty-p1-5"),
		6: makeChunkData(chunkSize, "git-dirty-p1-6"),
	}
	gitBaseIndex.Region.LogicalSizeBytes = 7 * chunkSize
	gitIdx1, err := gitUploader.MergeAndUploadDisk(ctx, gitDirty1, gitBaseIndex)
	if err != nil {
		t.Fatalf("Pause 1 git MergeAndUploadDisk: %v", err)
	}
	if got := gitStore.storesSince(gitSnap1); got != 2 {
		t.Errorf("Pause 1 git: expected 2 new stores, got %d", got)
	}

	bazelSnap1 := bazelStore.totalStores()
	bazelDirty1 := map[int][]byte{
		3: makeChunkData(chunkSize, "bazel-dirty-p1-3"),
	}
	bazelBaseIndex.Region.LogicalSizeBytes = 4 * chunkSize
	bazelIdx1, err := bazelUploader.MergeAndUploadDisk(ctx, bazelDirty1, bazelBaseIndex)
	if err != nil {
		t.Fatalf("Pause 1 bazel MergeAndUploadDisk: %v", err)
	}
	if got := bazelStore.storesSince(bazelSnap1); got != 1 {
		t.Errorf("Pause 1 bazel: expected 1 new store, got %d", got)
	}

	// --- Pause 2: 1 new on git, 0 on bazel ---
	gitSnap2 := gitStore.totalStores()
	gitDirty2 := map[int][]byte{
		7: makeChunkData(chunkSize, "git-dirty-p2-7"),
	}
	gitIdx1.Region.LogicalSizeBytes = 8 * chunkSize
	_, err = gitUploader.MergeAndUploadDisk(ctx, gitDirty2, gitIdx1)
	if err != nil {
		t.Fatalf("Pause 2 git MergeAndUploadDisk: %v", err)
	}
	if got := gitStore.storesSince(gitSnap2); got != 1 {
		t.Errorf("Pause 2 git: expected 1 new store, got %d", got)
	}

	bazelSnap2 := bazelStore.totalStores()
	_, err = bazelUploader.MergeAndUploadDisk(ctx, map[int][]byte{}, bazelIdx1)
	if err != nil {
		t.Fatalf("Pause 2 bazel MergeAndUploadDisk: %v", err)
	}
	if got := bazelStore.storesSince(bazelSnap2); got != 0 {
		t.Errorf("Pause 2 bazel: expected 0 new stores, got %d", got)
	}
}

// --- Test 3: Derived metadata with extension drives ---

func TestE2E_DerivedMetadataWithExtensionDrives(t *testing.T) {
	chunkSize := int64(DefaultChunkSize)

	// Build a base with 5 mem chunks + 3 rootfs chunks.
	baseMemChunks := make([]ChunkRef, 5)
	for i := range baseMemChunks {
		baseMemChunks[i] = ChunkRef{
			Offset: int64(i) * chunkSize,
			Size:   chunkSize,
			Hash:   fmt.Sprintf("base-mem-%d", i),
		}
	}
	baseRootfsChunks := make([]ChunkRef, 3)
	for i := range baseRootfsChunks {
		baseRootfsChunks[i] = ChunkRef{
			Offset: int64(i) * chunkSize,
			Size:   chunkSize,
			Hash:   fmt.Sprintf("base-rootfs-%d", i),
		}
	}

	base := &ChunkedSnapshotMetadata{
		Version:      "1",
		WorkloadKey:  "basekey123",
		ChunkSize:    chunkSize,
		KernelHash:   "kernel-hash-abc",
		StateHash:    "state-hash-def",
		TotalMemSize: 5 * chunkSize,
		MemChunks:    baseMemChunks,
		RootfsChunks: baseRootfsChunks,
	}

	// Extension drives.
	extensions := map[string]ExtensionDrive{
		"git_drive": {
			ReadOnly:  false,
			SizeBytes: 10 * 1024 * 1024 * 1024,
			Chunks: []ChunkRef{
				{Offset: 0, Size: chunkSize, Hash: "git-chunk-0"},
				{Offset: chunkSize, Size: chunkSize, Hash: "git-chunk-1"},
			},
		},
		"bazel_cache": {
			ReadOnly:  true,
			SizeBytes: 20 * 1024 * 1024 * 1024,
			Chunks: []ChunkRef{
				{Offset: 0, Size: chunkSize, Hash: "bazel-chunk-0"},
			},
		},
	}

	driveSpecs := []DriveSpec{
		{DriveID: "git_drive", Label: "GIT", SizeGB: 10},
		{DriveID: "bazel_cache", Label: "BAZEL", SizeGB: 20},
	}

	derivedKey := ComputeDerivedWorkloadKey(base.WorkloadKey, driveSpecs)
	derived := BuildDerivedMetadata(base, derivedKey, base.MemChunks, base.RootfsChunks, extensions)

	// Verify kernel/state/chunkSize inherited.
	if derived.KernelHash != base.KernelHash {
		t.Errorf("KernelHash = %q, want %q", derived.KernelHash, base.KernelHash)
	}
	if derived.StateHash != base.StateHash {
		t.Errorf("StateHash = %q, want %q", derived.StateHash, base.StateHash)
	}
	if derived.ChunkSize != base.ChunkSize {
		t.Errorf("ChunkSize = %d, want %d", derived.ChunkSize, base.ChunkSize)
	}

	// Extensions added.
	if len(derived.ExtensionDrives) != 2 {
		t.Fatalf("ExtensionDrives count = %d, want 2", len(derived.ExtensionDrives))
	}
	gitDrive, ok := derived.ExtensionDrives["git_drive"]
	if !ok {
		t.Fatal("git_drive not found in derived ExtensionDrives")
	}
	if len(gitDrive.Chunks) != 2 {
		t.Errorf("git_drive.Chunks count = %d, want 2", len(gitDrive.Chunks))
	}
	bazelDrive, ok := derived.ExtensionDrives["bazel_cache"]
	if !ok {
		t.Fatal("bazel_cache not found in derived ExtensionDrives")
	}
	if !bazelDrive.ReadOnly {
		t.Error("bazel_cache.ReadOnly = false, want true")
	}

	// Derived key is deterministic and different from base.
	if derivedKey == base.WorkloadKey {
		t.Error("derived key should differ from base key")
	}
	if len(derivedKey) != 16 {
		t.Errorf("derived key length = %d, want 16", len(derivedKey))
	}

	// Same specs in different order → same key.
	driveSpecs2 := []DriveSpec{
		{DriveID: "bazel_cache", Label: "BAZEL", SizeGB: 20},
		{DriveID: "git_drive", Label: "GIT", SizeGB: 10},
	}
	derivedKey2 := ComputeDerivedWorkloadKey(base.WorkloadKey, driveSpecs2)
	if derivedKey != derivedKey2 {
		t.Errorf("derived keys differ with same specs in different order: %q vs %q", derivedKey, derivedKey2)
	}
}

// --- Test 4: Mem dedup with real sparse files (SEEK_DATA/SEEK_HOLE) ---

func TestE2E_SessionPauseChaining_MemDedup(t *testing.T) {
	ctx := context.Background()
	chunkSize := int64(DefaultChunkSize) // 4MB
	numBaseChunks := 10
	totalSize := int64(numBaseChunks) * chunkSize
	store := newMemoryChunkStore()

	// Pre-seed 10 base mem chunks.
	baseIDs := make([]string, numBaseChunks)
	for i := range baseIDs {
		baseIDs[i] = fmt.Sprintf("base-mem-%d", i)
	}
	baseExtents := seedStore(store, chunkSize, baseIDs)
	baseStores := store.totalStores()

	baseIndex := makeBaseChunkIndex(chunkSize, totalSize, baseExtents, "mem")

	uploader := &SessionChunkUploader{
		memStore: store,
		logger:   testLogger().WithField("test", "mem-dedup"),
	}

	// --- Pause 1: sparse file with 3 dirty chunk regions ---
	sparseFile1 := createSparseFileWithDirtyChunks(t, totalSize, chunkSize, map[int]string{
		2: "dirty-p1-2",
		5: "dirty-p1-5",
		9: "dirty-p1-9",
	})
	defer os.Remove(sparseFile1)

	idx1, err := uploader.MergeAndUploadMem(ctx, sparseFile1, baseIndex)
	if err != nil {
		t.Fatalf("Pause 1 MergeAndUploadMem: %v", err)
	}
	newStores1 := store.storesSince(baseStores)
	if newStores1 != 3 {
		t.Errorf("Pause 1: expected 3 new stores, got %d", newStores1)
	}
	// Should have all 10 extents (7 base pass-through + 3 dirty).
	if len(idx1.Region.Extents) != numBaseChunks {
		t.Errorf("Pause 1: expected %d extents, got %d", numBaseChunks, len(idx1.Region.Extents))
	}

	// --- Pause 2: 1 dirty chunk ---
	snapshot2 := store.totalStores()
	sparseFile2 := createSparseFileWithDirtyChunks(t, totalSize, chunkSize, map[int]string{
		7: "dirty-p2-7",
	})
	defer os.Remove(sparseFile2)

	idx2, err := uploader.MergeAndUploadMem(ctx, sparseFile2, idx1)
	if err != nil {
		t.Fatalf("Pause 2 MergeAndUploadMem: %v", err)
	}
	newStores2 := store.storesSince(snapshot2)
	if newStores2 != 1 {
		t.Errorf("Pause 2: expected 1 new store, got %d", newStores2)
	}

	// --- Pause 3: empty sparse file (all holes, 0 dirty) ---
	snapshot3 := store.totalStores()
	sparseFile3 := createSparseFileWithDirtyChunks(t, totalSize, chunkSize, map[int]string{})
	defer os.Remove(sparseFile3)

	idx3, err := uploader.MergeAndUploadMem(ctx, sparseFile3, idx2)
	if err != nil {
		t.Fatalf("Pause 3 MergeAndUploadMem: %v", err)
	}
	newStores3 := store.storesSince(snapshot3)
	if newStores3 != 0 {
		t.Errorf("Pause 3: expected 0 new stores, got %d", newStores3)
	}
	if len(idx3.Region.Extents) != numBaseChunks {
		t.Errorf("Pause 3: expected %d extents, got %d", numBaseChunks, len(idx3.Region.Extents))
	}
}

// createSparseFileWithDirtyChunks creates a sparse file of the given totalSize,
// writing non-zero data at the specified chunk offsets. The file is sparse: only
// the dirty chunk regions contain data; the rest are holes.
// Returns the path to the temporary file.
func createSparseFileWithDirtyChunks(t *testing.T, totalSize, chunkSize int64, dirtyChunks map[int]string) string {
	t.Helper()
	f, err := os.CreateTemp("", "sparse-mem-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	// Truncate to set logical size (creates a sparse file of all holes).
	if err := f.Truncate(totalSize); err != nil {
		os.Remove(f.Name())
		t.Fatalf("Truncate: %v", err)
	}

	// Write non-zero data at each dirty chunk offset.
	// We write full chunks of non-zero data to ensure the entire chunk is "dirty".
	for chunkIdx, id := range dirtyChunks {
		offset := int64(chunkIdx) * chunkSize
		data := makeChunkData(chunkSize, id)
		if _, err := f.WriteAt(data, offset); err != nil {
			os.Remove(f.Name())
			t.Fatalf("WriteAt chunk %d: %v", chunkIdx, err)
		}
	}

	if err := f.Sync(); err != nil {
		os.Remove(f.Name())
		t.Fatalf("Sync: %v", err)
	}

	return f.Name()
}
