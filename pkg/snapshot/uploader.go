package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
)

// Uploader handles uploading snapshots to GCS
type Uploader struct {
	gcsBucket string
	gcsClient *storage.Client
	logger    *logrus.Entry
}

// UploaderConfig holds configuration for snapshot uploader
type UploaderConfig struct {
	GCSBucket string
	Logger    *logrus.Logger
}

// NewUploader creates a new snapshot uploader
func NewUploader(ctx context.Context, cfg UploaderConfig) (*Uploader, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}

	return &Uploader{
		gcsBucket: cfg.GCSBucket,
		gcsClient: client,
		logger:    logger.WithField("component", "snapshot-uploader"),
	}, nil
}

// UploadSnapshot uploads a snapshot to GCS using parallel gcloud storage cp calls.
// Paths are namespaced under the chunk key from metadata.ChunkKey.
func (u *Uploader) UploadSnapshot(ctx context.Context, localDir string, metadata SnapshotMetadata) error {
	version := metadata.Version
	prefix := metadata.ChunkKey + "/" + version
	u.logger.WithFields(logrus.Fields{
		"version":    version,
		"gcs_prefix": prefix,
	}).Info("Uploading snapshot to GCS")

	start := time.Now()

	// Files to upload (namespaced under repo slug if set)
	files := []struct {
		local  string
		remote string
	}{
		{filepath.Join(localDir, "kernel.bin"), fmt.Sprintf("%s/kernel.bin", prefix)},
		{filepath.Join(localDir, "rootfs.img"), fmt.Sprintf("%s/rootfs.img", prefix)},
		{filepath.Join(localDir, "snapshot.mem"), fmt.Sprintf("%s/snapshot.mem", prefix)},
		{filepath.Join(localDir, "snapshot.state"), fmt.Sprintf("%s/snapshot.state", prefix)},
		{filepath.Join(localDir, "repo-cache-seed.img"), fmt.Sprintf("%s/repo-cache-seed.img", prefix)},
	}

	// Calculate total size
	var totalSize int64
	for _, f := range files {
		info, err := os.Stat(f.local)
		if err == nil {
			totalSize += info.Size()
		}
	}
	metadata.SizeBytes = totalSize

	// Upload all files concurrently
	type uploadResult struct {
		file string
		err  error
	}
	ch := make(chan uploadResult, len(files))
	for _, f := range files {
		f := f // capture loop variable
		go func() {
			ch <- uploadResult{f.local, u.uploadFile(ctx, f.local, f.remote)}
		}()
	}

	var uploadErrors []string
	for range files {
		r := <-ch
		if r.err != nil {
			uploadErrors = append(uploadErrors, fmt.Sprintf("%s: %v", r.file, r.err))
		}
	}
	if len(uploadErrors) > 0 {
		return fmt.Errorf("failed to upload files: %s", joinErrors(uploadErrors))
	}

	// Upload metadata (small file, use Go client directly)
	metadataJSON, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	metadataObj := bucket.Object(fmt.Sprintf("%s/metadata.json", prefix))
	writer := metadataObj.NewWriter(ctx)
	writer.ContentType = "application/json"
	if _, err := writer.Write(metadataJSON); err != nil {
		writer.Close()
		return fmt.Errorf("failed to write metadata: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close metadata writer: %w", err)
	}

	duration := time.Since(start)
	u.logger.WithFields(logrus.Fields{
		"version":    version,
		"duration":   duration,
		"size_bytes": totalSize,
	}).Info("Snapshot uploaded successfully")

	return nil
}

func joinErrors(errs []string) string {
	result := ""
	for i, e := range errs {
		if i > 0 {
			result += "; "
		}
		result += e
	}
	return result
}

// uploadFile uploads a single file to GCS using gcloud storage cp with parallel composite upload
func (u *Uploader) uploadFile(ctx context.Context, localPath, remotePath string) error {
	gcsURI := fmt.Sprintf("gs://%s/%s", u.gcsBucket, remotePath)
	u.logger.WithFields(logrus.Fields{
		"local":  localPath,
		"remote": gcsURI,
	}).Info("Uploading file via gcloud storage")

	cmd := exec.CommandContext(ctx, "gcloud", "storage", "cp", localPath, gcsURI)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcloud storage cp failed for %s: %w", filepath.Base(localPath), err)
	}

	return nil
}

// UpdateCurrentPointerForRepo updates the "current" pointer for a specific chunk key.
func (u *Uploader) UpdateCurrentPointerForRepo(ctx context.Context, version, chunkKey string) error {
	u.logger.WithFields(logrus.Fields{
		"version":   version,
		"chunk_key": chunkKey,
	}).Info("Updating current pointer")

	pointer := struct {
		Version string `json:"version"`
	}{Version: version}

	pointerJSON, err := json.Marshal(pointer)
	if err != nil {
		return fmt.Errorf("failed to marshal pointer: %w", err)
	}

	pointerPath := chunkKey + "/current-pointer.json"

	bucket := u.gcsClient.Bucket(u.gcsBucket)
	obj := bucket.Object(pointerPath)
	writer := obj.NewWriter(ctx)
	writer.ContentType = "application/json"
	if _, err := writer.Write(pointerJSON); err != nil {
		writer.Close()
		return fmt.Errorf("failed to write pointer: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close pointer writer: %w", err)
	}

	u.logger.Info("Current pointer updated successfully")
	return nil
}

// DeleteVersion deletes a snapshot version from GCS
func (u *Uploader) DeleteVersion(ctx context.Context, version string) error {
	u.logger.WithField("version", version).Info("Deleting snapshot version")

	bucket := u.gcsClient.Bucket(u.gcsBucket)

	// List and delete all objects with this prefix
	it := bucket.Objects(ctx, &storage.Query{Prefix: version + "/"})
	for {
		attrs, err := it.Next()
		if err != nil {
			break
		}
		if err := bucket.Object(attrs.Name).Delete(ctx); err != nil {
			u.logger.WithError(err).Warnf("Failed to delete %s", attrs.Name)
		}
	}

	return nil
}

// Close closes the uploader
func (u *Uploader) Close() error {
	if u.gcsClient != nil {
		return u.gcsClient.Close()
	}
	return nil
}
