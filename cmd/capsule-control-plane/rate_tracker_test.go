package main

import (
	"sync"
	"testing"
	"time"
)

func TestAllocationRateTracker_EmptyReturnsZero(t *testing.T) {
	tracker := NewAllocationRateTracker(60*time.Second, 1024)
	if rate := tracker.Rate(); rate != 0 {
		t.Fatalf("expected Rate()=0 for empty tracker, got %f", rate)
	}
	if count := tracker.Count(); count != 0 {
		t.Fatalf("expected Count()=0 for empty tracker, got %d", count)
	}
}

func TestAllocationRateTracker_SingleEvent(t *testing.T) {
	tracker := NewAllocationRateTracker(60*time.Second, 1024)
	tracker.Record(1000)

	rate := tracker.Rate()
	// 1000 mCPU over 60s window = ~16.67 mCPU/s
	expected := 1000.0 / 60.0
	if rate < expected*0.9 || rate > expected*1.1 {
		t.Fatalf("expected Rate()≈%.2f, got %.2f", expected, rate)
	}
	if count := tracker.Count(); count != 1 {
		t.Fatalf("expected Count()=1, got %d", count)
	}
}

func TestAllocationRateTracker_MultipleEvents(t *testing.T) {
	tracker := NewAllocationRateTracker(60*time.Second, 1024)
	tracker.Record(500)
	tracker.Record(300)
	tracker.Record(200)

	rate := tracker.Rate()
	// 1000 total mCPU over 60s = ~16.67 mCPU/s
	expected := 1000.0 / 60.0
	if rate < expected*0.9 || rate > expected*1.1 {
		t.Fatalf("expected Rate()≈%.2f, got %.2f", expected, rate)
	}
	if count := tracker.Count(); count != 3 {
		t.Fatalf("expected Count()=3, got %d", count)
	}
}

func TestAllocationRateTracker_EventsOutsideWindowIgnored(t *testing.T) {
	// Use a very short window so we can test expiry.
	tracker := NewAllocationRateTracker(50*time.Millisecond, 1024)
	tracker.Record(1000)

	// Wait for the event to fall outside the window.
	time.Sleep(100 * time.Millisecond)

	if rate := tracker.Rate(); rate != 0 {
		t.Fatalf("expected Rate()=0 after window expired, got %f", rate)
	}
	if count := tracker.Count(); count != 0 {
		t.Fatalf("expected Count()=0 after window expired, got %d", count)
	}
}

func TestAllocationRateTracker_ConcurrentRecords(t *testing.T) {
	tracker := NewAllocationRateTracker(60*time.Second, 65536)

	var wg sync.WaitGroup
	goroutines := 10
	recordsPerGoroutine := 1000

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < recordsPerGoroutine; j++ {
				tracker.Record(100)
			}
		}()
	}

	wg.Wait()

	count := tracker.Count()
	expected := int64(goroutines * recordsPerGoroutine)
	if count != expected {
		t.Fatalf("expected Count()=%d after concurrent records, got %d", expected, count)
	}
}

func TestAllocationRateTracker_BufferWraparound(t *testing.T) {
	// Small buffer to force wraparound.
	tracker := NewAllocationRateTracker(60*time.Second, 8)

	// Write more events than the buffer can hold.
	for i := 0; i < 20; i++ {
		tracker.Record(100)
	}

	// Buffer holds 8 events, so only the most recent 8 are accessible.
	// All 8 should still be within the window.
	count := tracker.Count()
	if count != 8 {
		t.Fatalf("expected Count()=8 after wraparound, got %d", count)
	}

	rate := tracker.Rate()
	expected := 800.0 / 60.0 // 8 events * 100 mCPU / 60s
	if rate < expected*0.9 || rate > expected*1.1 {
		t.Fatalf("expected Rate()≈%.2f after wraparound, got %.2f", expected, rate)
	}
}
