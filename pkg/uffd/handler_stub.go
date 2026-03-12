//go:build !linux
// +build !linux

package uffd

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// Handler is a stub for non-Linux platforms
type Handler struct{}

// HandlerConfig holds configuration
type HandlerConfig struct {
	SocketPath             string
	ChunkStore             *snapshot.ChunkStore
	Metadata               *snapshot.ChunkedSnapshotMetadata
	MemStart               uint64
	MemSize                uint64
	Logger                 *logrus.Logger
	FaultTimeout           time.Duration
	MaxConsecutiveFailures int
	OnFatal                func(error)
	Meter                  metric.Meter
	FaultConcurrency       int
	EnablePrefetchTracking bool
}

// NewHandler returns an error on non-Linux platforms
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	return nil, fmt.Errorf("UFFD is only supported on Linux")
}

// Start is a stub
func (h *Handler) Start() error {
	return fmt.Errorf("UFFD is only supported on Linux")
}

// Stop is a stub
func (h *Handler) Stop() {}

// Stats is a stub
func (h *Handler) Stats() HandlerStats {
	return HandlerStats{}
}

// HandlerStats holds handler statistics
type HandlerStats struct {
	PageFaults   uint64
	CacheHits    uint64
	ChunkFetches uint64
}

// SocketPath is a stub
func (h *Handler) SocketPath() string {
	return ""
}

// WaitForConnection is a stub
func (h *Handler) WaitForConnection(timeout time.Duration) error {
	return fmt.Errorf("UFFD is only supported on Linux")
}

// GetPrefetchMapping is a stub
func (h *Handler) GetPrefetchMapping() *snapshot.PrefetchMapping { return nil }

// Mappings is a stub
func (h *Handler) Mappings() []GuestRegionUFFDMapping { return nil }

// Connected is a stub
func (h *Handler) Connected() <-chan struct{} { return nil }

// SetPrefetcher is a stub
func (h *Handler) SetPrefetcher(p *Prefetcher) {}

// GuestRegionUFFDMapping is a stub for non-Linux platforms
type GuestRegionUFFDMapping struct {
	BaseHostVirtAddr uintptr `json:"base_host_virt_addr"`
	Size             uintptr `json:"size"`
	Offset           uintptr `json:"offset"`
}
