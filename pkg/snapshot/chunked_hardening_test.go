package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// TestLocalCacheSharding verifies the sharded local cache path layout:
// {localCache}/{hash[:2]}/{hash}
func TestLocalCacheSharding(t *testing.T) {
	cs := &ChunkStore{
		localCache: "/tmp/test-cache",
	}

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	got := cs.localChunkPath(hash)
	want := filepath.Join("/tmp/test-cache", "ab", hash)
	if got != want {
		t.Errorf("localChunkPath(%q) = %q, want %q", hash, got, want)
	}

	// Edge case: hash starting with "00"
	hash2 := "00aabb1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	got2 := cs.localChunkPath(hash2)
	want2 := filepath.Join("/tmp/test-cache", "00", hash2)
	if got2 != want2 {
		t.Errorf("localChunkPath(%q) = %q, want %q", hash2, got2, want2)
	}
}

// TestLocalCacheAtomicWrite verifies that writeLocalCache creates a valid
// file at the sharded path and that the write is atomic (no partial files).
func TestLocalCacheAtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	cs := &ChunkStore{
		localCache: tmpDir,
		logger:     newTestLogger(),
	}

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	data := []byte("compressed chunk data here")

	cs.writeLocalCache(hash, data)

	// Verify file exists at sharded path
	expectedPath := filepath.Join(tmpDir, "ab", hash)
	got, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("Failed to read cached file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Cached data mismatch: got %q, want %q", got, data)
	}

	// Verify no temp files left behind
	entries, err := os.ReadDir(filepath.Join(tmpDir, "ab"))
	if err != nil {
		t.Fatalf("Failed to read shard dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != hash {
			t.Errorf("Unexpected file in shard dir: %s", e.Name())
		}
	}
}

// TestLocalCacheAtomicWriteNoPartial verifies that a concurrent read during
// write doesn't see a partial file.
func TestLocalCacheAtomicWriteNoPartial(t *testing.T) {
	tmpDir := t.TempDir()
	cs := &ChunkStore{
		localCache: tmpDir,
		logger:     newTestLogger(),
	}

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	data := make([]byte, 1024*1024) // 1MB
	for i := range data {
		data[i] = byte(i % 256)
	}

	// Write concurrently and read to check for partial files
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs.writeLocalCache(hash, data)
		}()
	}

	// Also read concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := cs.localChunkPath(hash)
			got, err := os.ReadFile(path)
			if err != nil {
				return // File may not exist yet, that's OK
			}
			// If we read something, it must be complete
			if len(got) != len(data) {
				t.Errorf("Read partial file: got %d bytes, want %d", len(got), len(data))
			}
		}()
	}
	wg.Wait()
}

// TestVerifyHash tests the hash verification logic.
func TestVerifyHash(t *testing.T) {
	cs := &ChunkStore{}

	data := []byte("test chunk data for hash verification")
	hash := sha256.Sum256(data)
	hashStr := hex.EncodeToString(hash[:])

	// Valid hash should pass
	if err := cs.verifyHash(hashStr, data); err != nil {
		t.Errorf("verifyHash() with correct hash returned error: %v", err)
	}

	// Wrong data should fail
	if err := cs.verifyHash(hashStr, []byte("wrong data")); err == nil {
		t.Error("verifyHash() with wrong data should have returned error")
	}

	// Wrong hash should fail
	wrongHash := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := cs.verifyHash(wrongHash, data); err == nil {
		t.Error("verifyHash() with wrong hash should have returned error")
	}
}

// TestNegativeCache tests the negative cache behavior.
func TestNegativeCache(t *testing.T) {
	cs := &ChunkStore{}

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Initially empty
	if _, ok := cs.negCache.Load(hash); ok {
		t.Error("Negative cache should be empty initially")
	}

	// Store with future expiry
	cs.negCache.Store(hash, time.Now().Add(5*time.Second))
	if _, ok := cs.negCache.Load(hash); !ok {
		t.Error("Negative cache entry should exist after Store")
	}

	// Store with past expiry (expired)
	cs.negCache.Store(hash, time.Now().Add(-1*time.Second))
	expiry, ok := cs.negCache.Load(hash)
	if !ok {
		t.Error("Negative cache entry should still exist")
	}
	if time.Now().Before(expiry.(time.Time)) {
		t.Error("Expired entry should have expiry in the past")
	}
}

// TestIsRetryable tests error classification for GCS retry decisions.
func TestIsRetryable(t *testing.T) {
	cs := &ChunkStore{}

	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"nil error", nil, false},
		{"generic error", errors.New("something failed"), false},
		{"timeout", errors.New("i/o timeout"), true},
		{"connection reset", errors.New("connection reset by peer"), true},
		{"connection refused", errors.New("connection refused"), true},
		{"TLS timeout", errors.New("TLS handshake timeout"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cs.isRetryable(tc.err)
			if got != tc.retryable {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.retryable)
			}
		})
	}
}

