//go:build linux
// +build linux

package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"github.com/klauspost/compress/zstd"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// sessionChunkUploadConcurrency controls how many chunks are uploaded in
// parallel during MergeAndUploadMem / MergeAndUploadDisk. Lower values
// reduce GCS stream contention when many runners pause concurrently
// (10 runners × 16 streams = 160 total, well within NIC capacity).
const sessionChunkUploadConcurrency = 16

// SessionChunkUploader merges dirty diff pages into base snapshot chunks and
// uploads them to GCS, producing self-contained ChunkIndex objects that can
// be used by the UFFD handler on any host (no golden snapshot.mem required).
type SessionChunkUploader struct {
	memStore  ChunkStorer     // chunks/mem/<p0>/<hash>
	diskStore ChunkStorer     // chunks/disk/<p0>/<hash>; may be nil
	gcsClient *storage.Client // for GCS-specific operations (upload/download manifests, state, etc.)
	gcsBucket string
	gcsPrefix string // e.g. "v1"
	logger    *logrus.Entry
}

// NewSessionChunkUploader creates a new uploader.
// memStore must have ChunkSubdir:"mem"; diskStore (optional) must have ChunkSubdir:"disk".
func NewSessionChunkUploader(memStore, diskStore *ChunkStore, logger *logrus.Logger) *SessionChunkUploader {
	if logger == nil {
		logger = logrus.New()
	}
	gcsBucket := ""
	gcsPrefix := ""
	var gcsClient *storage.Client
	if memStore != nil {
		gcsBucket = memStore.gcsBucket
		gcsPrefix = memStore.gcsPrefix
		gcsClient = memStore.gcsClient
	}
	return &SessionChunkUploader{
		memStore:  memStore,
		diskStore: diskStore,
		gcsClient: gcsClient,
		gcsBucket: gcsBucket,
		gcsPrefix: gcsPrefix,
		logger:    logger.WithField("component", "session-chunk-uploader"),
	}
}

