//go:build !linux
// +build !linux

package runner

import (
	"context"
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// ChunkedManager is a stub for non-Linux platforms
// The real implementation uses UFFD and FUSE which are Linux-only
type ChunkedManager struct {
	*Manager
}

// ChunkedManagerConfig extends HostConfig with chunked snapshot settings
type ChunkedManagerConfig struct {
	HostConfig
	UseChunkedSnapshots bool
	UseNetNS            bool
	ChunkCacheSizeBytes int64
	MemCacheSizeBytes   int64
	MemBackend          string
	GCSPrefix           string
}

// NewChunkedManager returns an error on non-Linux platforms
func NewChunkedManager(ctx context.Context, cfg ChunkedManagerConfig, logger *logrus.Logger) (*ChunkedManager, error) {
	return nil, fmt.Errorf("chunked snapshots (UFFD/FUSE) are only supported on Linux")
}

// AllocateRunnerChunked is a stub
func (cm *ChunkedManager) AllocateRunnerChunked(ctx context.Context, req AllocateRequest) (*Runner, error) {
	return nil, fmt.Errorf("chunked snapshots are only supported on Linux")
}

// ReleaseRunnerChunked is a stub
func (cm *ChunkedManager) ReleaseRunnerChunked(ctx context.Context, runnerID string, saveIncremental bool) error {
	return fmt.Errorf("chunked snapshots are only supported on Linux")
}

// GetChunkedStats is a stub
func (cm *ChunkedManager) GetChunkedStats() ChunkedStats {
	return ChunkedStats{}
}

// ChunkedStats holds statistics (stub)
type ChunkedStats struct {
	DiskCacheSize     int64
	DiskCacheMaxSize  int64
	DiskCacheItems    int
	MemCacheSize      int64
	MemCacheMaxSize   int64
	MemCacheItems     int
	TotalPageFaults   uint64
	TotalCacheHits    uint64
	TotalChunkFetches uint64
	TotalDiskReads    uint64
	TotalDiskWrites   uint64
	TotalDirtyChunks  int
}

// Close is a stub
func (cm *ChunkedManager) Close() error {
	if cm.Manager != nil {
		return cm.Manager.Close()
	}
	return nil
}

// GetChunkedMetadata is a stub
func (cm *ChunkedManager) GetChunkedMetadata() *snapshot.ChunkedSnapshotMetadata {
	return nil
}

// GetChunkStore is a stub
func (cm *ChunkedManager) GetChunkStore() *snapshot.ChunkStore {
	return nil
}

// GetSubnet is a stub
func (cm *ChunkedManager) GetSubnet() *net.IPNet {
	if cm.Manager != nil {
		return cm.Manager.network.GetSubnet()
	}
	return nil
}

// GetLoadedManifests is a stub
func (cm *ChunkedManager) GetLoadedManifests() map[string]string {
	return nil
}

// SyncManifest is a stub
func (cm *ChunkedManager) SyncManifest(ctx context.Context, workloadKey, version string) error {
	return fmt.Errorf("chunked snapshots are only supported on Linux")
}