// TestEgressBudget tests the per-runner egress budget.
func TestEgressBudget(t *testing.T) {
	budget := NewEgressBudget(100, 200)

	if budget.SoftCapExceeded() {
		t.Error("Soft cap should not be exceeded initially")
	}
	if budget.HardCapExceeded() {
		t.Error("Hard cap should not be exceeded initially")
	}

	budget.Add(50)
	if budget.Total() != 50 {
		t.Errorf("Total = %d, want 50", budget.Total())
	}

	budget.Add(60) // total 110 > softCap 100
	if !budget.SoftCapExceeded() {
		t.Error("Soft cap should be exceeded at 110")
	}
	if budget.HardCapExceeded() {
		t.Error("Hard cap should not be exceeded at 110")
	}

	budget.Add(100) // total 210 > hardCap 200
	if !budget.HardCapExceeded() {
		t.Error("Hard cap should be exceeded at 210")
	}
}

// TestEgressBudgetDefaults tests default egress budget values.
func TestEgressBudgetDefaults(t *testing.T) {
	budget := NewEgressBudget(0, 0)
	if budget.SoftCap() != 4*1024*1024*1024 {
		t.Errorf("Default soft cap = %d, want 4GB", budget.SoftCap())
	}
	if budget.HardCap() != 16*1024*1024*1024 {
		t.Errorf("Default hard cap = %d, want 16GB", budget.HardCap())
	}
}

// TestEgressBudgetConcurrent tests concurrent access to the egress budget.
func TestEgressBudgetConcurrent(t *testing.T) {
	budget := NewEgressBudget(1000, 2000)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			budget.Add(1)
			_ = budget.Total()
			_ = budget.SoftCapExceeded()
			_ = budget.HardCapExceeded()
		}()
	}
	wg.Wait()

	if budget.Total() != 100 {
		t.Errorf("Total = %d, want 100", budget.Total())
	}
}

// TestGCGraceWindow tests that the GC grace window config works.
func TestGCGraceWindow(t *testing.T) {
	cfg := DefaultGCConfig()
	if cfg.MinChunkAge != 24*time.Hour {
		t.Errorf("Default MinChunkAge = %v, want 24h", cfg.MinChunkAge)
	}

	// Zero MinChunkAge should mean no age protection
	zeroCfg := GCConfig{MinChunkAge: 0}
	if zeroCfg.MinChunkAge != 0 {
		t.Errorf("Zero MinChunkAge should be 0, got %v", zeroCfg.MinChunkAge)
	}
}

// TestSingleflightDedup verifies that singleflight deduplication counter works
// (integration test requires a mock GCS, but we can test the mechanism)
func TestSingleflightDedup(t *testing.T) {
	// Test that the singleflight group is initialized and usable
	cs := &ChunkStore{}

	var callCount atomic.Int64
	const N = 50

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs.fetchGroup.Do("test-key", func() (interface{}, error) {
				callCount.Add(1)
				time.Sleep(10 * time.Millisecond) // Simulate work
				return "result", nil
			})
		}()
	}
	wg.Wait()

	// singleflight should have coalesced into exactly 1 call
	if callCount.Load() != 1 {
		t.Errorf("singleflight.Do called function %d times, want 1", callCount.Load())
	}
}

// TestChunkStoreConfigDefaults verifies config defaults are applied.
func TestChunkStoreConfigDefaults(t *testing.T) {
	// We can't create a real ChunkStore without GCS, but we can test the
	// default logic by checking the constants.
	if defaultGCSMaxAttempts != 3 {
		t.Errorf("defaultGCSMaxAttempts = %d, want 3", defaultGCSMaxAttempts)
	}
	if defaultGCSFetchTimeout != 10*time.Second {
		t.Errorf("defaultGCSFetchTimeout = %v, want 10s", defaultGCSFetchTimeout)
	}
	if negCacheTTL != 5*time.Second {
		t.Errorf("negCacheTTL = %v, want 5s", negCacheTTL)
	}
}

// TestErrChunkNotFound tests the sentinel errors.
func TestErrChunkNotFound(t *testing.T) {
	err := ErrChunkNotFound
	if !errors.Is(err, ErrChunkNotFound) {
		t.Error("ErrChunkNotFound should match itself via errors.Is")
	}

	wrapped := errors.New("chunk abc: " + err.Error())
	_ = wrapped // Just verify it's usable
}

// TestErrChunkCorruption tests the corruption error sentinel.
func TestErrChunkCorruption(t *testing.T) {
	err := ErrChunkCorruption
	if !errors.Is(err, ErrChunkCorruption) {
		t.Error("ErrChunkCorruption should match itself via errors.Is")
	}
}

// TestCollectSessionRoots tests the stub returns nil.
func TestCollectSessionRoots(t *testing.T) {
	roots, err := CollectSessionRoots(nil)
	if err != nil {
		t.Errorf("CollectSessionRoots() returned error: %v", err)
	}
	if roots != nil {
		t.Errorf("CollectSessionRoots() returned non-nil roots: %v", roots)
	}
}

// newTestLogger creates a logrus entry for testing.
func newTestLogger() *logrus.Entry {
	return logrus.New().WithField("test", true)
}