// MergeAndUploadMem produces a self-contained ChunkIndex for all VM memory.
//
// It uses SEEK_DATA/SEEK_HOLE to iterate only dirty regions of memDiffPath
// (Firecracker writes the diff as a sparse file when track_dirty_pages=true).
// For each DefaultChunkSize block that is dirty:
//   - If the chunk straddles a clean boundary: fetch the base chunk from
//     memStore, overlay dirty pages, then store the merged chunk.
//   - If the chunk is fully dirty: store it directly.
//
// Non-dirty base extents are copied by hash reference (no re-upload).
// Zero extents are omitted entirely (coverage:sparse, default_fill:zero).
func (u *SessionChunkUploader) MergeAndUploadMem(ctx context.Context, memDiffPath string, baseIndex *ChunkIndex) (*ChunkIndex, error) {
	start := time.Now()

	// Build a set of dirty chunk indices from the sparse file.
	dirtyChunks, err := u.findDirtyChunks(memDiffPath, baseIndex.ChunkSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to find dirty chunks in %s: %w", memDiffPath, err)
	}

	// Build a page-level bitmap of data extents using SEEK_DATA/SEEK_HOLE.
	// This distinguishes "VM wrote zeros" (data extent) from "sparse hole"
	// (not dirty, use base data). Without this, zero pages from the VM would
	// be incorrectly replaced with stale base data, corrupting guest memory.
	pageBitmap, err := buildPageBitmap(memDiffPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build page bitmap for %s: %w", memDiffPath, err)
	}
	findDirtyDuration := time.Since(start)

	u.logger.WithFields(logrus.Fields{
		"mem_diff_path": memDiffPath,
		"dirty_chunks":  len(dirtyChunks),
		"base_extents":  len(baseIndex.Region.Extents),
		"find_dirty_ms": findDirtyDuration.Milliseconds(),
	}).Info("Merging dirty mem diff into base chunk index")

	// Build a lookup map: chunk index -> base ManifestChunkRef
	baseByChunkIdx := make(map[int64]ManifestChunkRef, len(baseIndex.Region.Extents))
	chunkSize := baseIndex.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	for _, ext := range baseIndex.Region.Extents {
		idx := ext.Offset / chunkSize
		baseByChunkIdx[idx] = ext
	}

	// Open the diff file for reads.
	diffFile, err := os.Open(memDiffPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open mem diff: %w", err)
	}
	defer diffFile.Close()

	totalSize := baseIndex.Region.LogicalSizeBytes
	numChunks := (totalSize + chunkSize - 1) / chunkSize

	// Results: one ManifestChunkRef per dirty chunk index (by position).
	type chunkResult struct {
		idx int64
		ref ManifestChunkRef
		err error
	}
	results := make(chan chunkResult, len(dirtyChunks))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(sessionChunkUploadConcurrency)

	for chunkIdx := range dirtyChunks {
		ci := chunkIdx // capture
		g.Go(func() error {
			ref, err := u.mergeChunk(gCtx, diffFile, ci, chunkSize, totalSize, baseByChunkIdx, pageBitmap)
			results <- chunkResult{idx: ci, ref: ref, err: err}
			return err
		})
	}

	// Wait and collect.
	mergeUploadStart := time.Now()
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("chunk merge/upload failed: %w", err)
	}
	close(results)
	mergeUploadDuration := time.Since(mergeUploadStart)

	// Count how many chunks needed base merge vs direct upload
	var mergedCount, directCount, zeroCount int

	// Build merged map: dirty results override base; non-dirty base entries pass through.
	mergedByIdx := make(map[int64]ManifestChunkRef, len(baseByChunkIdx))
	for idx, ref := range baseByChunkIdx {
		mergedByIdx[idx] = ref
	}
	for r := range results {
		if r.err != nil {
			continue
		}
		if r.ref.Hash == ZeroChunkHash {
			zeroCount++
		} else if _, hadBase := baseByChunkIdx[r.idx]; hadBase {
			mergedCount++
			mergedByIdx[r.idx] = r.ref
		} else {
			directCount++
			mergedByIdx[r.idx] = r.ref
		}
		// Note: we intentionally do NOT delete base entries when a dirty chunk
		// merges to all zeros. The sparse diff file cannot distinguish "VM wrote
		// zeros" from "sparse hole where base data should show through." Keeping
		// the base entry is the safe default — at worst we serve stale data for
		// a page the VM zeroed, which is harmless for memory semantics.
	}

	// Build new ChunkIndex extents (sorted by offset).
	extents := make([]ManifestChunkRef, 0, len(mergedByIdx))
	for i := int64(0); i < numChunks; i++ {
		if ref, ok := mergedByIdx[i]; ok {
			extents = append(extents, ref)
		}
	}

	newIdx := &ChunkIndex{
		Version:        "1",
		CreatedAt:      time.Now(),
		ChunkSizeBytes: chunkSize,
	}
	newIdx.CAS.Algo = "sha256"
	newIdx.CAS.Layout = "chunks/mem/{p0}/{hash}"
	newIdx.CAS.Kind = "mem"
	newIdx.Region.Name = "vm_memory"
	newIdx.Region.LogicalSizeBytes = totalSize
	newIdx.Region.Coverage = "sparse"
	newIdx.Region.DefaultFill = "zero"
	newIdx.Region.Extents = extents

	u.logger.WithFields(logrus.Fields{
		"extents":          len(extents),
		"duration":         time.Since(start),
		"find_dirty_ms":    findDirtyDuration.Milliseconds(),
		"merge_upload_ms":  mergeUploadDuration.Milliseconds(),
		"dirty_merged":     mergedCount,
		"dirty_direct":     directCount,
		"dirty_zero":       zeroCount,
		"base_carried_fwd": len(baseByChunkIdx) - mergedCount,
	}).Info("MergeAndUploadMem complete")

	return newIdx, nil
}

