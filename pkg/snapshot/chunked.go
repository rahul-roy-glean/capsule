package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/klauspost/compress/zstd"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/util/boundedstack"
)

const (
	// DefaultChunkSize is 4MB - balances between granularity and overhead
	DefaultChunkSize = 4 * 1024 * 1024

	// ChunksPrefix is the GCS prefix for chunk storage
	ChunksPrefix = "chunks"

	// ZeroChunkHash is the sentinel value for chunks that are entirely zero.
	// These chunks are never uploaded or fetched — readers return zero-filled
	// buffers when they encounter this hash.
	ZeroChunkHash = ""

	// Eager prefetching constants
	eagerFetchBufferCapacity = 1000 // Max chunks queued for prefetch
	numChunksToEagerFetch    = 64   // Chunks to queue on each access
	maxEagerFetchesPerSec    = 5000 // Rate limit for prefetch operations
	eagerFetchConcurrency    = 64   // Parallel prefetch workers

	// chunkFileUploadConcurrency controls how many chunks are uploaded in
	// parallel during ChunkFile.
	chunkFileUploadConcurrency = 16
)

// ChunkedSnapshotMetadata holds metadata for a chunked snapshot.
// Instead of storing full files, we store references to content-addressed chunks.
type ChunkedSnapshotMetadata struct {
	Version       string     `json:"version"`
	BazelVersion  string     `json:"bazel_version,omitempty"`
	RepoCommit    string     `json:"repo_commit,omitempty"`
	Repo          string     `json:"repo,omitempty"`
	RepoSlug      string     `json:"repo_slug,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ChunkSize     int64      `json:"chunk_size"`
	KernelHash    string     `json:"kernel_hash"`
	StateHash     string     `json:"state_hash"`
	MemChunks     []ChunkRef `json:"mem_chunks"`
	RootfsChunks  []ChunkRef `json:"rootfs_chunks"`
	TotalMemSize  int64      `json:"total_mem_size"`
	TotalDiskSize int64      `json:"total_disk_size"`
	// MemFilePath is the GCS object path of the raw memory file (zstd-compressed).
	// When set, the memory is downloaded as a single file and restored via
	// file-backed mem_backend instead of UFFD lazy loading.
	// MemChunks will be empty/nil for new-style snapshots.
	MemFilePath string `json:"mem_file_path,omitempty"`
	// RepoCacheSeedChunks holds chunks for the shared Bazel repo cache seed image
	RepoCacheSeedChunks []ChunkRef `json:"repo_cache_seed_chunks,omitempty"`
	// RootfsSourceHash is the SHA-256 hash of the original base rootfs.img used
	// to build this snapshot. Used by incremental builds to detect rootfs changes
	// and fall back to cold boot when the base image has been updated.
	RootfsSourceHash string `json:"rootfs_source_hash,omitempty"`
}

// ChunkRef references a single chunk by its content hash
type ChunkRef struct {
	Offset         int64  `json:"offset"`
	Size           int64  `json:"size"`            // Uncompressed size
	CompressedSize int64  `json:"compressed_size"` // Compressed size in storage
	Hash           string `json:"hash"`            // SHA256 of uncompressed content
}

// ChunkStore manages storage and retrieval of content-addressed chunks
type ChunkStore struct {
	gcsBucket  string
	gcsClient  *storage.Client
	localCache string
	encoder    *zstd.Encoder
	decoder    *zstd.Decoder
	logger     *logrus.Entry

	// In-memory LRU cache for decompressed chunks
	chunkCache *LRUCache

	// Eager prefetching infrastructure
	eagerFetchStack   *boundedstack.BoundedStack[string]
	eagerFetchCtx     context.Context
	eagerFetchCancel  context.CancelFunc
	eagerFetchWg      sync.WaitGroup
	eagerFetchStarted bool
}

// ChunkStoreConfig holds configuration for the chunk store
type ChunkStoreConfig struct {
	GCSBucket           string
	LocalCachePath      string
	ChunkCacheSizeBytes int64 // In-memory cache size (default 2GB)
	Logger              *logrus.Logger
}

// NewChunkStore creates a new chunk store
func NewChunkStore(ctx context.Context, cfg ChunkStoreConfig) (*ChunkStore, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}

	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd encoder: %w", err)
	}

	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd decoder: %w", err)
	}

	// Ensure local cache directory exists
	if cfg.LocalCachePath != "" {
		if err := os.MkdirAll(cfg.LocalCachePath, 0755); err != nil {
			return nil, fmt.Errorf("failed to create local cache directory: %w", err)
		}
	}

	// Create in-memory LRU cache (default 2GB)
	cacheSizeBytes := cfg.ChunkCacheSizeBytes
	if cacheSizeBytes == 0 {
		cacheSizeBytes = 2 * 1024 * 1024 * 1024 // 2GB default
	}
	chunkCache := NewLRUCache(cacheSizeBytes)

	// Create eager fetch stack
	eagerFetchStack, err := boundedstack.New[string](eagerFetchBufferCapacity)
	if err != nil {
		return nil, fmt.Errorf("failed to create eager fetch stack: %w", err)
	}

	eagerCtx, eagerCancel := context.WithCancel(context.Background())

	return &ChunkStore{
		gcsBucket:        cfg.GCSBucket,
		gcsClient:        client,
		localCache:       cfg.LocalCachePath,
		encoder:          encoder,
		decoder:          decoder,
		logger:           logger.WithField("component", "chunk-store"),
		chunkCache:       chunkCache,
		eagerFetchStack:  eagerFetchStack,
		eagerFetchCtx:    eagerCtx,
		eagerFetchCancel: eagerCancel,
	}, nil
}

// StoreChunk stores a chunk and returns its hash
func (cs *ChunkStore) StoreChunk(ctx context.Context, data []byte) (string, int64, error) {
	// Compute hash of uncompressed data
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	// Check if chunk already exists (deduplication)
	exists, err := cs.chunkExists(ctx, hashStr)
	if err != nil {
		cs.logger.WithError(err).WithField("hash", hashStr[:12]).Warn("Failed to check chunk existence")
	} else if exists {
		// Chunk already exists, skip upload
		cs.logger.WithField("hash", hashStr[:12]).Debug("Chunk already exists, skipping upload")
		// Get the compressed size from existing chunk
		compressedSize, _ := cs.getChunkSize(ctx, hashStr)
		return hashStr, compressedSize, nil
	}

	// Compress data
	compressed := cs.encoder.EncodeAll(data, make([]byte, 0, len(data)/2))

	// Store in GCS
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := cs.chunkPath(hashStr)
	obj := bucket.Object(objPath)

	writer := obj.NewWriter(ctx)
	writer.ContentType = "application/octet-stream"

	if _, err := writer.Write(compressed); err != nil {
		writer.Close()
		return "", 0, fmt.Errorf("failed to write chunk: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", 0, fmt.Errorf("failed to close chunk writer: %w", err)
	}

	// Also store in local cache
	if cs.localCache != "" {
		localPath := filepath.Join(cs.localCache, hashStr)
		if err := os.WriteFile(localPath, compressed, 0644); err != nil {
			cs.logger.WithError(err).WithField("hash", hashStr[:12]).Warn("Failed to write chunk to local cache")
		}
	}

	cs.logger.WithFields(logrus.Fields{
		"hash":            hashStr[:12],
		"size":            len(data),
		"compressed_size": len(compressed),
		"ratio":           float64(len(compressed)) / float64(len(data)),
	}).Debug("Stored chunk")

	return hashStr, int64(len(compressed)), nil
}

// GetChunk retrieves a chunk by hash, checking caches in order:
// 1. In-memory LRU cache (fastest)
// 2. Local file cache (fast)
// 3. GCS (slow, network)
func (cs *ChunkStore) GetChunk(ctx context.Context, hash string) ([]byte, error) {
	// 1. Check in-memory LRU cache first (fastest)
	if data, ok := cs.chunkCache.Get(hash); ok {
		return data, nil
	}

	// 2. Check local file cache
	if cs.localCache != "" {
		localPath := filepath.Join(cs.localCache, hash)
		if compressed, err := os.ReadFile(localPath); err == nil {
			data, err := cs.decoder.DecodeAll(compressed, nil)
			if err != nil {
				cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to decompress cached chunk")
			} else {
				// Add to in-memory cache
				cs.chunkCache.Put(hash, data)
				return data, nil
			}
		}
	}

	// 3. Fetch from GCS
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := cs.chunkPath(hash)
	obj := bucket.Object(objPath)

	reader, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open chunk %s: %w", hash[:12], err)
	}
	defer reader.Close()

	compressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk %s: %w", hash[:12], err)
	}

	// Decompress
	data, err := cs.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress chunk %s: %w", hash[:12], err)
	}

	// Cache locally (compressed)
	if cs.localCache != "" {
		localPath := filepath.Join(cs.localCache, hash)
		if err := os.WriteFile(localPath, compressed, 0644); err != nil {
			cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to cache chunk locally")
		}
	}

	// Add to in-memory cache (decompressed)
	cs.chunkCache.Put(hash, data)

	return data, nil
}

// GetChunkToFile retrieves a chunk and writes it directly to a file at the given offset
func (cs *ChunkStore) GetChunkToFile(ctx context.Context, hash string, file *os.File, offset int64) error {
	data, err := cs.GetChunk(ctx, hash)
	if err != nil {
		return err
	}

	_, err = file.WriteAt(data, offset)
	return err
}

// IsZeroChunk returns true if the ChunkRef represents a zero chunk (never stored).
func (r *ChunkRef) IsZeroChunk() bool {
	return r.Hash == ZeroChunkHash
}

// zeroRef is a pre-allocated zero buffer used for fast zero-chunk detection.
// bytes.Equal uses SIMD-accelerated comparison internally.
var zeroRef = make([]byte, DefaultChunkSize)

// isZeroChunk returns true if data is entirely zeros.
func isZeroChunk(data []byte) bool {
	return bytes.Equal(data, zeroRef[:len(data)])
}

// ChunkFile splits a file into chunks and stores them, returning chunk refs.
// Zero chunks (all-zero data) are detected and skipped — they are recorded with
// Hash="" so that readers can serve zero-filled pages without a network fetch.
// Non-zero chunks are uploaded in parallel for throughput.
func (cs *ChunkStore) ChunkFile(ctx context.Context, path string, chunkSize int64) ([]ChunkRef, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	totalSize := stat.Size()
	numChunks := (totalSize + chunkSize - 1) / chunkSize

	cs.logger.WithFields(logrus.Fields{
		"file":       path,
		"size":       totalSize,
		"chunk_size": chunkSize,
		"num_chunks": numChunks,
	}).Info("Chunking file")

	// Pre-allocate the refs slice so goroutines can write by index.
	refs := make([]ChunkRef, numChunks)

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(chunkFileUploadConcurrency)

	var zeroChunks int64
	buf := make([]byte, chunkSize) // reusable read buffer

	for i := int64(0); i < numChunks; i++ {
		offset := i * chunkSize
		readSize := chunkSize
		if offset+readSize > totalSize {
			readSize = totalSize - offset
		}

		n, err := file.ReadAt(buf[:readSize], offset)
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read at offset %d: %w", offset, err)
		}

		// Fast path: zero chunks are recorded with a sentinel hash and never uploaded.
		if isZeroChunk(buf[:n]) {
			refs[i] = ChunkRef{
				Offset: offset,
				Size:   int64(n),
				Hash:   ZeroChunkHash,
			}
			zeroChunks++
			continue
		}

		// Copy the data so the goroutine owns its buffer while we reuse buf.
		chunkData := make([]byte, n)
		copy(chunkData, buf[:n])

		idx := i
		chunkOffset := offset
		g.Go(func() error {
			hash, compressedSize, err := cs.StoreChunk(gCtx, chunkData)
			if err != nil {
				return fmt.Errorf("failed to store chunk at offset %d: %w", chunkOffset, err)
			}
			refs[idx] = ChunkRef{
				Offset:         chunkOffset,
				Size:           int64(len(chunkData)),
				CompressedSize: compressedSize,
				Hash:           hash,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	cs.logger.WithFields(logrus.Fields{
		"file":         path,
		"total_chunks": numChunks,
		"zero_chunks":  zeroChunks,
		"data_chunks":  numChunks - zeroChunks,
	}).Info("Chunking complete")

	return refs, nil
}

// ReassembleFile reassembles a file from chunks
func (cs *ChunkStore) ReassembleFile(ctx context.Context, refs []ChunkRef, destPath string, totalSize int64) error {
	// Create destination file with proper size
	file, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer file.Close()

	// Pre-allocate file
	if err := file.Truncate(totalSize); err != nil {
		return fmt.Errorf("failed to truncate file: %w", err)
	}

	cs.logger.WithFields(logrus.Fields{
		"dest":       destPath,
		"num_chunks": len(refs),
		"total_size": totalSize,
	}).Info("Reassembling file from chunks")

	// Fetch chunks in parallel
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(chunkFileUploadConcurrency)

	for _, ref := range refs {
		r := ref
		if r.IsZeroChunk() {
			continue // file already zeroed by Truncate
		}
		g.Go(func() error {
			if err := cs.GetChunkToFile(gCtx, r.Hash, file, r.Offset); err != nil {
				return fmt.Errorf("failed to write chunk at offset %d: %w", r.Offset, err)
			}
			return nil
		})
	}

	return g.Wait()
}

// chunkPath returns the GCS path for a chunk
func (cs *ChunkStore) chunkPath(hash string) string {
	// Use hash prefix for better GCS distribution
	return fmt.Sprintf("%s/%s/%s", ChunksPrefix, hash[:2], hash)
}

// chunkExists checks if a chunk already exists in GCS
func (cs *ChunkStore) chunkExists(ctx context.Context, hash string) (bool, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := cs.chunkPath(hash)
	_, err := bucket.Object(objPath).Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// getChunkSize returns the compressed size of a chunk in GCS
func (cs *ChunkStore) getChunkSize(ctx context.Context, hash string) (int64, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := cs.chunkPath(hash)
	attrs, err := bucket.Object(objPath).Attrs(ctx)
	if err != nil {
		return 0, err
	}
	return attrs.Size, nil
}

// UploadRawFile compresses a local file with zstd and uploads it to GCS as a
// single object. Returns the GCS object path and compressed size.
func (cs *ChunkStore) UploadRawFile(ctx context.Context, localPath, gcsObjectPath string) (string, int64, error) {
	start := time.Now()

	srcFile, err := os.Open(localPath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	srcStat, err := srcFile.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("failed to stat source file: %w", err)
	}

	cs.logger.WithFields(logrus.Fields{
		"src":      localPath,
		"dst":      gcsObjectPath,
		"src_size": srcStat.Size(),
	}).Info("Uploading raw file to GCS (zstd-compressed)")

	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	obj := bucket.Object(gcsObjectPath)
	writer := obj.NewWriter(ctx)
	writer.ContentType = "application/zstd"

	// Create a streaming zstd encoder that writes compressed data to GCS
	zw, err := zstd.NewWriter(writer, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		writer.Close()
		return "", 0, fmt.Errorf("failed to create zstd writer: %w", err)
	}

	n, err := io.Copy(zw, srcFile)
	if err != nil {
		zw.Close()
		writer.Close()
		return "", 0, fmt.Errorf("failed to compress and upload: %w", err)
	}

	if err := zw.Close(); err != nil {
		writer.Close()
		return "", 0, fmt.Errorf("failed to close zstd writer: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", 0, fmt.Errorf("failed to close GCS writer: %w", err)
	}

	// Get compressed size from GCS object attributes
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get object attrs: %w", err)
	}

	cs.logger.WithFields(logrus.Fields{
		"src":             localPath,
		"dst":             gcsObjectPath,
		"src_size":        n,
		"compressed_size": attrs.Size,
		"ratio":           float64(attrs.Size) / float64(n),
		"duration":        time.Since(start),
	}).Info("Raw file uploaded to GCS")

	return gcsObjectPath, attrs.Size, nil
}

// DownloadRawFile downloads a zstd-compressed file from GCS and decompresses it
// to localDestPath.
func (cs *ChunkStore) DownloadRawFile(ctx context.Context, gcsObjectPath, localDestPath string) error {
	start := time.Now()

	cs.logger.WithFields(logrus.Fields{
		"src": gcsObjectPath,
		"dst": localDestPath,
	}).Info("Downloading raw file from GCS (zstd-compressed)")

	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	reader, err := bucket.Object(gcsObjectPath).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to open GCS object %s: %w", gcsObjectPath, err)
	}
	defer reader.Close()

	downloadSize := reader.Attrs.Size

	// Create destination file
	if err := os.MkdirAll(filepath.Dir(localDestPath), 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	destFile, err := os.Create(localDestPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	// Create streaming zstd decoder
	zr, err := zstd.NewReader(reader)
	if err != nil {
		return fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zr.Close()

	n, err := io.Copy(destFile, zr)
	if err != nil {
		return fmt.Errorf("failed to decompress and write: %w", err)
	}

	cs.logger.WithFields(logrus.Fields{
		"src":               gcsObjectPath,
		"dst":               localDestPath,
		"download_size":     downloadSize,
		"decompressed_size": n,
		"duration":          time.Since(start),
	}).Info("Raw file downloaded and decompressed")

	return nil
}

// ListChunks lists all chunk hashes stored in GCS under the chunks/ prefix.
func (cs *ChunkStore) ListChunks(ctx context.Context) ([]string, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	prefix := ChunksPrefix + "/"

	it := bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var hashes []string
	for {
		attrs, err := it.Next()
		if err != nil {
			break // done or error
		}
		// Extract hash from path: chunks/<hash>.zst → <hash>
		name := attrs.Name
		name = name[len(prefix):]
		name = strings.TrimSuffix(name, ".zst")
		if name != "" {
			hashes = append(hashes, name)
		}
	}

	return hashes, nil
}

// DeleteChunk deletes a chunk from GCS by its hash.
func (cs *ChunkStore) DeleteChunk(ctx context.Context, hash string) error {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	obj := bucket.Object(cs.chunkPath(hash))
	return obj.Delete(ctx)
}

// Close closes the chunk store
func (cs *ChunkStore) Close() error {
	cs.StopEagerFetcher()
	cs.encoder.Close()
	cs.decoder.Close()
	if cs.gcsClient != nil {
		return cs.gcsClient.Close()
	}
	return nil
}

// StartEagerFetcher starts background goroutines for eager chunk prefetching.
// This should be called after the ChunkStore is created if prefetching is desired.
func (cs *ChunkStore) StartEagerFetcher() {
	if cs.eagerFetchStarted {
		return
	}
	cs.eagerFetchStarted = true

	limiter := rate.NewLimiter(rate.Limit(maxEagerFetchesPerSec), 1)
	eg := &errgroup.Group{}
	eg.SetLimit(eagerFetchConcurrency)

	cs.eagerFetchWg.Add(1)
	go func() {
		defer cs.eagerFetchWg.Done()
		cs.eagerFetchLoop(limiter, eg)
	}()

	cs.logger.Info("Started eager prefetch workers")
}

// StopEagerFetcher stops the background prefetch goroutines.
func (cs *ChunkStore) StopEagerFetcher() {
	if !cs.eagerFetchStarted {
		return
	}
	cs.eagerFetchCancel()
	cs.eagerFetchStack.Close()
	cs.eagerFetchWg.Wait()
	cs.eagerFetchStarted = false
	cs.logger.Info("Stopped eager prefetch workers")
}

// QueueEagerFetch queues chunk hashes for background prefetching.
// This is non-blocking and will drop oldest items if the queue is full.
func (cs *ChunkStore) QueueEagerFetch(hashes []string) {
	if !cs.eagerFetchStarted {
		return
	}
	for _, hash := range hashes {
		cs.eagerFetchStack.Push(hash)
	}
}

// eagerFetchLoop runs the eager fetch worker loop
func (cs *ChunkStore) eagerFetchLoop(limiter *rate.Limiter, eg *errgroup.Group) {
	for {
		hash, err := cs.eagerFetchStack.Recv(cs.eagerFetchCtx)
		if err != nil {
			// Context cancelled or stack closed
			return
		}

		if err := limiter.Wait(cs.eagerFetchCtx); err != nil {
			if cs.eagerFetchCtx.Err() != nil {
				return
			}
			cs.logger.WithError(err).Warn("Eager fetch rate limiter failed")
			return
		}

		// Fetch chunk in background goroutine
		eg.Go(func() error {
			// Check if already in cache
			if _, ok := cs.chunkCache.Get(hash); ok {
				return nil
			}

			// Fetch chunk (this populates both local file cache and in-memory cache)
			_, err := cs.GetChunk(cs.eagerFetchCtx, hash)
			if err != nil {
				cs.logger.WithError(err).WithField("hash", hash[:12]).Debug("Eager fetch failed")
			}
			return nil
		})
	}
}

// ChunkedSnapshotBuilder creates chunked snapshots from existing snapshot files
type ChunkedSnapshotBuilder struct {
	store  *ChunkStore
	logger *logrus.Entry
}

// NewChunkedSnapshotBuilder creates a new chunked snapshot builder
func NewChunkedSnapshotBuilder(store *ChunkStore, logger *logrus.Logger) *ChunkedSnapshotBuilder {
	return &ChunkedSnapshotBuilder{
		store:  store,
		logger: logger.WithField("component", "chunked-snapshot-builder"),
	}
}

// BuildChunkedSnapshot creates a chunked snapshot from traditional snapshot files
func (b *ChunkedSnapshotBuilder) BuildChunkedSnapshot(ctx context.Context, paths *SnapshotPaths, version string) (*ChunkedSnapshotMetadata, error) {
	b.logger.WithField("version", version).Info("Building chunked snapshot")
	start := time.Now()

	meta := &ChunkedSnapshotMetadata{
		Version:   version,
		CreatedAt: time.Now(),
		ChunkSize: DefaultChunkSize,
	}

	// Chunk kernel (small, usually single chunk)
	b.logger.Info("Chunking kernel...")
	kernelData, err := os.ReadFile(paths.Kernel)
	if err != nil {
		return nil, fmt.Errorf("failed to read kernel: %w", err)
	}
	kernelHash, _, err := b.store.StoreChunk(ctx, kernelData)
	if err != nil {
		return nil, fmt.Errorf("failed to store kernel: %w", err)
	}
	meta.KernelHash = kernelHash

	// Chunk state (small, usually single chunk)
	if paths.State != "" {
		b.logger.Info("Chunking state...")
		stateData, err := os.ReadFile(paths.State)
		if err != nil {
			return nil, fmt.Errorf("failed to read state: %w", err)
		}
		stateHash, _, err := b.store.StoreChunk(ctx, stateData)
		if err != nil {
			return nil, fmt.Errorf("failed to store state: %w", err)
		}
		meta.StateHash = stateHash
	}

	// Upload memory file as a single compressed file (not chunked).
	// This enables file-backed restore instead of UFFD lazy loading,
	// eliminating the per-page-fault latency that causes 3-14 minute boot times.
	if paths.Mem != "" {
		b.logger.Info("Uploading raw memory file...")
		memGCSPath := fmt.Sprintf("%s/snapshot.mem.zst", version)
		_, _, err := b.store.UploadRawFile(ctx, paths.Mem, memGCSPath)
		if err != nil {
			return nil, fmt.Errorf("failed to upload raw memory file: %w", err)
		}
		meta.MemFilePath = memGCSPath

		memStat, _ := os.Stat(paths.Mem)
		meta.TotalMemSize = memStat.Size()
	}

	// Chunk rootfs
	b.logger.Info("Chunking rootfs...")
	rootfsChunks, err := b.store.ChunkFile(ctx, paths.Rootfs, DefaultChunkSize)
	if err != nil {
		return nil, fmt.Errorf("failed to chunk rootfs: %w", err)
	}
	meta.RootfsChunks = rootfsChunks

	rootfsStat, _ := os.Stat(paths.Rootfs)
	meta.TotalDiskSize = rootfsStat.Size()

	// Chunk repo cache seed if present
	if paths.RepoCacheSeed != "" {
		b.logger.Info("Chunking repo cache seed...")
		seedChunks, err := b.store.ChunkFile(ctx, paths.RepoCacheSeed, DefaultChunkSize)
		if err != nil {
			return nil, fmt.Errorf("failed to chunk repo cache seed: %w", err)
		}
		meta.RepoCacheSeedChunks = seedChunks
	}

	duration := time.Since(start)
	b.logger.WithFields(logrus.Fields{
		"version":       version,
		"duration":      duration,
		"mem_file_path": meta.MemFilePath,
		"disk_chunks":   len(meta.RootfsChunks),
		"total_chunks":  len(meta.RootfsChunks) + len(meta.RepoCacheSeedChunks) + 2,
	}).Info("Chunked snapshot built successfully")

	return meta, nil
}

// UploadChunkedMetadata uploads the chunked snapshot metadata to GCS
func (b *ChunkedSnapshotBuilder) UploadChunkedMetadata(ctx context.Context, meta *ChunkedSnapshotMetadata) error {
	bucket := b.store.gcsClient.Bucket(b.store.gcsBucket)
	objPath := fmt.Sprintf("%s/chunked-metadata.json", meta.Version)

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	obj := bucket.Object(objPath)
	writer := obj.NewWriter(ctx)
	writer.ContentType = "application/json"

	if _, err := writer.Write(data); err != nil {
		writer.Close()
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close metadata writer: %w", err)
	}

	b.logger.WithField("version", meta.Version).Info("Uploaded chunked metadata")
	return nil
}

// LoadChunkedMetadata loads chunked snapshot metadata from GCS
func (cs *ChunkStore) LoadChunkedMetadata(ctx context.Context, version string) (*ChunkedSnapshotMetadata, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := fmt.Sprintf("%s/chunked-metadata.json", version)

	reader, err := bucket.Object(objPath).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open metadata: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata: %w", err)
	}

	var meta ChunkedSnapshotMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &meta, nil
}
