//go:build !linux
// +build !linux

package snapshot

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
)

// SessionChunkUploader is a stub for non-Linux platforms.
// The Linux implementation uses SEEK_DATA/SEEK_HOLE for sparse file iteration.
type SessionChunkUploader struct{}

// NewSessionChunkUploader creates a stub uploader on non-Linux platforms.
func NewSessionChunkUploader(memStore, diskStore *ChunkStore, logger *logrus.Logger) *SessionChunkUploader {
	return &SessionChunkUploader{}
}

// MergeAndUploadMem is a stub.
func (u *SessionChunkUploader) MergeAndUploadMem(ctx context.Context, memDiffPath string, baseIndex *ChunkIndex) (*ChunkIndex, error) {
	return nil, fmt.Errorf("SessionChunkUploader: MergeAndUploadMem not supported on non-Linux platforms")
}

// MergeAndUploadDisk is a stub.
func (u *SessionChunkUploader) MergeAndUploadDisk(ctx context.Context, dirtyChunks map[int][]byte, baseIndex *ChunkIndex) (*ChunkIndex, error) {
	return nil, fmt.Errorf("SessionChunkUploader: MergeAndUploadDisk not supported on non-Linux platforms")
}

// UploadVMState is a stub.
func (u *SessionChunkUploader) UploadVMState(ctx context.Context, localPath, gcsObjectPath string) error {
	return fmt.Errorf("SessionChunkUploader: UploadVMState not supported on non-Linux platforms")
}

// DownloadVMState is a stub.
func (u *SessionChunkUploader) DownloadVMState(ctx context.Context, gcsObjectPath, localPath string) error {
	return fmt.Errorf("SessionChunkUploader: DownloadVMState not supported on non-Linux platforms")
}

// WriteManifest is a stub.
func (u *SessionChunkUploader) WriteManifest(ctx context.Context, gcsBase string, manifest *SnapshotManifest, memIndex, diskIndex *ChunkIndex) error {
	return fmt.Errorf("SessionChunkUploader: WriteManifest not supported on non-Linux platforms")
}

// WriteManifestWithExtensions is a stub.
func (u *SessionChunkUploader) WriteManifestWithExtensions(ctx context.Context, gcsBase string, manifest *SnapshotManifest, memIndex *ChunkIndex, extDiskIndexes map[string]*ChunkIndex) error {
	return fmt.Errorf("SessionChunkUploader: WriteManifestWithExtensions not supported on non-Linux platforms")
}

// DownloadChunkIndex is a stub.
func (u *SessionChunkUploader) DownloadChunkIndex(ctx context.Context, gcsObjectPath string) (*ChunkIndex, error) {
	return nil, fmt.Errorf("SessionChunkUploader: DownloadChunkIndex not supported on non-Linux platforms")
}

// DownloadManifest is a stub.
func (u *SessionChunkUploader) DownloadManifest(ctx context.Context, gcsObjectPath string) (*SnapshotManifest, error) {
	return nil, fmt.Errorf("SessionChunkUploader: DownloadManifest not supported on non-Linux platforms")
}

// UploadSessionMetadata is a stub.
func (u *SessionChunkUploader) UploadSessionMetadata(ctx context.Context, workloadKey, runnerID string, data []byte) error {
	return fmt.Errorf("SessionChunkUploader: UploadSessionMetadata not supported on non-Linux platforms")
}

// DownloadSessionMetadata is a stub.
func (u *SessionChunkUploader) DownloadSessionMetadata(ctx context.Context, workloadKey, runnerID string) ([]byte, error) {
	return nil, fmt.Errorf("SessionChunkUploader: DownloadSessionMetadata not supported on non-Linux platforms")
}

// FullGCSPath is a stub — returns the path unchanged.
func (u *SessionChunkUploader) FullGCSPath(relativePath string) string {
	return relativePath
}