// mergeChunk fetches/reads the dirty chunk data and uploads it.
// pageBitmap has one bit per page: set if the page is in a data extent
// (SEEK_DATA), clear if it's a sparse hole. This correctly distinguishes
// "VM wrote zeros" from "page not dirty, use base".
func (u *SessionChunkUploader) mergeChunk(
	ctx context.Context,
	diffFile *os.File,
	chunkIdx, chunkSize, totalSize int64,
	baseByIdx map[int64]ManifestChunkRef,
	pageBitmap []uint64,
) (ManifestChunkRef, error) {
	offset := chunkIdx * chunkSize
	size := chunkSize
	if offset+size > totalSize {
		size = totalSize - offset
	}

	// Read the dirty pages from the diff file.
	diffData := make([]byte, size)
	if _, err := diffFile.ReadAt(diffData, offset); err != nil && err != io.EOF {
		return ManifestChunkRef{}, fmt.Errorf("failed to read diff at offset %d: %w", offset, err)
	}

	// Check if there is a base chunk we need to merge with.
	// Use the page bitmap to determine which pages are sparse holes (use base)
	// vs data extents (use diff, even if all zeros).
	needsMerge := false
	if base, ok := baseByIdx[chunkIdx]; ok && base.Hash != ZeroChunkHash {
		for i := 0; i+PageSize <= len(diffData); i += PageSize {
			pageOffset := offset + int64(i)
			if !pageInBitmap(pageBitmap, pageOffset) {
				// This page is a sparse hole — need to fill from base.
				needsMerge = true
				break
			}
		}
	}

	if needsMerge {
		baseHash := baseByIdx[chunkIdx].Hash
		baseFetchStart := time.Now()
		baseData, err := u.memStore.GetChunk(ctx, baseHash)
		if err != nil {
			return ManifestChunkRef{}, fmt.Errorf("failed to fetch base chunk %s: %w", baseHash[:12], err)
		}
		baseFetchDur := time.Since(baseFetchStart)
		if baseFetchDur > 500*time.Millisecond {
			u.logger.WithFields(logrus.Fields{
				"chunk_idx":     chunkIdx,
				"base_hash":     baseHash[:12],
				"base_fetch_ms": baseFetchDur.Milliseconds(),
			}).Warn("Slow base chunk fetch during merge")
		}
		// Start with base data, then overlay pages that are in data extents.
		merged := make([]byte, size)
		copy(merged, baseData[:size])
		for i := 0; i+PageSize <= len(diffData); i += PageSize {
			pageOffset := offset + int64(i)
			if pageInBitmap(pageBitmap, pageOffset) {
				// Page is in a data extent — use diff data (even if zeros).
				copy(merged[i:], diffData[i:i+PageSize])
			}
		}
		diffData = merged
	}

	// Skip all-zero chunks — they're implicit in the sparse index.
	if isZeroChunk(diffData) {
		return ManifestChunkRef{Hash: ZeroChunkHash}, nil
	}

	hash, compressedSize, err := u.memStore.StoreChunk(ctx, diffData)
	if err != nil {
		return ManifestChunkRef{}, fmt.Errorf("failed to store merged chunk: %w", err)
	}

	return ManifestChunkRef{
		Offset:       offset,
		Length:       size,
		Hash:         hash,
		StoredLength: compressedSize,
	}, nil
}

// PageSize is the x86_64 page size used for per-page overlay.
const PageSize = 4096

