package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
)

// SnapshotMetadata holds metadata about a snapshot
type SnapshotMetadata struct {
	Version      string            `json:"version"`
	BazelVersion string            `json:"bazel_version"`
	RepoCommit   string            `json:"repo_commit"`
	Repo         string            `json:"repo,omitempty"`
	WorkloadKey  string            `json:"workload_key,omitempty"`
	Commands     []SnapshotCommand `json:"commands,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	SizeBytes    int64             `json:"size_bytes"`
	KernelPath   string            `json:"kernel_path"`
	RootfsPath   string            `json:"rootfs_path"`
	MemPath      string            `json:"mem_path"`
	StatePath    string            `json:"state_path"`
	// RepoCacheSeedPath is a path (relative to the snapshot version dir) to the
	// shared Bazel repository cache seed disk image (ext4).
	RepoCacheSeedPath string `json:"repo_cache_seed_path,omitempty"`
}

// SnapshotPaths holds the local paths to snapshot files
type SnapshotPaths struct {
	Kernel        string
	Rootfs        string
	Mem           string
	State         string
	RepoCacheSeed string
	Version       string
}

// Cache manages local snapshot cache with GCS sync
type Cache struct {
	localPath   string
	gcsBucket   string
	gcsPrefix   string // top-level prefix for all GCS paths (e.g. "v1")
	workloadKey string
	gcsClient   *storage.Client
	currentVer  string
	metadata    *SnapshotMetadata
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

	// Load current metadata if exists
	cache.loadLocalMetadata()

	return cache, nil
}

// gcsPath prepends the configured GCS prefix to a path.
func (c *Cache) gcsPath(path string) string {
	if c.gcsPrefix != "" {
		return c.gcsPrefix + "/" + path
	}
	return path
}

// SyncFromGCS syncs snapshot files from GCS to local cache
func (c *Cache) SyncFromGCS(ctx context.Context, version string) error {
	if c.gcsClient == nil {
		c.logger.Warn("GCS client not available, skipping snapshot sync")
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if version == "" {
		resolved, err := c.resolveCurrentPointerForRepo(ctx, c.workloadKey)
		if err != nil || resolved == "" {
			return fmt.Errorf("failed to resolve current version for workload key %q: %w", c.workloadKey, err)
		}
		c.logger.WithField("resolved_version", resolved).Info("Resolved current pointer to versioned directory")
		version = resolved
	}

	c.logger.WithField("version", version).Info("Syncing snapshot from GCS")

	start := time.Now()

	gcsPath := fmt.Sprintf("gs://%s/%s/", c.gcsBucket, c.gcsPath(version))
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "rsync", "-r", gcsPath, c.localPath+"/")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gcloud storage rsync failed: %w", err)
	}

	duration := time.Since(start)
	c.logger.WithFields(logrus.Fields{
		"version":  version,
		"duration": duration,
	}).Info("Snapshot sync completed")

	// Load metadata
	if err := c.loadLocalMetadata(); err != nil {
		c.logger.WithError(err).Warn("Failed to load metadata after sync")
	}

	return nil
}

// resolveCurrentPointerForRepo reads the workload-key-scoped current-pointer.json from GCS.
func (c *Cache) resolveCurrentPointerForRepo(ctx context.Context, workloadKey string) (string, error) {
	pointerPath := c.gcsPath(workloadKey + "/current-pointer.json")
	bucket := c.gcsClient.Bucket(c.gcsBucket)
	obj := bucket.Object(pointerPath)

	reader, err := obj.NewReader(ctx)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}

	var pointer struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pointer); err != nil {
		return "", err
	}

	return pointer.Version, nil
}

// loadLocalMetadata loads metadata from local cache
func (c *Cache) loadLocalMetadata() error {
	metadataPath := filepath.Join(c.localPath, "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var metadata SnapshotMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return err
	}

	c.metadata = &metadata
	c.currentVer = metadata.Version
	return nil
}

// GetSnapshotPaths returns the paths to snapshot files.
// Kernel, rootfs, and repo-cache-seed are required; mem/state are optional (fresh boot if missing).
func (c *Cache) GetSnapshotPaths() (*SnapshotPaths, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	kernelPath := filepath.Join(c.localPath, "kernel.bin")
	rootfsPath := filepath.Join(c.localPath, "rootfs.img")
	memPath := filepath.Join(c.localPath, "snapshot.mem")
	statePath := filepath.Join(c.localPath, "snapshot.state")
	repoCacheSeedPath := filepath.Join(c.localPath, "repo-cache-seed.img")

	// Required files for any boot mode
	for _, path := range []string{kernelPath, rootfsPath, repoCacheSeedPath} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("required snapshot file not found: %s", path)
		}
	}

	// Snapshot files are optional - if missing, caller will use fresh boot
	paths := &SnapshotPaths{
		Kernel:        kernelPath,
		Rootfs:        rootfsPath,
		RepoCacheSeed: repoCacheSeedPath,
		Version:       c.currentVer,
	}

	// Only include mem/state if BOTH exist (partial snapshot is invalid)
	if _, err := os.Stat(memPath); err == nil {
		if _, err := os.Stat(statePath); err == nil {
			paths.Mem = memPath
			paths.State = statePath
		}
	}

	return paths, nil
}

// GetMetadata returns the current snapshot metadata
func (c *Cache) GetMetadata() *SnapshotMetadata {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metadata
}

// CurrentVersion returns the current snapshot version
func (c *Cache) CurrentVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentVer
}

// ListVersions lists available snapshot versions in GCS
func (c *Cache) ListVersions(ctx context.Context) ([]string, error) {
	if c.gcsClient == nil {
		return nil, fmt.Errorf("GCS client not available")
	}

	c.logger.Debug("Listing snapshot versions from GCS")

	bucket := c.gcsClient.Bucket(c.gcsBucket)
	it := bucket.Objects(ctx, &storage.Query{
		Prefix:    "",
		Delimiter: "/",
	})

	var versions []string
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		if attrs.Prefix != "" {
			// This is a "directory" (prefix)
			version := filepath.Base(attrs.Prefix)
			if version != "" {
				versions = append(versions, version)
			}
		}
	}

	return versions, nil
}

// GetRemoteMetadata fetches metadata for a specific version from GCS
func (c *Cache) GetRemoteMetadata(ctx context.Context, version string) (*SnapshotMetadata, error) {
	if c.gcsClient == nil {
		return nil, fmt.Errorf("GCS client not available")
	}

	bucket := c.gcsClient.Bucket(c.gcsBucket)
	obj := bucket.Object(c.gcsPath(fmt.Sprintf("%s/metadata.json", version)))

	reader, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata content: %w", err)
	}

	var metadata SnapshotMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &metadata, nil
}

// IsStale checks if the local cache is stale compared to GCS
func (c *Cache) IsStale(ctx context.Context) (bool, error) {
	if c.gcsClient == nil {
		return false, nil
	}

	c.mu.RLock()
	localVer := c.currentVer
	c.mu.RUnlock()

	remoteVer, err := c.resolveCurrentPointerForRepo(ctx, c.workloadKey)
	if err != nil {
		return false, fmt.Errorf("failed to resolve current version: %w", err)
	}

	return localVer != remoteVer, nil
}

// CreateOverlay creates a copy-on-write overlay of the rootfs
func (c *Cache) CreateOverlay(runnerID string) (string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	baseRootfs := filepath.Join(c.localPath, "rootfs.img")
	overlayDir := filepath.Join(c.localPath, "overlays")
	overlayPath := filepath.Join(overlayDir, fmt.Sprintf("rootfs-%s.img", runnerID))

	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create overlay directory: %w", err)
	}

	// Firecracker expects raw block images. Use reflink (instant, CoW clone) if
	// supported by the filesystem, otherwise fall back to a sparse copy. Try
	// --reflink=always first so we get an explicit error if reflink fails rather
	// than silently falling back to a full data copy.
	start := time.Now()
	if output, err := exec.Command("cp", "--reflink=always", baseRootfs, overlayPath).CombinedOutput(); err != nil {
		c.logger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"error":     strings.TrimSpace(string(output)),
		}).Warn("Reflink copy failed, falling back to sparse copy")
		if output2, err2 := exec.Command("cp", "--sparse=always", baseRootfs, overlayPath).CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("failed to copy rootfs overlay: %s / %s: %w", string(output), string(output2), err2)
		}
	}
	elapsed := time.Since(start)

	c.logger.WithFields(logrus.Fields{
		"runner_id":   runnerID,
		"overlay":     overlayPath,
		"duration_ms": elapsed.Milliseconds(),
	}).Info("Created rootfs overlay")

	return overlayPath, nil
}

// RemoveOverlay removes a rootfs overlay
func (c *Cache) RemoveOverlay(runnerID string) error {
	overlayPath := filepath.Join(c.localPath, "overlays", fmt.Sprintf("rootfs-%s.img", runnerID))
	if err := os.Remove(overlayPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove overlay: %w", err)
	}
	return nil
}

// GetCacheSize returns the total size of the local cache
func (c *Cache) GetCacheSize() (int64, error) {
	var size int64
	err := filepath.Walk(c.localPath, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

// Close closes the cache and releases resources
func (c *Cache) Close() error {
	if c.gcsClient != nil {
		return c.gcsClient.Close()
	}
	return nil
}
