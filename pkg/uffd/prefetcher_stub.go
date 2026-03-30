//go:build !linux
// +build !linux

package uffd

import (
	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// Prefetcher is a stub for non-Linux platforms.
type Prefetcher struct{}

// PrefetcherConfig holds configuration for the prefetcher.
type PrefetcherConfig struct {
	Mapping      *snapshot.PrefetchMapping
	ChunkStore   *snapshot.ChunkStore
	Metadata     *snapshot.ChunkedSnapshotMetadata
	Connected    <-chan struct{}
	Logger       *logrus.Logger
	FetchWorkers int
	CopyWorkers  int
}

// NewPrefetcher is a stub.
func NewPrefetcher(cfg PrefetcherConfig) *Prefetcher { return &Prefetcher{} }

// SetUFFD is a stub.
func (p *Prefetcher) SetUFFD(uffdFd int, mappings []GuestRegionUFFDMapping) {}

// Done is a stub — returns a closed channel (always ready).
func (p *Prefetcher) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Start is a stub.
func (p *Prefetcher) Start() {}

// Stop is a stub.
func (p *Prefetcher) Stop() {}