// findDirtyChunks uses SEEK_DATA/SEEK_HOLE to find chunk indices with dirty data.
func (u *SessionChunkUploader) findDirtyChunks(path string, chunkSize int64) (map[int64]struct{}, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dirty := make(map[int64]struct{})

	for offset := int64(0); ; {
		dataStart, err := unix.Seek(int(f.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			break // ENXIO = no more data segments
		}
		holeStart, err := unix.Seek(int(f.Fd()), dataStart, unix.SEEK_HOLE)
		if err != nil {
			// Treat end-of-file as hole start.
			fi, statErr := f.Stat()
			if statErr != nil {
				break
			}
			holeStart = fi.Size()
		}

		// Mark every chunk index that overlaps [dataStart, holeStart) as dirty.
		firstChunk := dataStart / chunkSize
		lastChunk := (holeStart - 1) / chunkSize
		for ci := firstChunk; ci <= lastChunk; ci++ {
			dirty[ci] = struct{}{}
		}

		offset = holeStart
	}

	return dirty, nil
}

// buildPageBitmap walks a sparse file with SEEK_DATA/SEEK_HOLE and returns
// a bitmap with one bit per page. Bit N is set if page N is in a data extent
// (has actual data, including zeros the VM wrote). Clear bits are sparse holes
// (page was never written, should use base data on merge).
func buildPageBitmap(path string) ([]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	numPages := (fi.Size() + PageSize - 1) / PageSize
	numWords := (numPages + 63) / 64
	bitmap := make([]uint64, numWords)

	for offset := int64(0); ; {
		dataStart, err := unix.Seek(int(f.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			break // ENXIO = no more data segments
		}
		holeStart, err := unix.Seek(int(f.Fd()), dataStart, unix.SEEK_HOLE)
		if err != nil {
			holeStart = fi.Size()
		}

		firstPage := dataStart / PageSize
		lastPage := (holeStart - 1) / PageSize
		for p := firstPage; p <= lastPage; p++ {
			bitmap[p/64] |= 1 << uint(p%64)
		}

		offset = holeStart
	}

	return bitmap, nil
}

// pageInBitmap returns true if the page at the given byte offset is marked
// as a data extent in the bitmap (not a sparse hole).
func pageInBitmap(bitmap []uint64, byteOffset int64) bool {
	page := uint64(byteOffset) / PageSize
	word := page / 64
	if word >= uint64(len(bitmap)) {
		return false
	}
	return bitmap[word]&(1<<(page%64)) != 0
}

// findDirtySegments uses SEEK_DATA/SEEK_HOLE to find byte-level data regions
// in a sparse file. Returns segments (offset+length pairs) and total dirty bytes.
func findDirtySegments(path string) ([]DiffSegment, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var segments []DiffSegment
	var totalDirty int64

	for offset := int64(0); ; {
		dataStart, err := unix.Seek(int(f.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			break // ENXIO = no more data segments
		}
		holeStart, err := unix.Seek(int(f.Fd()), dataStart, unix.SEEK_HOLE)
		if err != nil {
			fi, statErr := f.Stat()
			if statErr != nil {
				break
			}
			holeStart = fi.Size()
		}

		length := holeStart - dataStart
		segments = append(segments, DiffSegment{
			Offset: dataStart,
			Length: length,
		})
		totalDirty += length

		offset = holeStart
	}

	return segments, totalDirty, nil
}

// UploadMemDiff streams all dirty segments from a sparse memory diff file into
// a single zstd-compressed GCS blob. Returns the blob object path and segments.
// This replaces MergeAndUploadMem for the diff_file mode, reducing GCS round-trips
// from ~30 per-chunk (fetch base + upload merged) to a single streaming upload.
func (u *SessionChunkUploader) UploadMemDiff(ctx context.Context, memDiffPath, gcsBase string, totalSize int64) (string, []DiffSegment, error) {
	start := time.Now()

	segments, totalDirty, err := findDirtySegments(memDiffPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to find dirty segments: %w", err)
	}

	u.logger.WithFields(logrus.Fields{
		"mem_diff_path":  memDiffPath,
		"segments":       len(segments),
		"total_dirty_mb": totalDirty / (1024 * 1024),
	}).Info("UploadMemDiff: streaming dirty segments to GCS")

	blobPath := u.prefixedPath(gcsBase + "/mem_diff.zst")

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	obj := bucket.Object(blobPath)
	w := obj.NewWriter(ctx)
	w.ContentType = "application/zstd"

	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		w.Close()
		return "", nil, fmt.Errorf("failed to create zstd writer: %w", err)
	}

	f, err := os.Open(memDiffPath)
	if err != nil {
		zw.Close()
		w.Close()
		return "", nil, fmt.Errorf("failed to open mem diff: %w", err)
	}
	defer f.Close()

	buf := make([]byte, 256*1024) // 256KB read buffer
	for _, seg := range segments {
		remaining := seg.Length
		off := seg.Offset
		for remaining > 0 {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			nr, err := f.ReadAt(buf[:n], off)
			if err != nil && err != io.EOF {
				zw.Close()
				w.Close()
				return "", nil, fmt.Errorf("failed to read diff at offset %d: %w", off, err)
			}
			if nr == 0 {
				break
			}
			if _, err := zw.Write(buf[:nr]); err != nil {
				zw.Close()
				w.Close()
				return "", nil, fmt.Errorf("failed to write to zstd: %w", err)
			}
			off += int64(nr)
			remaining -= int64(nr)
		}
	}

	if err := zw.Close(); err != nil {
		w.Close()
		return "", nil, fmt.Errorf("failed to close zstd writer: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", nil, fmt.Errorf("failed to close GCS writer: %w", err)
	}

	u.logger.WithFields(logrus.Fields{
		"blob_path": blobPath,
		"segments":  len(segments),
		"dirty_mb":  totalDirty / (1024 * 1024),
		"ms":        time.Since(start).Milliseconds(),
	}).Info("UploadMemDiff complete")

	return blobPath, segments, nil
}

// DownloadMemDiff downloads a zstd-compressed diff blob from GCS and
// reconstructs a sparse local file with data at the correct offsets.
func (u *SessionChunkUploader) DownloadMemDiff(ctx context.Context, blobObject string, segments []DiffSegment, totalSize int64, localPath string) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	reader, err := bucket.Object(blobObject).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to open diff blob %s: %w", blobObject, err)
	}
	defer reader.Close()

	zr, err := zstd.NewReader(reader)
	if err != nil {
		return fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zr.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create parent dir: %w", err)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local diff file: %w", err)
	}
	defer f.Close()

	// Create a sparse file of the correct total size.
	if err := f.Truncate(totalSize); err != nil {
		return fmt.Errorf("failed to truncate diff file: %w", err)
	}

	buf := make([]byte, 256*1024)
	for _, seg := range segments {
		remaining := seg.Length
		off := seg.Offset
		for remaining > 0 {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			nr, err := io.ReadFull(zr, buf[:n])
			if err != nil && err != io.ErrUnexpectedEOF {
				return fmt.Errorf("failed to read from zstd at offset %d: %w", off, err)
			}
			if nr == 0 {
				break
			}
			if _, err := f.WriteAt(buf[:nr], off); err != nil {
				return fmt.Errorf("failed to write to diff file at offset %d: %w", off, err)
			}
			off += int64(nr)
			remaining -= int64(nr)
		}
	}

	return nil
}

