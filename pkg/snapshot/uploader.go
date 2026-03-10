package snapshot

import (
	"context"
	"encoding/json"
	"fmt"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
)

// Uploader handles uploading snapshots to GCS
type Uploader struct {
	gcsBucket string
	gcsPrefix string // top-level prefix for all GCS paths (e.g. "v1")
	gcsClient *storage.Client
	logger    *logrus.Entry
}

// UploaderConfig holds configuration for snapshot uploader
type UploaderConfig struct {
	GCSBucket string
	GCSPrefix string // Top-level prefix for all GCS paths (e.g. "v1"); empty means no prefix
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
		gcsPrefix: cfg.GCSPrefix,
		gcsClient: client,
		logger:    logger.WithField("component", "snapshot-uploader"),
	}, nil
}

// gcsPath prepends the configured GCS prefix to a path.
func (u *Uploader) gcsPath(path string) string {
	if u.gcsPrefix != "" {
		return u.gcsPrefix + "/" + path
	}
	return path
}

// UpdateCurrentPointerForRepo updates the "current" pointer for a specific workload key.
func (u *Uploader) UpdateCurrentPointerForRepo(ctx context.Context, version, workloadKey string) error {
	u.logger.WithFields(logrus.Fields{
		"version":      version,
		"workload_key": workloadKey,
	}).Info("Updating current pointer")

	pointer := struct {
		Version string `json:"version"`
	}{Version: version}

	pointerJSON, err := json.Marshal(pointer)
	if err != nil {
		return fmt.Errorf("failed to marshal pointer: %w", err)
	}

	pointerPath := u.gcsPath(workloadKey + "/current-pointer.json")

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
	it := bucket.Objects(ctx, &storage.Query{Prefix: u.gcsPath(version) + "/"})
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
