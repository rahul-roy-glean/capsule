//go:build !linux
// +build !linux

package fuse

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

// ChunkedDisk is a stub for non-Linux platforms
type ChunkedDisk struct{}

// ChunkedDiskConfig holds configuration
type ChunkedDiskConfig struct {
	ChunkStore *snapshot.ChunkStore
	Chunks     []snapshot.ChunkRef
	TotalSize  int64
	ChunkSize  int64
	MountPoint string
	Logger     *logrus.Logger
}

// NewChunkedDisk returns an error on non-Linux platforms
func NewChunkedDisk(cfg ChunkedDiskConfig) (*ChunkedDisk, error) {
	return nil, fmt.Errorf("FUSE is only supported on Linux")
}

// Mount is a stub
func (d *ChunkedDisk) Mount() error {
	return fmt.Errorf("FUSE is only supported on Linux")
}

// Unmount is a stub
func (d *ChunkedDisk) Unmount() error {
	return nil
}

// DiskImagePath is a stub
func (d *ChunkedDisk) DiskImagePath() string {
	return ""
}

// GetDirtyChunks is a stub
func (d *ChunkedDisk) GetDirtyChunks() map[int][]byte {
	return nil
}

// DirtyChunkCount is a stub
func (d *ChunkedDisk) DirtyChunkCount() int {
	return 0
}

// Stats is a stub
func (d *ChunkedDisk) Stats() DiskStats {
	return DiskStats{}
}

// DiskStats holds disk statistics
type DiskStats struct {
	Reads       uint64
	Writes      uint64
	ChunkReads  uint64
	DirtyWrites uint64
	DirtyChunks int
}

// SaveDirtyChunks is a stub
func (d *ChunkedDisk) SaveDirtyChunks(ctx context.Context) ([]snapshot.ChunkRef, error) {
	return nil, fmt.Errorf("FUSE is only supported on Linux")
}