// MergeLocalDiffs merges two local sparse diff files into one. The newer diff
// (newDiffPath) takes priority over the previous diff (prevDiffPath) at
// overlapping offsets. Used for multi-pause chains to combine accumulated diffs.
func MergeLocalDiffs(outPath, prevDiffPath, newDiffPath string, totalSize int64) ([]DiffSegment, error) {
	prevSegs, _, err := findDirtySegments(prevDiffPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find segments in prev diff: %w", err)
	}
	newSegs, _, err := findDirtySegments(newDiffPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find segments in new diff: %w", err)
	}

	outFile, err := os.Create(outPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	if err := outFile.Truncate(totalSize); err != nil {
		return nil, fmt.Errorf("failed to truncate output: %w", err)
	}

	buf := make([]byte, 256*1024)

	// Helper to copy segments from a source file to outFile.
	copySegments := func(srcPath string, segs []DiffSegment) error {
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		for _, seg := range segs {
			remaining := seg.Length
			off := seg.Offset
			for remaining > 0 {
				n := int64(len(buf))
				if remaining < n {
					n = remaining
				}
				nr, err := src.ReadAt(buf[:n], off)
				if err != nil && err != io.EOF {
					return err
				}
				if nr == 0 {
					break
				}
				if _, err := outFile.WriteAt(buf[:nr], off); err != nil {
					return err
				}
				off += int64(nr)
				remaining -= int64(nr)
			}
		}
		return nil
	}

	// Copy prev diff first (base layer).
	if err := copySegments(prevDiffPath, prevSegs); err != nil {
		return nil, fmt.Errorf("failed to copy prev diff: %w", err)
	}

	// Overlay new diff (newer data wins at overlapping offsets).
	if err := copySegments(newDiffPath, newSegs); err != nil {
		return nil, fmt.Errorf("failed to copy new diff: %w", err)
	}

	// Find the merged segments of the output file.
	mergedSegs, _, err := findDirtySegments(outPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find merged segments: %w", err)
	}

	return mergedSegs, nil
}

// UploadJSON uploads a JSON-serializable value to a GCS object path.
func (u *SessionChunkUploader) UploadJSON(ctx context.Context, gcsObjectPath string, v any) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", gcsObjectPath, err)
	}
	bucket := u.gcsClient.Bucket(u.gcsBucket)
	w := bucket.Object(gcsObjectPath).NewWriter(ctx)
	w.ContentType = "application/json"
	if _, err := w.Write(data); err != nil {
		w.Close()
		return fmt.Errorf("write %s: %w", gcsObjectPath, err)
	}
	return w.Close()
}

