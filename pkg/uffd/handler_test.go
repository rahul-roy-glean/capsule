//go:build linux
// +build linux

package uffd

import (
	"testing"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

func TestNewHandler_FaultConcurrency(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
		wantSem     bool
	}{
		{"zero (serial)", 0, false},
		{"one (serial)", 1, false},
		{"32 (concurrent)", 32, true},
		{"2 (concurrent)", 2, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := &snapshot.ChunkedSnapshotMetadata{
				ChunkSize:    snapshot.DefaultChunkSize,
				TotalMemSize: snapshot.DefaultChunkSize,
				MemChunks:    []snapshot.ChunkRef{{Offset: 0, Size: snapshot.DefaultChunkSize, Hash: "test"}},
			}
			h, err := NewHandler(HandlerConfig{
				SocketPath:       "/tmp/test.sock",
				Metadata:         meta,
				FaultConcurrency: tt.concurrency,
			})
			if err != nil {
				t.Fatalf("NewHandler failed: %v", err)
			}
			defer h.Stop()

			if tt.wantSem && h.faultSem == nil {
				t.Error("faultSem is nil, want non-nil for concurrent mode")
			}
			if !tt.wantSem && h.faultSem != nil {
				t.Error("faultSem is non-nil, want nil for serial mode")
			}
			if tt.wantSem && h.faultSem != nil {
				if cap(h.faultSem) != tt.concurrency {
					t.Errorf("faultSem capacity = %d, want %d", cap(h.faultSem), tt.concurrency)
				}
			}
		})
	}
}

func TestNewHandler_PrefetchTracking(t *testing.T) {
	meta := &snapshot.ChunkedSnapshotMetadata{
		ChunkSize:    snapshot.DefaultChunkSize,
		TotalMemSize: snapshot.DefaultChunkSize,
	}

	// Disabled by default
	h, err := NewHandler(HandlerConfig{
		SocketPath: "/tmp/test.sock",
		Metadata:   meta,
	})
	if err != nil {
		t.Fatalf("NewHandler failed: %v", err)
	}
	defer h.Stop()
	if h.prefetchTracker != nil {
		t.Error("prefetchTracker should be nil when disabled")
	}
	if h.GetPrefetchMapping() != nil {
		t.Error("GetPrefetchMapping() should return nil when disabled")
	}

	// Enabled
	h2, err := NewHandler(HandlerConfig{
		SocketPath:             "/tmp/test2.sock",
		Metadata:               meta,
		EnablePrefetchTracking: true,
	})
	if err != nil {
		t.Fatalf("NewHandler failed: %v", err)
	}
	defer h2.Stop()
	if h2.prefetchTracker == nil {
		t.Error("prefetchTracker should be non-nil when enabled")
	}
}

func TestNewHandler_DefaultTimeouts(t *testing.T) {
	h, err := NewHandler(HandlerConfig{
		SocketPath: "/tmp/test.sock",
	})
	if err != nil {
		t.Fatalf("NewHandler failed: %v", err)
	}
	defer h.Stop()

	if h.faultTimeout.Seconds() != 5 {
		t.Errorf("faultTimeout = %v, want 5s", h.faultTimeout)
	}
	if h.maxConsecutiveFailures != 3 {
		t.Errorf("maxConsecutiveFailures = %d, want 3", h.maxConsecutiveFailures)
	}
}
