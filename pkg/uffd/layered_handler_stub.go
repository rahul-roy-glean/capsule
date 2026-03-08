//go:build !linux
// +build !linux

package uffd

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
)

// LayeredHandler is a stub for non-Linux platforms
type LayeredHandler struct{}

// LayeredHandlerConfig holds configuration for the layered UFFD handler
type LayeredHandlerConfig struct {
	SocketPath       string
	GoldenMemPath    string
	DiffLayers       []string
	PageCacheSize    int
	Logger           *logrus.Logger
	FaultConcurrency int
}

// NewLayeredHandler returns an error on non-Linux platforms
func NewLayeredHandler(cfg LayeredHandlerConfig) (*LayeredHandler, error) {
	return nil, fmt.Errorf("UFFD is only supported on Linux")
}

// Start is a stub
func (h *LayeredHandler) Start() error {
	return fmt.Errorf("UFFD is only supported on Linux")
}

// Stop is a stub
func (h *LayeredHandler) Stop() {}

// Stats is a stub
func (h *LayeredHandler) Stats() LayeredHandlerStats {
	return LayeredHandlerStats{}
}

// LayeredHandlerStats holds layered UFFD handler statistics
type LayeredHandlerStats struct {
	PageFaults  uint64
	CacheHits   uint64
	GoldenReads uint64
	LayerReads  uint64
}

// SocketPath is a stub
func (h *LayeredHandler) SocketPath() string {
	return ""
}

// WaitForConnection is a stub
func (h *LayeredHandler) WaitForConnection(timeout time.Duration) error {
	return fmt.Errorf("UFFD is only supported on Linux")
}