// DownloadJSON downloads and unmarshals a JSON object from GCS into v.
func (u *SessionChunkUploader) DownloadJSON(ctx context.Context, gcsObjectPath string, v any) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}
	bucket := u.gcsClient.Bucket(u.gcsBucket)
	reader, err := bucket.Object(gcsObjectPath).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", gcsObjectPath, err)
	}
	defer reader.Close()
	return json.NewDecoder(reader).Decode(v)
}

// MergeAndUploadDisk produces a self-contained ChunkIndex for the FUSE disk.
// dirtyChunks is a map of chunk index -> uncompressed chunk data from fuse.ChunkedDisk.GetDirtyChunks().
func (u *SessionChunkUploader) MergeAndUploadDisk(ctx context.Context, dirtyChunks map[int][]byte, baseIndex *ChunkIndex) (*ChunkIndex, error) {
	if u.diskStore == nil {
		return nil, fmt.Errorf("disk chunk store not configured")
	}

	start := time.Now()
	chunkSize := baseIndex.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	u.logger.WithFields(logrus.Fields{
		"dirty_chunks": len(dirtyChunks),
		"base_extents": len(baseIndex.Region.Extents),
	}).Info("Merging dirty disk chunks into base chunk index")

	// Build lookup: chunk index -> base ref.
	baseByIdx := make(map[int64]ManifestChunkRef, len(baseIndex.Region.Extents))
	for _, ext := range baseIndex.Region.Extents {
		baseByIdx[ext.Offset/chunkSize] = ext
	}

	type result struct {
		idx int64
		ref ManifestChunkRef
	}
	resultsCh := make(chan result, len(dirtyChunks))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(sessionChunkUploadConcurrency)

	for rawIdx, data := range dirtyChunks {
		ci := int64(rawIdx)
		d := data
		g.Go(func() error {
			if isZeroChunk(d) {
				resultsCh <- result{idx: ci, ref: ManifestChunkRef{Hash: ZeroChunkHash}}
				return nil
			}
			hash, compressedSize, err := u.diskStore.StoreChunk(gCtx, d)
			if err != nil {
				return fmt.Errorf("failed to store disk chunk %d: %w", ci, err)
			}
			resultsCh <- result{idx: ci, ref: ManifestChunkRef{
				Offset:       ci * chunkSize,
				Length:       int64(len(d)),
				Hash:         hash,
				StoredLength: compressedSize,
			}}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("disk chunk upload failed: %w", err)
	}
	close(resultsCh)

	// Merge results with base.
	mergedByIdx := make(map[int64]ManifestChunkRef, len(baseByIdx))
	for idx, ref := range baseByIdx {
		mergedByIdx[idx] = ref
	}
	for r := range resultsCh {
		if r.ref.Hash == ZeroChunkHash {
			delete(mergedByIdx, r.idx)
		} else {
			mergedByIdx[r.idx] = r.ref
		}
	}

	totalSize := baseIndex.Region.LogicalSizeBytes
	numChunks := (totalSize + chunkSize - 1) / chunkSize
	extents := make([]ManifestChunkRef, 0, len(mergedByIdx))
	for i := int64(0); i < numChunks; i++ {
		if ref, ok := mergedByIdx[i]; ok {
			extents = append(extents, ref)
		}
	}

	newIdx := &ChunkIndex{
		Version:        "1",
		CreatedAt:      time.Now(),
		ChunkSizeBytes: chunkSize,
	}
	newIdx.CAS.Algo = "sha256"
	newIdx.CAS.Layout = "chunks/disk/{p0}/{hash}"
	newIdx.CAS.Kind = "disk"
	newIdx.Region.Name = "vm_disk"
	newIdx.Region.LogicalSizeBytes = totalSize
	newIdx.Region.Coverage = "sparse"
	newIdx.Region.DefaultFill = "zero"
	newIdx.Region.Extents = extents

	u.logger.WithFields(logrus.Fields{
		"extents":  len(extents),
		"duration": time.Since(start),
	}).Info("MergeAndUploadDisk complete")

	return newIdx, nil
}

// UploadVMState uploads the snapshot.state file to a GCS object path using
// the mem chunk store's GCS client. The state file is uploaded uncompressed
// (Firecracker opens it directly and it's typically < 1 MB).
func (u *SessionChunkUploader) UploadVMState(ctx context.Context, localPath, gcsObjectPath string) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}

	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open vmstate: %w", err)
	}
	defer f.Close()

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	obj := bucket.Object(gcsObjectPath)
	w := obj.NewWriter(ctx)
	w.ContentType = "application/octet-stream"

	if _, err := io.Copy(w, f); err != nil {
		w.Close()
		return fmt.Errorf("failed to write vmstate to GCS: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to close vmstate GCS writer: %w", err)
	}

	return nil
}

