//go:build !linux
// +build !linux

package uffd

import "github.com/rahul-roy-glean/capsule/pkg/snapshot"

// PrefetchTracker is a stub for non-Linux platforms.
type PrefetchTracker struct{}

// NewPrefetchTracker is a stub.
func NewPrefetchTracker(blockSize int64) *PrefetchTracker {
	return &PrefetchTracker{}
}

// Add is a stub.
func (t *PrefetchTracker) Add(offset int64) {}

// GetMapping is a stub.
func (t *PrefetchTracker) GetMapping() *snapshot.PrefetchMapping { return nil }

// Len is a stub.
func (t *PrefetchTracker) Len() int { return 0 }
