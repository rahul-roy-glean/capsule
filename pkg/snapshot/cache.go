package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
)

// SnapshotPaths holds the local paths to snapshot files
type SnapshotPaths struct {
	Kernel  string
	Rootfs  string
	Mem     string
	State   string
	Version string
	// ExtensionDriveImages maps DriveID to the local path of the extension drive image.
	// Used by BuildChunkedSnapshot to chunk extension drives.
	ExtensionDriveImages map[string]string
}

// Cache manages local snapshot cache with GCS sync
type Cache struct {
	localPath   string
	gcsBucket   string
	gcsPrefix   string // top-level prefix for all GCS paths (e.g. "v1")
	workloadKey string
	gcsClient   *storage.Client
	mu          sync.RWMutex
	logger      *logrus.Entry
}

// CacheConfig holds configuration for snapshot cache
type CacheConfig struct {
	LocalPath   string
	GCSBucket   string
	GCSPrefix   string // Top-level prefix for all GCS paths (e.g. "v1"); empty means no prefix
	WorkloadKey string
	Logger      *logrus.Logger
}

// NewCache creates a new snapshot cache manager
func NewCache(ctx context.Context, cfg CacheConfig) (*Cache, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = logrus.New()
	}

	// GCS client is only needed when a real bucket is configured.
	// Skip creation for local-only dev setups to avoid requiring GCP credentials.
	var client *storage.Client
	if cfg.GCSBucket != "" {
		var err error
		client, err = storage.NewClient(ctx)
		if err != nil {
			logger.WithError(err).Warn("Failed to create GCS client, snapshot sync from GCS will be unavailable")
		}
	}

	cache := &Cache{
		localPath:   cfg.LocalPath,
		gcsBucket:   cfg.GCSBucket,
		gcsPrefix:   cfg.GCSPrefix,
		workloadKey: cfg.WorkloadKey,
		gcsClient:   client,
		logger:      logger.WithField("component", "snapshot-cache"),
	}

	// Ensure local path exists
	if err := os.MkdirAll(cfg.LocalPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create local cache directory: %w", err)
	}

	return cache, nil
}

// gcsPath prepends the configured GCS prefix to a path.
func (c *Cache) gcsPath(path string) string {
	if c.gcsPrefix != "" {
		return c.gcsPrefix + "/" + path
	}
	return path
}

// GetKernelPath returns the path to kernel.bin, verifying it exists.
// Use this when only the kernel is needed (e.g. GCS-backed resume where
// rootfs is provided via FUSE).
func (c *Cache) GetKernelPath() (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	kernelPath := filepath.Join(c.localPath, "kernel.bin")
	if _, err := os.Stat(kernelPath); err != nil {
		return "", fmt.Errorf("required snapshot file not found: %s", kernelPath)
	}
	return kernelPath, nil
}

// GetSnapshotPaths returns the paths to snapshot files.
// Kernel and rootfs are required; mem/state are optional.
func (c *Cache) GetSnapshotPaths() (*SnapshotPaths, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	kernelPath := filepath.Join(c.localPath, "kernel.bin")
	rootfsPath := filepath.Join(c.localPath, "rootfs.img")
	memPath := filepath.Join(c.localPath, "snapshot.mem")
	statePath := filepath.Join(c.localPath, "snapshot.state")

	// Required files for any boot mode
	for _, path := range []string{kernelPath, rootfsPath} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("required snapshot file not found: %s", path)
		}
	}

	// Snapshot files are optional - if missing, caller will use fresh boot
	paths := &SnapshotPaths{
		Kernel: kernelPath,
		Rootfs: rootfsPath,
	}

	// Only include mem/state if BOTH exist (partial snapshot is invalid)
	if _, err := os.Stat(memPath); err == nil {
		if _, err := os.Stat(statePath); err == nil {
			paths.Mem = memPath
			paths.State = statePath
		}
	}

	// Scan for extension drive images: any .img file that isn't a known
	// infrastructure file.
	paths.ExtensionDriveImages = make(map[string]string)
	knownFiles := map[string]bool{
		"kernel.bin":            true,
		"rootfs.img":            true,
		"snapshot.mem":          true,
		"snapshot.state":        true,
		"metadata.json":         true,
		"chunked-metadata.json": true,
	}
	entries, _ := os.ReadDir(c.localPath)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".img") || knownFiles[e.Name()] {
			continue
		}
		driveID := strings.TrimSuffix(e.Name(), ".img")
		driveID = strings.ReplaceAll(driveID, "-", "_")
		paths.ExtensionDriveImages[driveID] = filepath.Join(c.localPath, e.Name())
	}

	return paths, nil
}

// Close closes the cache and releases resources
func (c *Cache) Close() error {
	if c.gcsClient != nil {
		return c.gcsClient.Close()
	}
	return nil
}
