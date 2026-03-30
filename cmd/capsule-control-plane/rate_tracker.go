package main

import (
	"sync/atomic"
	"time"
)

type allocEvent struct {
	timestamp time.Time
	cpuMilli  int
}

// AllocationRateTracker uses a lock-free circular buffer to record allocation
// attempts and compute an approximate mCPU/s allocation rate over a sliding
// window. The design is intentionally approximate — a torn read on one event
// during Rate() is negligible for a scaling heuristic.
type AllocationRateTracker struct {
	events []allocEvent
	head   atomic.Int64
	window time.Duration
	mask   int64
}

// NewAllocationRateTracker creates a rate tracker. bufferSize must be a power
// of 2 (default 65536). window is the lookback duration (default 60s).
func NewAllocationRateTracker(window time.Duration, bufferSize int) *AllocationRateTracker {
	if bufferSize <= 0 {
		bufferSize = 65536
	}
	// Round up to next power of 2.
	size := 1
	for size < bufferSize {
		size <<= 1
	}
	if window <= 0 {
		window = 60 * time.Second
	}
	return &AllocationRateTracker{
		events: make([]allocEvent, size),
		window: window,
		mask:   int64(size - 1),
	}
}

// Record appends an allocation event. Called from the hot allocation path.
func (t *AllocationRateTracker) Record(cpuMillicores int) {
	idx := t.head.Add(1) - 1
	slot := idx & t.mask
	t.events[slot] = allocEvent{
		timestamp: time.Now(),
		cpuMilli:  cpuMillicores,
	}
}

// Rate returns the allocation rate in mCPU/s over the configured window.
// Only called every ~10s from the downscaler loop.
func (t *AllocationRateTracker) Rate() float64 {
	cutoff := time.Now().Add(-t.window)
	var totalCPU int64
	size := int64(len(t.events))
	head := t.head.Load()

	// Scan at most the entire buffer.
	scan := min(size, head)

	for i := int64(0); i < scan; i++ {
		slot := (head - 1 - i) & t.mask
		ev := t.events[slot]
		if ev.timestamp.Before(cutoff) {
			break
		}
		totalCPU += int64(ev.cpuMilli)
	}

	return float64(totalCPU) / t.window.Seconds()
}

// Count returns the number of events within the window.
func (t *AllocationRateTracker) Count() int64 {
	cutoff := time.Now().Add(-t.window)
	size := int64(len(t.events))
	head := t.head.Load()

	scan := min(size, head)

	var count int64
	for i := int64(0); i < scan; i++ {
		slot := (head - 1 - i) & t.mask
		ev := t.events[slot]
		if ev.timestamp.Before(cutoff) {
			break
		}
		count++
	}

	return count
}
