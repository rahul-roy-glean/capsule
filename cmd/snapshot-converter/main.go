// snapshot-converter converts traditional Firecracker snapshots to chunked format.
//
// Usage:
//
//	snapshot-converter \
//	  --source-dir=/mnt/nvme/snapshots \
//	  --gcs-bucket=my-bucket \
//	  --version=v20240101-abc123
//
// This will:
// 1. Read the traditional snapshot files (kernel.bin, rootfs.img, snapshot.mem, snapshot.state)
// 2. Split them into 4MB content-addressed chunks
// 3. Upload chunks to GCS with deduplication
// 4. Generate and upload chunked metadata
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

var (
	sourceDir       = flag.String("source-dir", "", "Directory containing traditional snapshot files")
	gcsBucket       = flag.String("gcs-bucket", "", "GCS bucket for chunk storage")
	snapshotVersion = flag.String("version", "", "Snapshot version (e.g., v20240101-abc123)")
	chunkSize       = flag.Int64("chunk-size", snapshot.DefaultChunkSize, "Chunk size in bytes (default 4MB)")
	setCurrent      = flag.Bool("set-current", false, "Set this version as current after conversion")
	logLevel        = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	localCache      = flag.String("local-cache", "/tmp/chunk-cache", "Local chunk cache directory")
	bazelVer        = flag.String("bazel-version", "", "Bazel version in the snapshot")
	repoCommit      = flag.String("repo-commit", "", "Repository commit hash in the snapshot")
	workloadKeyFlag = flag.String("workload-key", "", "Workload key for scoping GCS paths (required)")
	gcsPrefix       = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")
)

func main() {
	flag.Parse()

	// Setup logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	log := logger.WithField("component", "snapshot-converter")

	// Validate flags
	if *sourceDir == "" {
		log.Fatal("--source-dir is required")
	}
	if *gcsBucket == "" {
		log.Fatal("--gcs-bucket is required")
	}
	if *snapshotVersion == "" {
		// Generate version from timestamp
		*snapshotVersion = fmt.Sprintf("v%s", time.Now().Format("20060102-150405"))
		log.WithField("version", *snapshotVersion).Info("Generated version from timestamp")
	}
	if *workloadKeyFlag == "" {
		log.Fatal("--workload-key is required")
	}

	// Verify source files exist
	requiredFiles := []string{"kernel.bin", "rootfs.img"}
	optionalFiles := []string{"snapshot.mem", "snapshot.state", "repo-cache-seed.img"}

	for _, f := range requiredFiles {
		path := filepath.Join(*sourceDir, f)
		if _, err := os.Stat(path); err != nil {
			log.WithField("file", path).Fatal("Required snapshot file not found")
		}
	}

	// Check for optional files
	hasMemSnapshot := true
	for _, f := range optionalFiles[:2] { // mem and state
		path := filepath.Join(*sourceDir, f)
		if _, err := os.Stat(path); err != nil {
			log.WithField("file", path).Warn("Optional snapshot file not found")
			hasMemSnapshot = false
		}
	}

	if !hasMemSnapshot {
		log.Warn("Memory snapshot files not found - will create disk-only chunked snapshot")
	}

	log.WithFields(logrus.Fields{
		"source_dir": *sourceDir,
		"gcs_bucket": *gcsBucket,
		"version":    *snapshotVersion,
		"chunk_size": *chunkSize,
	}).Info("Starting snapshot conversion")

	ctx := context.Background()
	startTime := time.Now()

	// Create chunk store
	chunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
		GCSBucket:      *gcsBucket,
		GCSPrefix:      *gcsPrefix,
		LocalCachePath: *localCache,
		ChunkSubdir:    "disk",
		Logger:         logger,
	})
	if err != nil {
		log.WithError(err).Fatal("Failed to create chunk store")
	}
	defer chunkStore.Close()

	// Build snapshot paths
	paths := &snapshot.SnapshotPaths{
		Kernel:  filepath.Join(*sourceDir, "kernel.bin"),
		Rootfs:  filepath.Join(*sourceDir, "rootfs.img"),
		Version: *snapshotVersion,
	}

	if hasMemSnapshot {
		paths.Mem = filepath.Join(*sourceDir, "snapshot.mem")
		paths.State = filepath.Join(*sourceDir, "snapshot.state")
	}

	// Check for repo cache seed
	repoCachePath := filepath.Join(*sourceDir, "repo-cache-seed.img")
	if _, err := os.Stat(repoCachePath); err == nil {
		paths.RepoCacheSeed = repoCachePath
	}

	// Create chunked snapshot builder
	builder := snapshot.NewChunkedSnapshotBuilder(chunkStore, nil, logger)

	// Build chunked snapshot
	log.Info("Building chunked snapshot (this may take a while for large snapshots)...")
	meta, err := builder.BuildChunkedSnapshot(ctx, paths, *snapshotVersion, *workloadKeyFlag)
	if err != nil {
		log.WithError(err).Fatal("Failed to build chunked snapshot")
	}

	// Set additional metadata
	meta.BazelVersion = *bazelVer
	meta.RepoCommit = *repoCommit

	// Upload metadata
	log.Info("Uploading chunked metadata...")
	if err := builder.UploadChunkedMetadata(ctx, meta); err != nil {
		log.WithError(err).Fatal("Failed to upload chunked metadata")
	}

	// Optionally set as current
	if *setCurrent {
		log.Info("Setting as current snapshot...")
		uploader := snapshot.NewIncrementalUploader(chunkStore, logger)
		if err := uploader.UpdateSnapshotPointer(ctx, meta); err != nil {
			log.WithError(err).Fatal("Failed to update current pointer")
		}
	}

	duration := time.Since(startTime)

	// Calculate statistics
	totalChunks := len(meta.MemChunks) + len(meta.RootfsChunks) + len(meta.RepoCacheSeedChunks) + 2 // +2 for kernel and state
	var totalCompressed int64
	for _, c := range meta.MemChunks {
		totalCompressed += c.CompressedSize
	}
	for _, c := range meta.RootfsChunks {
		totalCompressed += c.CompressedSize
	}

	totalOriginal := meta.TotalMemSize + meta.TotalDiskSize
	compressionRatio := 1.0
	if totalOriginal > 0 {
		compressionRatio = float64(totalCompressed) / float64(totalOriginal)
	}

	log.WithFields(logrus.Fields{
		"version":           meta.Version,
		"duration":          duration,
		"total_chunks":      totalChunks,
		"mem_chunks":        len(meta.MemChunks),
		"disk_chunks":       len(meta.RootfsChunks),
		"total_mem_size":    formatSize(meta.TotalMemSize),
		"total_disk_size":   formatSize(meta.TotalDiskSize),
		"compressed_size":   formatSize(totalCompressed),
		"compression_ratio": fmt.Sprintf("%.1f%%", compressionRatio*100),
		"set_current":       *setCurrent,
	}).Info("Snapshot conversion completed successfully")

	fmt.Printf("\nChunked snapshot created: %s\n", meta.Version)
	fmt.Printf("  Memory chunks: %d (%.2f GB original)\n", len(meta.MemChunks), float64(meta.TotalMemSize)/(1024*1024*1024))
	fmt.Printf("  Disk chunks:   %d (%.2f GB original)\n", len(meta.RootfsChunks), float64(meta.TotalDiskSize)/(1024*1024*1024))
	fmt.Printf("  Compression:   %.1f%%\n", compressionRatio*100)
	fmt.Printf("  Duration:      %s\n", duration.Round(time.Second))
	if *setCurrent {
		fmt.Printf("  Status:        Set as current\n")
	}
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
