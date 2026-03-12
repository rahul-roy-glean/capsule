//go:build linux
// +build linux

package uffd

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"testing"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// ---------------------------------------------------------------------------
// findChunk binary search — hot path in every page fault
// ---------------------------------------------------------------------------

// makeHandler builds a Handler with a realistic chunk lookup table.
// numChunks simulates an 8GB VM with 4MB chunks = 2048 entries.
func makeTestHandler(numChunks int) *Handler {
	chunkSize := int64(snapshot.DefaultChunkSize)
	chunks := make([]snapshot.ChunkRef, numChunks)
	for i := range chunks {
		chunks[i] = snapshot.ChunkRef{
			Offset: int64(i) * chunkSize,
			Size:   chunkSize,
			Hash:   fmt.Sprintf("hash%012d", i),
		}
	}
	return &Handler{
		chunkLookup: chunks,
	}
}

// BenchmarkFindChunk_2048 simulates page fault chunk lookups for an 8GB VM.
// 2048 chunks of 4MB = 8GB. Binary search should be ~11 comparisons.
func BenchmarkFindChunk_2048(b *testing.B) {
	h := makeTestHandler(2048)
	chunkSize := uint64(snapshot.DefaultChunkSize)
	// Pre-compute random offsets within the valid range
	offsets := make([]uint64, 4096)
	for i := range offsets {
		chunkIdx := rand.Intn(2048)
		pageInChunk := rand.Intn(int(chunkSize / PageSize))
		offsets[i] = uint64(chunkIdx)*chunkSize + uint64(pageInChunk)*PageSize
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.findChunk(offsets[i%4096])
	}
}

// BenchmarkFindChunk_512 simulates a 2GB VM.
func BenchmarkFindChunk_512(b *testing.B) {
	h := makeTestHandler(512)
	chunkSize := uint64(snapshot.DefaultChunkSize)
	offsets := make([]uint64, 4096)
	for i := range offsets {
		chunkIdx := rand.Intn(512)
		pageInChunk := rand.Intn(int(chunkSize / PageSize))
		offsets[i] = uint64(chunkIdx)*chunkSize + uint64(pageInChunk)*PageSize
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.findChunk(offsets[i%4096])
	}
}

// BenchmarkFindChunk_Miss benchmarks lookup for an offset beyond all chunks.
func BenchmarkFindChunk_Miss(b *testing.B) {
	h := makeTestHandler(2048)
	beyondEnd := uint64(2048) * uint64(snapshot.DefaultChunkSize)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.findChunk(beyondEnd + uint64(i%100)*PageSize)
	}
}

// ---------------------------------------------------------------------------
// bitmapHasPage — replaces per-fault SEEK_DATA syscall in LayeredHandler
// ---------------------------------------------------------------------------

// makeBitmap creates a bitmap with the given density of set bits.
func makeBitmap(numPages int64, density float64) []uint64 {
	numWords := (numPages + 63) / 64
	bitmap := make([]uint64, numWords)
	numSet := int(float64(numPages) * density)
	step := int(numPages) / numSet
	if step < 1 {
		step = 1
	}
	for i := 0; i < numSet; i++ {
		page := int64(i * step)
		if page >= numPages {
			break
		}
		bitmap[page/64] |= 1 << (page % 64)
	}
	return bitmap
}

// BenchmarkBitmapHasPage_Hit benchmarks bitmap lookup when the page is present.
func BenchmarkBitmapHasPage_Hit(b *testing.B) {
	// 8GB VM / 4KB pages = 2M pages
	numPages := int64(8 * 1024 * 1024 * 1024 / PageSize)
	bitmap := makeBitmap(numPages, 0.3) // 30% of pages have data

	// Pre-compute offsets that are known to be set
	var hitOffsets []uint64
	for page := int64(0); page < numPages && len(hitOffsets) < 4096; page++ {
		if bitmap[page/64]&(1<<(page%64)) != 0 {
			hitOffsets = append(hitOffsets, uint64(page)*PageSize)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bitmapHasPage(bitmap, hitOffsets[i%len(hitOffsets)])
	}
}

// BenchmarkBitmapHasPage_Miss benchmarks bitmap lookup when the page is absent.
func BenchmarkBitmapHasPage_Miss(b *testing.B) {
	numPages := int64(8 * 1024 * 1024 * 1024 / PageSize)
	bitmap := makeBitmap(numPages, 0.3)

	// Find offsets that are NOT set
	var missOffsets []uint64
	for page := int64(0); page < numPages && len(missOffsets) < 4096; page++ {
		if bitmap[page/64]&(1<<(page%64)) == 0 {
			missOffsets = append(missOffsets, uint64(page)*PageSize)
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bitmapHasPage(bitmap, missOffsets[i%len(missOffsets)])
	}
}

// ---------------------------------------------------------------------------
// Message parsing — compares stack-local vs heap allocation
// ---------------------------------------------------------------------------

// BenchmarkParseUffdMsg_StackBuffer benchmarks the current approach:
// parse from a stack-allocated [32]byte buffer.
func BenchmarkParseUffdMsg_StackBuffer(b *testing.B) {
	// Simulate a page fault message
	var msg [uffdMsgSize]byte
	msg[0] = UFFD_EVENT_PAGEFAULT
	binary.LittleEndian.PutUint64(msg[16:24], 0x7f0000001000) // address

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		event := msg[0]
		addr := binary.LittleEndian.Uint64(msg[16:24])
		// Prevent compiler from optimizing away
		if event == 0 && addr == 0 {
			b.Fatal("unexpected")
		}
	}
}

// BenchmarkParseUffdMsg_HeapBuffer benchmarks the old approach:
// parse from a heap-allocated []byte.
func BenchmarkParseUffdMsg_HeapBuffer(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg := make([]byte, uffdMsgSize)
		msg[0] = UFFD_EVENT_PAGEFAULT
		binary.LittleEndian.PutUint64(msg[16:24], 0x7f0000001000)
		event := msg[0]
		addr := binary.LittleEndian.Uint64(msg[16:24])
		if event == 0 && addr == 0 {
			b.Fatal("unexpected")
		}
	}
}

// ---------------------------------------------------------------------------
// findMapping — linear scan over guest regions
// ---------------------------------------------------------------------------

func BenchmarkFindMapping_3Regions(b *testing.B) {
	h := &Handler{
		mappings: []GuestRegionUFFDMapping{
			{BaseHostVirtAddr: 0x100000, Size: 0x80000000, Offset: 0},              // 2GB
			{BaseHostVirtAddr: 0x80100000, Size: 0x80000000, Offset: 0x80000000},   // 2GB
			{BaseHostVirtAddr: 0x100100000, Size: 0x80000000, Offset: 0x100000000}, // 2GB
		},
	}

	// Addresses in each of the 3 regions
	addrs := []uintptr{0x100000, 0x90000000, 0x140000000}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.findMapping(addrs[i%3])
	}
}