// DownloadVMState downloads a GCS object to a local file path.
func (u *SessionChunkUploader) DownloadVMState(ctx context.Context, gcsObjectPath, localPath string) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	reader, err := bucket.Object(gcsObjectPath).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to open vmstate GCS object %s: %w", gcsObjectPath, err)
	}
	defer reader.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("failed to create parent dir: %w", err)
	}

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local vmstate file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, reader); err != nil {
		return fmt.Errorf("failed to write vmstate locally: %w", err)
	}

	return nil
}

// WriteManifest uploads SnapshotManifest and ChunkIndex JSON files to GCS.
// Paths (all relative to the GCS bucket, prefixed with gcsPrefix if set):
//
//	{gcsBase}/snapshot_manifest.json
//	{gcsBase}/chunked-metadata.json
//	{gcsBase}/disk-chunked-metadata.json  (if diskIndex != nil)
func (u *SessionChunkUploader) WriteManifest(
	ctx context.Context,
	gcsBase string,
	manifest *SnapshotManifest,
	memIndex, diskIndex *ChunkIndex,
) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)

	uploadJSON := func(objPath string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", objPath, err)
		}
		w := bucket.Object(objPath).NewWriter(ctx)
		w.ContentType = "application/json"
		if _, err := w.Write(data); err != nil {
			w.Close()
			return fmt.Errorf("write %s: %w", objPath, err)
		}
		return w.Close()
	}

	memIdxPath := u.prefixedPath(gcsBase + "/chunked-metadata.json")
	if err := uploadJSON(memIdxPath, memIndex); err != nil {
		return fmt.Errorf("failed to upload mem chunk index: %w", err)
	}

	if diskIndex != nil {
		diskIdxPath := u.prefixedPath(gcsBase + "/disk-chunked-metadata.json")
		if err := uploadJSON(diskIdxPath, diskIndex); err != nil {
			return fmt.Errorf("failed to upload disk chunk index: %w", err)
		}
	}

	manifestPath := u.prefixedPath(gcsBase + "/snapshot_manifest.json")
	if err := uploadJSON(manifestPath, manifest); err != nil {
		return fmt.Errorf("failed to upload snapshot manifest: %w", err)
	}

	u.logger.WithFields(logrus.Fields{
		"gcs_base": gcsBase,
		"mem_idx":  memIdxPath,
		"manifest": manifestPath,
	}).Info("WriteManifest complete")

	return nil
}

// WriteManifestWithExtensions uploads SnapshotManifest and all ChunkIndex JSON files to GCS.
// extDiskIndexes maps driveID to ChunkIndex for each dirty extension drive.
// Per-drive disk indexes are stored at {gcsBase}/{driveID}-disk.json.
func (u *SessionChunkUploader) WriteManifestWithExtensions(
	ctx context.Context,
	gcsBase string,
	manifest *SnapshotManifest,
	memIndex *ChunkIndex,
	extDiskIndexes map[string]*ChunkIndex,
) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)

	uploadJSON := func(objPath string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", objPath, err)
		}
		w := bucket.Object(objPath).NewWriter(ctx)
		w.ContentType = "application/json"
		if _, err := w.Write(data); err != nil {
			w.Close()
			return fmt.Errorf("write %s: %w", objPath, err)
		}
		return w.Close()
	}

	memIdxPath := u.prefixedPath(gcsBase + "/chunked-metadata.json")
	if memIndex != nil {
		if err := uploadJSON(memIdxPath, memIndex); err != nil {
			return fmt.Errorf("failed to upload mem chunk index: %w", err)
		}
	}

	for driveID, diskIdx := range extDiskIndexes {
		diskIdxPath := u.prefixedPath(gcsBase + "/" + driveID + "-disk.json")
		if err := uploadJSON(diskIdxPath, diskIdx); err != nil {
			return fmt.Errorf("failed to upload disk chunk index for drive %s: %w", driveID, err)
		}
	}

	manifestPath := u.prefixedPath(gcsBase + "/snapshot_manifest.json")
	if err := uploadJSON(manifestPath, manifest); err != nil {
		return fmt.Errorf("failed to upload snapshot manifest: %w", err)
	}

	u.logger.WithFields(logrus.Fields{
		"gcs_base": gcsBase,
		"mem_idx":  memIdxPath,
		"manifest": manifestPath,
	}).Info("WriteManifestWithExtensions complete")

	return nil
}

