//go:build linux
// +build linux

package uffd

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// PrefetchEntry records a single page fault with its access order.
type PrefetchEntry struct {
	Offset int64
	Order  uint64
}

// PrefetchTracker records the order of first page faults for a memory region.
// It is integrated into the UFFD handler's hot path: Add() is called after each
// successful UFFDIO_COPY. Only the first access to each offset is recorded
// (first-write-wins via map lookup).
type PrefetchTracker struct {
	mu           sync.Mutex
	entries      map[int64]PrefetchEntry // offset → entry (first-write-wins)
	orderCounter uint64
	blockSize    int64
	tracking     atomic.Bool
}

// NewPrefetchTracker creates a tracker that records page fault order.
// blockSize should match PageSize (4096).
func NewPrefetchTracker(blockSize int64) *PrefetchTracker {
	t := &PrefetchTracker{
		entries:   make(map[int64]PrefetchEntry),
		blockSize: blockSize,
	}
	t.tracking.Store(true)
	return t
}

// Add records a page fault at the given offset. Only the first access to each
// offset is recorded. Safe for concurrent use.
func (t *PrefetchTracker) Add(offset int64) {
	if !t.tracking.Load() {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.entries[offset]; exists {
		return // first-write-wins
	}

	t.entries[offset] = PrefetchEntry{
		Offset: offset,
		Order:  t.orderCounter,
	}
	t.orderCounter++
}

// GetMapping stops tracking and returns the recorded offsets sorted by access
// order. After calling this, further Add() calls are no-ops.
func (t *PrefetchTracker) GetMapping() *snapshot.PrefetchMapping {
	t.tracking.Store(false)

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.entries) == 0 {
		return nil
	}

	// Collect and sort entries by access order
	sorted := make([]PrefetchEntry, 0, len(t.entries))
	for _, e := range t.entries {
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Order < sorted[j].Order
	})

	offsets := make([]int64, len(sorted))
	for i, e := range sorted {
		offsets[i] = e.Offset
	}

	return &snapshot.PrefetchMapping{
		Offsets:   offsets,
		BlockSize: t.blockSize,
	}
}

// Len returns the number of unique offsets recorded so far.
func (t *PrefetchTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}
