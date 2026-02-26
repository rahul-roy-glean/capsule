package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// LRU Cache benchmarks — exercises the sharded cache on the page-fault hot path
// ---------------------------------------------------------------------------

func BenchmarkLRUCache_Get_Hit(b *testing.B) {
	cache := NewLRUCache(1 * 1024 * 1024 * 1024) // 1GB
	// Pre-populate with 256 chunks (4MB each, keyed by SHA-256 hash)
	keys := make([]string, 256)
	chunk := make([]byte, DefaultChunkSize)
	for i := range keys {
		chunk[0] = byte(i)
		h := sha256.Sum256(chunk)
		keys[i] = hex.EncodeToString(h[:])
		cache.Put(keys[i], chunk)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cache.Get(keys[i%256])
	}
}

func BenchmarkLRUCache_Get_Miss(b *testing.B) {
	cache := NewLRUCache(1 * 1024 * 1024 * 1024)
	keys := make([]string, 256)
	for i := range keys {
		keys[i] = fmt.Sprintf("miss-%064d", i)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cache.Get(keys[i%256])
	}
}

func BenchmarkLRUCache_Put(b *testing.B) {
	cache := NewLRUCache(256 * DefaultChunkSize) // fits exactly 256 4MB chunks
	chunk := make([]byte, DefaultChunkSize)
	// Pre-compute a fixed set of keys to cycle through
	keys := make([]string, 1024)
	for i := range keys {
		chunk[0] = byte(i)
		chunk[1] = byte(i >> 8)
		h := sha256.Sum256(chunk)
		keys[i] = hex.EncodeToString(h[:])
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cache.Put(keys[i%1024], chunk)
	}
}

// BenchmarkLRUCache_Concurrent simulates the contention pattern during VM
// restore: multiple UFFD handlers and eager-fetch goroutines hitting the cache
// simultaneously.
func BenchmarkLRUCache_Concurrent_8(b *testing.B) {
	benchmarkLRUConcurrent(b, 8)
}

func BenchmarkLRUCache_Concurrent_64(b *testing.B) {
	benchmarkLRUConcurrent(b, 64)
}

func benchmarkLRUConcurrent(b *testing.B, goroutines int) {
	cache := NewLRUCache(1 * 1024 * 1024 * 1024)
	chunk := make([]byte, DefaultChunkSize)
	keys := make([]string, 512)
	for i := range keys {
		chunk[0] = byte(i)
		chunk[1] = byte(i >> 8)
		h := sha256.Sum256(chunk)
		keys[i] = hex.EncodeToString(h[:])
		cache.Put(keys[i], chunk)
	}

	b.ResetTimer()
	b.ReportAllocs()

	var wg sync.WaitGroup
	opsPerGoroutine := b.N / goroutines
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				k := keys[(id*opsPerGoroutine+i)%512]
				if i%10 == 0 {
					cache.Put(k, chunk) // 10% writes
				} else {
					cache.Get(k) // 90% reads
				}
			}
		}(g)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// ChunkIndex <-> Metadata conversion benchmarks
// ---------------------------------------------------------------------------

// makeChunkIndex creates a realistic ChunkIndex simulating a VM with a given
// density of non-zero extents.
func makeChunkIndex(totalSize int64, density float64) *ChunkIndex {
	chunkSize := int64(DefaultChunkSize)
	numChunks := (totalSize + chunkSize - 1) / chunkSize
	numNonZero := int(float64(numChunks) * density)

	idx := &ChunkIndex{
		Version:        "1",
		ChunkSizeBytes: chunkSize,
	}
	idx.CAS.Algo = "sha256"
	idx.CAS.Layout = "chunks/mem/{p0}/{hash}"
	idx.CAS.Kind = "mem"
	idx.Region.Name = "vm_memory"
	idx.Region.LogicalSizeBytes = totalSize
	idx.Region.Coverage = "sparse"
	idx.Region.DefaultFill = "zero"

	// Spread non-zero extents evenly
	step := int(numChunks) / numNonZero
	if step < 1 {
		step = 1
	}
	for i := 0; i < numNonZero; i++ {
		offset := int64(i*step) * chunkSize
		if offset >= totalSize {
			break
		}
		idx.Region.Extents = append(idx.Region.Extents, ManifestChunkRef{
			Offset:       offset,
			Length:       chunkSize,
			Hash:         fmt.Sprintf("hash%012d", i),
			StoredLength: chunkSize / 2,
		})
	}

	return idx
}

// BenchmarkChunkIndexToMetadata_8GB_50pct simulates converting the session
// ChunkIndex for an 8GB VM where 50% of chunks are dirty.
// Note: allocates ~1.5GB for the dense ChunkRef array; run on Linux build hosts.
func BenchmarkChunkIndexToMetadata_8GB_50pct(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 8GB benchmark in short mode")
	}
	idx := makeChunkIndex(8*1024*1024*1024, 0.5) // 8GB, 50% dirty
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ChunkIndexToMetadata(idx)
	}
}

// BenchmarkChunkIndexToMetadata_8GB_5pct — sparse index (typical session with
// few dirty pages).
func BenchmarkChunkIndexToMetadata_8GB_5pct(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 8GB benchmark in short mode")
	}
	idx := makeChunkIndex(8*1024*1024*1024, 0.05)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ChunkIndexToMetadata(idx)
	}
}

func BenchmarkChunkIndexFromMeta(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 8GB benchmark in short mode")
	}
	idx := makeChunkIndex(8*1024*1024*1024, 0.5)
	meta := ChunkIndexToMetadata(idx)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ChunkIndexFromMeta(meta)
	}
}

// Smaller variants that run fast on dev machines (512MB VM)
func BenchmarkChunkIndexToMetadata_512MB_50pct(b *testing.B) {
	idx := makeChunkIndex(512*1024*1024, 0.5)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ChunkIndexToMetadata(idx)
	}
}

func BenchmarkChunkIndexToMetadata_512MB_5pct(b *testing.B) {
	idx := makeChunkIndex(512*1024*1024, 0.05)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ChunkIndexToMetadata(idx)
	}
}

// ---------------------------------------------------------------------------
// isZeroChunk — hot path in session uploader and chunk builder
// ---------------------------------------------------------------------------

func BenchmarkIsZeroChunk_4MB_AllZero(b *testing.B) {
	data := make([]byte, DefaultChunkSize)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		isZeroChunk(data)
	}
}

func BenchmarkIsZeroChunk_4MB_NonZero(b *testing.B) {
	data := make([]byte, DefaultChunkSize)
	data[DefaultChunkSize-1] = 1 // last byte non-zero
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		isZeroChunk(data)
	}
}

func BenchmarkIsZeroChunk_4KB_AllZero(b *testing.B) {
	data := make([]byte, 4096)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		isZeroChunk(data)
	}
}
