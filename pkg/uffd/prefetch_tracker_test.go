//go:build linux
// +build linux

package uffd

import (
	"sync"
	"testing"
)

func TestPrefetchTracker_FirstWriteWins(t *testing.T) {
	tracker := NewPrefetchTracker(4096)

	tracker.Add(0)
	tracker.Add(4096)
	tracker.Add(0) // duplicate — should be ignored

	if tracker.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", tracker.Len())
	}

	mapping := tracker.GetMapping()
	if mapping == nil {
		t.Fatal("GetMapping() returned nil")
	}
	if len(mapping.Offsets) != 2 {
		t.Fatalf("len(Offsets) = %d, want 2", len(mapping.Offsets))
	}
	if mapping.Offsets[0] != 0 {
		t.Errorf("Offsets[0] = %d, want 0", mapping.Offsets[0])
	}
	if mapping.Offsets[1] != 4096 {
		t.Errorf("Offsets[1] = %d, want 4096", mapping.Offsets[1])
	}
}

func TestPrefetchTracker_AccessOrder(t *testing.T) {
	tracker := NewPrefetchTracker(4096)

	offsets := []int64{8192, 0, 4096, 16384}
	for _, off := range offsets {
		tracker.Add(off)
	}

	mapping := tracker.GetMapping()
	if mapping == nil {
		t.Fatal("GetMapping() returned nil")
	}
	if len(mapping.Offsets) != len(offsets) {
		t.Fatalf("len(Offsets) = %d, want %d", len(mapping.Offsets), len(offsets))
	}
	for i, want := range offsets {
		if mapping.Offsets[i] != want {
			t.Errorf("Offsets[%d] = %d, want %d", i, mapping.Offsets[i], want)
		}
	}
}

func TestPrefetchTracker_GetMappingStopsTracking(t *testing.T) {
	tracker := NewPrefetchTracker(4096)

	tracker.Add(0)
	mapping := tracker.GetMapping()
	if mapping == nil {
		t.Fatal("GetMapping() returned nil")
	}

	// Add after GetMapping should be a no-op
	tracker.Add(4096)
	if tracker.Len() != 1 {
		t.Errorf("Len() = %d after GetMapping+Add, want 1 (tracking should be stopped)", tracker.Len())
	}
}

func TestPrefetchTracker_EmptyReturnsNil(t *testing.T) {
	tracker := NewPrefetchTracker(4096)
	mapping := tracker.GetMapping()
	if mapping != nil {
		t.Errorf("GetMapping() = %v, want nil for empty tracker", mapping)
	}
}

func TestPrefetchTracker_BlockSize(t *testing.T) {
	tracker := NewPrefetchTracker(4096)
	tracker.Add(0)
	mapping := tracker.GetMapping()
	if mapping.BlockSize != 4096 {
		t.Errorf("BlockSize = %d, want 4096", mapping.BlockSize)
	}
}

func TestPrefetchTracker_ConcurrentAdds(t *testing.T) {
	tracker := NewPrefetchTracker(4096)
	const numGoroutines = 50
	const offsetsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < offsetsPerGoroutine; i++ {
				// Each goroutine writes overlapping offsets
				tracker.Add(int64(i) * 4096)
				// And some unique ones
				tracker.Add(int64(g*offsetsPerGoroutine+i) * 4096)
			}
		}()
	}
	wg.Wait()

	mapping := tracker.GetMapping()
	if mapping == nil {
		t.Fatal("GetMapping() returned nil")
	}

	// Should have at most numGoroutines*offsetsPerGoroutine + offsetsPerGoroutine unique offsets
	// (the overlapping ones from i*4096 are deduplicated)
	// At minimum, should have offsetsPerGoroutine unique offsets from the shared range
	if len(mapping.Offsets) < offsetsPerGoroutine {
		t.Errorf("len(Offsets) = %d, want >= %d", len(mapping.Offsets), offsetsPerGoroutine)
	}

	// Verify no duplicate offsets
	seen := make(map[int64]bool)
	for _, off := range mapping.Offsets {
		if seen[off] {
			t.Errorf("duplicate offset %d in mapping", off)
		}
		seen[off] = true
	}
}