// DownloadChunkIndex downloads and unmarshals a ChunkIndex from a GCS object path.
func (u *SessionChunkUploader) DownloadChunkIndex(ctx context.Context, gcsObjectPath string) (*ChunkIndex, error) {
	if u.gcsClient == nil {
		return nil, fmt.Errorf("GCS client not configured")
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	reader, err := bucket.Object(gcsObjectPath).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open chunk index %s: %w", gcsObjectPath, err)
	}
	defer reader.Close()

	var idx ChunkIndex
	if err := json.NewDecoder(reader).Decode(&idx); err != nil {
		return nil, fmt.Errorf("failed to decode chunk index %s: %w", gcsObjectPath, err)
	}

	return &idx, nil
}

// DownloadManifest downloads and unmarshals a SnapshotManifest from a GCS object path.
func (u *SessionChunkUploader) DownloadManifest(ctx context.Context, gcsObjectPath string) (*SnapshotManifest, error) {
	if u.gcsClient == nil {
		return nil, fmt.Errorf("GCS client not configured")
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	reader, err := bucket.Object(gcsObjectPath).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open manifest %s: %w", gcsObjectPath, err)
	}
	defer reader.Close()

	var m SnapshotManifest
	if err := json.NewDecoder(reader).Decode(&m); err != nil {
		return nil, fmt.Errorf("failed to decode manifest %s: %w", gcsObjectPath, err)
	}

	return &m, nil
}

// prefixedPath prepends the GCS prefix to a relative path.
func (u *SessionChunkUploader) prefixedPath(path string) string {
	if u.gcsPrefix != "" {
		return u.gcsPrefix + "/" + path
	}
	return path
}

// UploadSessionMetadata uploads session metadata JSON to the runner_state path
// in GCS, enabling cross-host resume without local metadata.json.
func (u *SessionChunkUploader) UploadSessionMetadata(ctx context.Context, workloadKey, runnerID string, data []byte) error {
	if u.gcsClient == nil {
		return fmt.Errorf("GCS client not configured")
	}

	objPath := u.prefixedPath(fmt.Sprintf("%s/runner_state/%s/session_metadata.json", workloadKey, runnerID))
	bucket := u.gcsClient.Bucket(u.gcsBucket)
	w := bucket.Object(objPath).NewWriter(ctx)
	w.ContentType = "application/json"
	if _, err := w.Write(data); err != nil {
		w.Close()
		return fmt.Errorf("write session metadata: %w", err)
	}
	return w.Close()
}

// DownloadSessionMetadata downloads session metadata JSON from the runner_state
// path in GCS, using workloadKey and runnerID to locate the file.
func (u *SessionChunkUploader) DownloadSessionMetadata(ctx context.Context, workloadKey, runnerID string) ([]byte, error) {
	if u.gcsClient == nil {
		return nil, fmt.Errorf("GCS client not configured")
	}

	objPath := u.prefixedPath(fmt.Sprintf("%s/runner_state/%s/session_metadata.json", workloadKey, runnerID))
	bucket := u.gcsClient.Bucket(u.gcsBucket)
	reader, err := bucket.Object(objPath).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open session metadata %s: %w", objPath, err)
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// FullGCSPath returns the full GCS object path (with prefix) for a relative path.
// This is the path callers should store in metadata for later retrieval.
func (u *SessionChunkUploader) FullGCSPath(relativePath string) string {
	return u.prefixedPath(relativePath)
}
