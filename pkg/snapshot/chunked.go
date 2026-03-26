package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/klauspost/compress/zstd"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"

	fcrotel "github.com/rahul-roy-glean/capsule/pkg/otel"
	"github.com/rahul-roy-glean/capsule/pkg/util/boundedstack"
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

	// defaultGCSMaxAttempts is the default number of GCS fetch attempts
	// before giving up on a chunk (initial attempt + retries).
	defaultGCSMaxAttempts = 3

	// defaultGCSFetchTimeout is the default per-fetch timeout used inside
	// singleflight. Callers may have shorter deadlines but the shared fetch
	// uses this timeout to avoid a short-lived caller poisoning the group.
	defaultGCSFetchTimeout = 10 * time.Second

	// negCacheTTL is the TTL for negative cache entries (404 Not Found).
	negCacheTTL = 5 * time.Second
)

// ErrChunkNotFound is returned when a chunk does not exist in GCS.
var ErrChunkNotFound = errors.New("chunk not found")

// ErrChunkCorruption is returned when a chunk's content hash does not match.
var ErrChunkCorruption = errors.New("chunk corruption detected")

// ChunkedSnapshotMetadata holds metadata for a chunked snapshot.
// Instead of storing full files, we store references to content-addressed chunks.
type ChunkedSnapshotMetadata struct {
	Version       string            `json:"version"`
	RepoCommit    string            `json:"repo_commit,omitempty"`
	Repo          string            `json:"repo,omitempty"`
	WorkloadKey   string            `json:"workload_key,omitempty"`
	Commands      []SnapshotCommand `json:"commands,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	ChunkSize     int64             `json:"chunk_size"`
	KernelHash    string            `json:"kernel_hash"`
	StateHash     string            `json:"state_hash"`
	MemChunks     []ChunkRef        `json:"mem_chunks"`
	RootfsChunks  []ChunkRef        `json:"rootfs_chunks"`
	TotalMemSize  int64             `json:"total_mem_size"`
	TotalDiskSize int64             `json:"total_disk_size"`
	// MemFilePath is the GCS object path of the raw memory file (zstd-compressed).
	// When set, the memory is downloaded as a single file and restored via
	// file-backed mem_backend instead of UFFD lazy loading.
	// MemChunks will be empty/nil for new-style snapshots.
	MemFilePath string `json:"mem_file_path,omitempty"`
	// ExtensionDrives holds chunks for extension block devices, keyed by DriveID.
	ExtensionDrives map[string]ExtensionDrive `json:"extension_drives,omitempty"`
	// RootfsSourceHash is a SHA-256 fingerprint of the effective rootfs
	// provenance inputs used to build this snapshot, such as the source
	// rootfs image or base-image configuration, capsule-thaw-agent binary, and any
	// requested resize. Used by restore paths to detect rootfs changes and
	// fall back to cold boot when resuming would use stale rootfs content.
	RootfsSourceHash string `json:"rootfs_source_hash,omitempty"`
	// RootfsFlavor records the detected rootfs family used for a base-image
	// build (for example "debian-like" or "alpine-like"). Used alongside
	// RootfsSourceHash to scope restore-safety checks to the relevant platform
	// shim contract. Empty for legacy prebuilt rootfs.img snapshots.
	RootfsFlavor string `json:"rootfs_flavor,omitempty"`
	// Layer fields for layered snapshot builds.
	LayerHash       string `json:"layer_hash,omitempty"`
	ParentLayerHash string `json:"parent_layer_hash,omitempty"`
	ParentVersion   string `json:"parent_version,omitempty"`
	LayerDepth      int    `json:"layer_depth,omitempty"`
	LayerName       string `json:"layer_name,omitempty"`
	// MemPrefetchMapping records the page fault access pattern from a previous
	// run for replay during subsequent resumes (access-pattern prefetching).
	MemPrefetchMapping *PrefetchMapping `json:"mem_prefetch_mapping,omitempty"`
}

// PrefetchMapping records page fault order for replay during subsequent resumes.
// Stored as part of ChunkedSnapshotMetadata for access-pattern prefetching.
type PrefetchMapping struct {
	Offsets   []int64 `json:"offsets"`    // sorted by access order
	BlockSize int64   `json:"block_size"` // page size used during recording (typically 4096)
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
	gcsBucket   string
	gcsPrefix   string // top-level prefix for all GCS paths (e.g. "v1")
	gcsClient   *storage.Client
	localCache  string
	chunkSubdir string // "disk", "mem", or "" for legacy flat layout
	encoder     *zstd.Encoder
	decoder     *zstd.Decoder
	logger      *logrus.Entry

	// In-memory LRU cache for decompressed chunks
	chunkCache *LRUCache

	// Singleflight deduplication: coalesces concurrent GCS fetches for the
	// same chunk hash into a single network call.
	fetchGroup singleflight.Group

	// Negative cache: maps hash → expiration time. Only 404 Not Found is
	// cached; 403 Forbidden and transient errors are never cached.
	negCache sync.Map // map[string]time.Time

	// Retry / timeout configuration
	gcsMaxAttempts  int
	gcsFetchTimeout time.Duration
	verifyOnRead    bool

	// Eager prefetching infrastructure
	eagerFetchStack   *boundedstack.BoundedStack[string]
	eagerFetchCtx     context.Context
	eagerFetchCancel  context.CancelFunc
	eagerFetchWg      sync.WaitGroup
	eagerFetchStarted bool

	// Counters for observability (atomic, safe for concurrent access)
	remoteFetches atomic.Uint64 // GCS fetches only (excludes LRU and disk cache hits)

	// OTel instruments (nil-safe: callers that don't provide a Meter get no-ops)
	chunkFetchHist    metric.Float64Histogram
	chunkFetchBytes   metric.Int64Counter
	chunkNegCacheHits metric.Int64Counter
	chunkSFDedup      metric.Int64Counter
}

// ChunkStoreConfig holds configuration for the chunk store
type ChunkStoreConfig struct {
	GCSBucket           string
	GCSPrefix           string // Top-level prefix for all GCS paths (e.g. "v1"); empty means no prefix
	LocalCachePath      string
	ChunkCacheSizeBytes int64  // In-memory cache size (default 2GB)
	ChunkSubdir         string // Subdirectory under chunks/ (e.g. "disk" or "mem"); empty means flat "chunks/"
	Logger              *logrus.Logger

	// GCSMaxAttempts is the maximum number of GCS fetch attempts per chunk
	// (default 3). Includes the initial attempt.
	GCSMaxAttempts int

	// GCSFetchTimeout is the per-fetch timeout used inside singleflight.
	// The caller's context deadline controls how long the caller waits,
	// but the shared fetch uses this timeout so a short-lived caller
	// doesn't cancel the fetch for other waiters (default 10s).
	GCSFetchTimeout time.Duration

	// DisableVerifyOnRead disables SHA-256 verification of decompressed
	// chunks against their expected hash. When false (the zero value),
	// every chunk read is verified; on mismatch the chunk is purged from
	// caches, refetched once, and if still corrupt ErrChunkCorruption is
	// returned. Set to true only for benchmarks or trusted environments.
	DisableVerifyOnRead bool

	// Meter is an OTel metric.Meter used to create chunk-level instruments.
	// If nil, no metrics are recorded.
	Meter metric.Meter
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

	// Apply config defaults
	maxAttempts := cfg.GCSMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultGCSMaxAttempts
	}
	fetchTimeout := cfg.GCSFetchTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = defaultGCSFetchTimeout
	}

	cs := &ChunkStore{
		gcsBucket:        cfg.GCSBucket,
		gcsPrefix:        cfg.GCSPrefix,
		gcsClient:        client,
		localCache:       cfg.LocalCachePath,
		chunkSubdir:      cfg.ChunkSubdir,
		encoder:          encoder,
		decoder:          decoder,
		logger:           logger.WithField("component", "chunk-store"),
		chunkCache:       chunkCache,
		gcsMaxAttempts:   maxAttempts,
		gcsFetchTimeout:  fetchTimeout,
		verifyOnRead:     !cfg.DisableVerifyOnRead,
		eagerFetchStack:  eagerFetchStack,
		eagerFetchCtx:    eagerCtx,
		eagerFetchCancel: eagerCancel,
	}

	// Initialize OTel instruments if a Meter was provided.
	if cfg.Meter != nil {
		cs.chunkFetchHist, _ = fcrotel.NewHistogram(cfg.Meter, fcrotel.ChunkFetchDuration)
		cs.chunkFetchBytes, _ = fcrotel.NewCounter(cfg.Meter, fcrotel.ChunkFetchBytes)
		cs.chunkNegCacheHits, _ = fcrotel.NewCounter(cfg.Meter, fcrotel.ChunkNegCacheHits)
		cs.chunkSFDedup, _ = fcrotel.NewCounter(cfg.Meter, fcrotel.ChunkSingleflightDedup)
	}

	return cs, nil
}

// StoreChunk stores a chunk and returns its hash.
// Deduplication is handled via the in-memory LRU cache: if the chunk is
// already cached we skip the upload entirely. Otherwise we upload
// unconditionally — CAS writes are idempotent so re-uploading an existing
// chunk is harmless and saves the two GCS round-trips (Attrs + getChunkSize)
// that a pre-upload existence check would require.
func (cs *ChunkStore) StoreChunk(ctx context.Context, data []byte) (string, int64, error) {
	// Compute hash of uncompressed data
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	// Fast dedup: if the chunk is in our in-memory LRU, we already have it
	// in GCS (since we only cache after a successful upload or download).
	if _, ok := cs.chunkCache.Get(hashStr); ok {
		// Return estimated compressed size from what we'd produce.
		// The exact stored size doesn't matter for chunk refs since we
		// recompute it from GCS attrs when needed.
		compressed := cs.encoder.EncodeAll(data, make([]byte, 0, len(data)/2))
		return hashStr, int64(len(compressed)), nil
	}

	// Compress data
	compressed := cs.encoder.EncodeAll(data, make([]byte, 0, len(data)/2))

	// Store in GCS (idempotent for same hash)
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

	// Also store in local cache (atomic: write to temp + fsync + rename)
	cs.writeLocalCache(hashStr, compressed)

	// Add to in-memory cache so subsequent StoreChunk calls for the same
	// data skip the upload entirely.
	cs.chunkCache.Put(hashStr, data)

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
// 2. Local file cache (fast, sharded by hash[:2])
// 3. Negative cache (reject known-missing hashes)
// 4. Singleflight-coalesced GCS fetch with retry+backoff
//
// The singleflight layer uses a detached context so a short-lived caller
// doesn't cancel the shared fetch for other waiters. The caller's context
// controls how long *this caller* waits for the result.
func (cs *ChunkStore) GetChunk(ctx context.Context, hash string) ([]byte, error) {
	start := time.Now()

	// 1. Check in-memory LRU cache first (fastest)
	if data, ok := cs.chunkCache.Get(hash); ok {
		if cs.chunkFetchHist != nil {
			cs.chunkFetchHist.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(fcrotel.AttrSource.String("lru_cache")))
		}
		return data, nil
	}

	// 2. Check local file cache (sharded path)
	if data, compressed, ok := cs.readLocalCache(hash); ok {
		cs.chunkCache.Put(hash, data)
		if cs.chunkFetchHist != nil {
			cs.chunkFetchHist.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(fcrotel.AttrSource.String("disk_cache")))
		}
		if cs.chunkFetchBytes != nil {
			cs.chunkFetchBytes.Add(ctx, int64(len(compressed)), metric.WithAttributes(fcrotel.AttrSource.String("disk_cache")))
		}
		return data, nil
	}

	// 3. Check negative cache (known-missing hashes)
	if expiry, ok := cs.negCache.Load(hash); ok {
		if time.Now().Before(expiry.(time.Time)) {
			if cs.chunkNegCacheHits != nil {
				cs.chunkNegCacheHits.Add(ctx, 1)
			}
			return nil, fmt.Errorf("chunk %s: %w (negative cached)", hash[:12], ErrChunkNotFound)
		}
		// Expired entry, remove it
		cs.negCache.Delete(hash)
	}

	// 4. Singleflight-coalesced GCS fetch.
	// Use a detached context inside singleflight so a short-deadline caller
	// doesn't cancel the shared fetch for other waiters.
	type fetchResult struct {
		data       []byte
		compressed []byte
	}
	v, err, shared := cs.fetchGroup.Do(hash, func() (interface{}, error) {
		fetchCtx, fetchCancel := context.WithTimeout(context.Background(), cs.gcsFetchTimeout)
		defer fetchCancel()

		compressed, fetchErr := cs.fetchAndVerify(fetchCtx, hash)
		if fetchErr != nil {
			return nil, fetchErr
		}
		cs.remoteFetches.Add(1)

		data, decErr := cs.decoder.DecodeAll(compressed, nil)
		if decErr != nil {
			return nil, fmt.Errorf("failed to decompress chunk %s: %w", hash[:12], decErr)
		}

		// Verify hash on read
		if cs.verifyOnRead {
			if verifyErr := cs.verifyHash(hash, data); verifyErr != nil {
				// Corrupt from GCS — refetch once bypassing singleflight
				cs.logger.WithField("hash", hash[:12]).Warn("GCS chunk failed hash verification, refetching once")
				compressed2, fetchErr2 := cs.fetchFromGCS(fetchCtx, hash)
				if fetchErr2 != nil {
					return nil, fmt.Errorf("refetch after corruption failed for %s: %w", hash[:12], fetchErr2)
				}
				data2, decErr2 := cs.decoder.DecodeAll(compressed2, nil)
				if decErr2 != nil {
					return nil, fmt.Errorf("failed to decompress refetched chunk %s: %w", hash[:12], decErr2)
				}
				if verifyErr2 := cs.verifyHash(hash, data2); verifyErr2 != nil {
					cs.logger.WithFields(logrus.Fields{
						"hash":   hash[:12],
						"source": "gcs",
					}).Error("Persistent chunk corruption after refetch")
					return nil, fmt.Errorf("chunk %s: %w", hash[:12], ErrChunkCorruption)
				}
				compressed = compressed2
				data = data2
			}
		}

		// Cache locally (atomic write) and in memory
		cs.writeLocalCache(hash, compressed)
		cs.chunkCache.Put(hash, data)

		if cs.chunkFetchHist != nil {
			cs.chunkFetchHist.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(fcrotel.AttrSource.String("gcs")))
		}
		if cs.chunkFetchBytes != nil {
			cs.chunkFetchBytes.Add(ctx, int64(len(compressed)), metric.WithAttributes(fcrotel.AttrSource.String("gcs")))
		}

		return &fetchResult{data: data, compressed: compressed}, nil
	})

	if shared {
		if cs.chunkSFDedup != nil {
			cs.chunkSFDedup.Add(ctx, 1)
		}
	}

	// Check caller context after singleflight returns — even if the fetch
	// succeeded, respect the caller's deadline.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if err != nil {
		return nil, err
	}

	result := v.(*fetchResult)
	return result.data, nil
}

// fetchFromGCS performs a single GCS read for a chunk hash. Returns the
// compressed bytes. Does NOT retry — callers use fetchAndVerify for that.
func (cs *ChunkStore) fetchFromGCS(ctx context.Context, hash string) ([]byte, error) {
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

	return compressed, nil
}

// fetchAndVerify wraps fetchFromGCS with retry+backoff.
// Up to GCSMaxAttempts attempts with jittered exponential backoff.
// Only retryable errors (5xx, timeout, connection reset) are retried.
// Non-retryable errors (404, 403) return immediately.
func (cs *ChunkStore) fetchAndVerify(ctx context.Context, hash string) ([]byte, error) {
	var lastErr error
	backoff := 20 * time.Millisecond

	for attempt := 1; attempt <= cs.gcsMaxAttempts; attempt++ {
		compressed, err := cs.fetchFromGCS(ctx, hash)
		if err == nil {
			return compressed, nil
		}

		lastErr = err

		// Classify the error
		if !cs.isRetryable(err) {
			// Non-retryable: 404, 403, etc. — fail immediately
			if cs.isNotFound(err) {
				// Add to negative cache
				cs.negCache.Store(hash, time.Now().Add(negCacheTTL))
				return nil, fmt.Errorf("chunk %s: %w", hash[:12], ErrChunkNotFound)
			}
			return nil, err
		}

		if attempt < cs.gcsMaxAttempts {
			// Check context before sleeping
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			// Jittered exponential backoff
			jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
			sleepDur := backoff + jitter
			cs.logger.WithFields(logrus.Fields{
				"hash":    hash[:12],
				"attempt": attempt,
				"backoff": sleepDur,
			}).Warn("Retrying GCS fetch after transient error")

			select {
			case <-time.After(sleepDur):
			case <-ctx.Done():
				return nil, ctx.Err()
			}

			backoff *= 2
		}
	}

	return nil, fmt.Errorf("chunk %s: all %d fetch attempts failed: %w", hash[:12], cs.gcsMaxAttempts, lastErr)
}

// isRetryable returns true if the error is a transient GCS error worth retrying:
// 5xx server errors, timeouts, connection resets.
func (cs *ChunkStore) isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Check for Google API HTTP error codes
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code >= 500 // 5xx
	}

	// Check for context deadline exceeded (timeout)
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Check for connection reset
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}

	// Check for HTTP transport errors
	if errors.Is(err, http.ErrHandlerTimeout) {
		return true
	}

	errStr := err.Error()
	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "TLS handshake timeout")
}

// isNotFound returns true if the error indicates the GCS object doesn't exist.
func (cs *ChunkStore) isNotFound(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusNotFound
	}
	return false
}

// verifyHash checks that the SHA-256 of data matches the expected hash.
func (cs *ChunkStore) verifyHash(expectedHash string, data []byte) error {
	actual := sha256.Sum256(data)
	actualStr := hex.EncodeToString(actual[:])
	if actualStr != expectedHash {
		return fmt.Errorf("hash mismatch: expected %s, got %s", expectedHash[:12], actualStr[:12])
	}
	return nil
}

// localChunkPath returns the sharded local cache path for a chunk hash.
// Layout: {localCache}/{hash[:2]}/{hash} — prevents millions of files in one dir.
func (cs *ChunkStore) localChunkPath(hash string) string {
	return filepath.Join(cs.localCache, hash[:2], hash)
}

// readLocalCache attempts to read and decompress a chunk from the local disk
// cache. Returns (decompressed, compressed, true) on success.
// Returns (nil, nil, false) on miss, decompress error, or hash mismatch.
// On corrupt/invalid entries the cache file is removed automatically.
func (cs *ChunkStore) readLocalCache(hash string) ([]byte, []byte, bool) {
	if cs.localCache == "" {
		return nil, nil, false
	}

	localPath := cs.localChunkPath(hash)
	compressed, err := os.ReadFile(localPath)
	if err != nil {
		return nil, nil, false
	}

	data, err := cs.decoder.DecodeAll(compressed, nil)
	if err != nil {
		cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to decompress cached chunk, removing")
		os.Remove(localPath)
		return nil, nil, false
	}

	if cs.verifyOnRead {
		if verifyErr := cs.verifyHash(hash, data); verifyErr != nil {
			cs.logger.WithError(verifyErr).WithField("hash", hash[:12]).Warn("Local cache chunk failed hash verification, removing")
			os.Remove(localPath)
			cs.chunkCache.Remove(hash)
			return nil, nil, false
		}
	}

	return data, compressed, true
}

// writeLocalCache atomically writes compressed chunk data to the local file
// cache. Uses write-to-temp + fsync + rename to prevent partial/corrupt files
// on crash.
func (cs *ChunkStore) writeLocalCache(hash string, compressed []byte) {
	if cs.localCache == "" {
		return
	}

	destPath := cs.localChunkPath(hash)
	dir := filepath.Dir(destPath)

	if err := os.MkdirAll(dir, 0755); err != nil {
		cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to create local cache shard dir")
		return
	}

	// Write to temp file in the same directory (same filesystem for rename)
	tmp, err := os.CreateTemp(dir, ".chunk-*")
	if err != nil {
		cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to create temp file for local cache")
		return
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(compressed); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to write chunk to temp file")
		return
	}

	// fsync to ensure data is on disk before rename
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to fsync chunk temp file")
		return
	}
	tmp.Close()

	// Atomic rename
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		cs.logger.WithError(err).WithField("hash", hash[:12]).Warn("Failed to rename chunk to local cache")
	}
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

// gcsPath prepends the configured GCS prefix to a path.
// E.g. with prefix "v1": gcsPath("chunks/disk/ab/abc") → "v1/chunks/disk/ab/abc"
func (cs *ChunkStore) gcsPath(path string) string {
	if cs.gcsPrefix != "" {
		return cs.gcsPrefix + "/" + path
	}
	return path
}

// GCSPrefix returns the configured GCS prefix (e.g. "v1").
func (cs *ChunkStore) GCSPrefix() string {
	return cs.gcsPrefix
}

// chunkPath returns the GCS path for a chunk.
// Layout: chunks/{subdir}/{hash[:2]}/{hash} (e.g. chunks/disk/ab/abcdef...)
func (cs *ChunkStore) chunkPath(hash string) string {
	var raw string
	if cs.chunkSubdir != "" {
		raw = fmt.Sprintf("%s/%s/%s/%s", ChunksPrefix, cs.chunkSubdir, hash[:2], hash)
	} else {
		raw = fmt.Sprintf("%s/%s/%s", ChunksPrefix, hash[:2], hash)
	}
	return cs.gcsPath(raw)
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
	rawPrefix := ChunksPrefix + "/"
	if cs.chunkSubdir != "" {
		rawPrefix = ChunksPrefix + "/" + cs.chunkSubdir + "/"
	}
	prefix := cs.gcsPath(rawPrefix)

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

// GetChunkCreationTime returns the creation time of a chunk in GCS.
func (cs *ChunkStore) GetChunkCreationTime(ctx context.Context, hash string) (time.Time, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	attrs, err := bucket.Object(cs.chunkPath(hash)).Attrs(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get chunk attrs for %s: %w", hash[:12], err)
	}
	return attrs.Created, nil
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

// CacheStats returns statistics for the in-memory LRU chunk cache.
func (cs *ChunkStore) CacheStats() CacheStats {
	return cs.chunkCache.Stats()
}

// RemoteFetches returns the total number of chunks fetched from GCS (not served
// from LRU or disk cache). This count only includes successful fetches.
func (cs *ChunkStore) RemoteFetches() uint64 {
	return cs.remoteFetches.Load()
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

// eagerFetchLoop runs the eager fetch worker loop.
// Uses a fixed worker pool (N=eagerFetchConcurrency) with the rate limiter
// inside each worker (not in the producer), preventing unbounded goroutine
// growth under slow GCS.
func (cs *ChunkStore) eagerFetchLoop(limiter *rate.Limiter, _ *errgroup.Group) {
	// Channel acts as a bounded work queue for the fixed worker pool.
	work := make(chan string, eagerFetchBufferCapacity)

	// Spawn fixed worker pool
	var workerWg sync.WaitGroup
	for i := 0; i < eagerFetchConcurrency; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for hash := range work {
				// Rate limit inside the worker, not the producer
				if err := limiter.Wait(cs.eagerFetchCtx); err != nil {
					return
				}

				// Skip if already cached
				if _, ok := cs.chunkCache.Get(hash); ok {
					continue
				}

				// Skip negative-cached hashes
				if expiry, ok := cs.negCache.Load(hash); ok {
					if time.Now().Before(expiry.(time.Time)) {
						continue
					}
				}

				// Fetch chunk (goes through singleflight, correctly
				// coalesces with demand fetches)
				_, err := cs.GetChunk(cs.eagerFetchCtx, hash)
				if err != nil {
					cs.logger.WithError(err).WithField("hash", hash[:12]).Debug("Eager fetch failed")
				}
			}
		}()
	}

	// Producer: pop from stack, push to work channel
producerLoop:
	for {
		hash, err := cs.eagerFetchStack.Recv(cs.eagerFetchCtx)
		if err != nil {
			break
		}
		select {
		case work <- hash:
		case <-cs.eagerFetchCtx.Done():
			break producerLoop
		default:
			// Queue full, drop this prefetch request
		}
	}

	close(work)
	workerWg.Wait()
}

// ChunkedSnapshotBuilder creates chunked snapshots from existing snapshot files
type ChunkedSnapshotBuilder struct {
	store    *ChunkStore // disk chunks (rootfs, kernel, state, extension drives)
	memStore *ChunkStore // memory chunks (UFFD); nil falls back to store
	logger   *logrus.Entry
	// MemBackend controls how memory is stored: "chunked" (UFFD lazy via MemChunks,
	// default) or "file" (single compressed blob via MemFilePath for file-backed restore).
	MemBackend string
}

// NewChunkedSnapshotBuilder creates a new chunked snapshot builder.
// memStore is used for memory chunks (chunks/mem/); if nil, store is used for everything.
func NewChunkedSnapshotBuilder(store *ChunkStore, memStore *ChunkStore, logger *logrus.Logger) *ChunkedSnapshotBuilder {
	return &ChunkedSnapshotBuilder{
		store:      store,
		memStore:   memStore,
		logger:     logger.WithField("component", "chunked-snapshot-builder"),
		MemBackend: "chunked",
	}
}

// getMemStore returns the chunk store to use for memory chunks.
func (b *ChunkedSnapshotBuilder) getMemStore() *ChunkStore {
	if b.memStore != nil {
		return b.memStore
	}
	return b.store
}

// BuildChunkedSnapshot creates a chunked snapshot from traditional snapshot files.
// workloadKey is used for scoping GCS paths under {workloadKey}/snapshot_state/{version}/.
// driveSpecs lists extension drives to chunk; each spec's image is expected at
// paths.ExtensionDriveImages[spec.DriveID].
func (b *ChunkedSnapshotBuilder) BuildChunkedSnapshot(ctx context.Context, paths *SnapshotPaths, driveSpecs []DriveSpec, version, workloadKey string) (*ChunkedSnapshotMetadata, error) {
	b.logger.WithField("version", version).Info("Building chunked snapshot")
	start := time.Now()

	meta := &ChunkedSnapshotMetadata{
		Version:     version,
		WorkloadKey: workloadKey,
		CreatedAt:   time.Now(),
		ChunkSize:   DefaultChunkSize,
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

	// Chunk memory file into MemChunks for UFFD lazy loading.
	// Previously this was uploaded as a single compressed file (MemFilePath) to
	// avoid per-page-fault GCS latency, but that required downloading the full
	// 8GB snapshot.mem per repo per host before any VM could start. With the
	// chunk LRU cache and eager prefetcher, UFFD lazy loading is fast enough and
	// avoids the upfront download cost entirely.
	if paths.Mem != "" {
		if b.MemBackend == "file" {
			b.logger.Info("Uploading raw memory file (file-backed restore)...")
			memGCSPath := b.store.gcsPath(fmt.Sprintf("%s/snapshot_state/%s/snapshot.mem.zst", meta.WorkloadKey, version))
			_, _, err := b.getMemStore().UploadRawFile(ctx, paths.Mem, memGCSPath)
			if err != nil {
				return nil, fmt.Errorf("failed to upload raw memory file: %w", err)
			}
			meta.MemFilePath = memGCSPath
		} else {
			b.logger.Info("Chunking memory file for UFFD lazy loading...")
			memChunks, err := b.getMemStore().ChunkFile(ctx, paths.Mem, DefaultChunkSize)
			if err != nil {
				return nil, fmt.Errorf("failed to chunk memory file: %w", err)
			}
			meta.MemChunks = memChunks
		}

		memStat, _ := os.Stat(paths.Mem)
		if memStat != nil {
			meta.TotalMemSize = memStat.Size()
		}
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

	// Chunk extension drives
	if len(driveSpecs) > 0 {
		meta.ExtensionDrives = make(map[string]ExtensionDrive, len(driveSpecs))
		for _, spec := range driveSpecs {
			imgPath, ok := paths.ExtensionDriveImages[spec.DriveID]
			if !ok || imgPath == "" {
				continue
			}
			b.logger.WithField("drive_id", spec.DriveID).Info("Chunking extension drive...")
			driveChunks, err := b.store.ChunkFile(ctx, imgPath, DefaultChunkSize)
			if err != nil {
				return nil, fmt.Errorf("failed to chunk extension drive %s: %w", spec.DriveID, err)
			}
			driveStat, _ := os.Stat(imgPath)
			var driveSize int64
			if driveStat != nil {
				driveSize = driveStat.Size()
			}
			meta.ExtensionDrives[spec.DriveID] = ExtensionDrive{
				Chunks:    driveChunks,
				ReadOnly:  spec.ReadOnly,
				SizeBytes: driveSize,
				Label:     spec.Label,
				MountPath: spec.MountPath,
			}
		}
	}

	duration := time.Since(start)
	b.logger.WithFields(logrus.Fields{
		"version":          version,
		"duration":         duration,
		"mem_file_path":    meta.MemFilePath,
		"disk_chunks":      len(meta.RootfsChunks),
		"extension_drives": len(meta.ExtensionDrives),
	}).Info("Chunked snapshot built successfully")

	return meta, nil
}

// UploadChunkedMetadata uploads the chunked snapshot metadata to GCS.
// Path: {workload_key}/snapshot_state/{version}/chunked-metadata.json
func (b *ChunkedSnapshotBuilder) UploadChunkedMetadata(ctx context.Context, meta *ChunkedSnapshotMetadata) error {
	bucket := b.store.gcsClient.Bucket(b.store.gcsBucket)
	objPath := b.store.gcsPath(fmt.Sprintf("%s/snapshot_state/%s/chunked-metadata.json", meta.WorkloadKey, meta.Version))

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

// ReadCurrentVersion reads the current-pointer.json for a workload key and returns
// the version string it points to.
func (cs *ChunkStore) ReadCurrentVersion(ctx context.Context, workloadKey string) (string, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := cs.gcsPath(workloadKey + "/current-pointer.json")

	reader, err := bucket.Object(objPath).NewReader(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to open current pointer: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to read current pointer: %w", err)
	}

	var pointer struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pointer); err != nil {
		return "", fmt.Errorf("failed to parse current pointer: %w", err)
	}
	if pointer.Version == "" {
		return "", fmt.Errorf("current pointer has empty version")
	}
	return pointer.Version, nil
}

// LoadChunkedMetadata loads chunked snapshot metadata from GCS.
// Path: {workload_key}/snapshot_state/{version}/chunked-metadata.json
func (cs *ChunkStore) LoadChunkedMetadata(ctx context.Context, workloadKey, version string) (*ChunkedSnapshotMetadata, error) {
	bucket := cs.gcsClient.Bucket(cs.gcsBucket)
	objPath := cs.gcsPath(fmt.Sprintf("%s/snapshot_state/%s/chunked-metadata.json", workloadKey, version))

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
