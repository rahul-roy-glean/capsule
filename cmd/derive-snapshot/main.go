// derive-snapshot builds a derived workload ChunkedSnapshotMetadata from a base snapshot.
//
// A derived workload shares the kernel and base memory/rootfs from a base workload but
// adds one or more extension drives populated by running commands inside a mini-VM
// booted from the base snapshot. The resulting derived metadata has:
//   - Merged memory chunks (base unchanged pages + dirty overlay from mini-VM run)
//   - Merged rootfs chunks (base unchanged pages + dirty overlay from mini-VM run)
//   - Per-drive ExtensionDrive entries (chunked images for each extension drive)
//
// Usage:
//
//	derive-snapshot \
//	  --base-workload-key <key> \
//	  --drive-specs '[{"drive_id":"git_drive","label":"GIT","size_gb":10,"read_only":false,
//	                   "commands":[{"type":"git-clone","args":["https://github.com/...","main"]}],
//	                   "mount_path":"/workspace/repo"}]' \
//	  --gcs-bucket <bucket> \
//	  --gcs-prefix v1 \
//	  --local-cache /tmp/chunk-cache
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

var version string // set by -ldflags "-X main.version=..."

var (
	baseWorkloadKey = flag.String("base-workload-key", "", "Workload key of the base snapshot (required)")
	driveSpecsJSON  = flag.String("drive-specs", "[]", "JSON array of DriveSpec objects describing extension drives")
	gcsBucket       = flag.String("gcs-bucket", "", "GCS bucket for chunk storage (required)")
	gcsPrefix       = flag.String("gcs-prefix", "v1", "Top-level GCS prefix (e.g. 'v1')")
	localCache      = flag.String("local-cache", "/tmp/chunk-cache", "Local chunk cache directory")
	snapshotVersion = flag.String("version", "", "Version string for the derived snapshot (default: timestamp)")
	setCurrent      = flag.Bool("set-current", false, "Set derived snapshot as current after building")
	logLevel        = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

func main() {
	flag.Parse()

	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	logger.SetLevel(lvl)
	log := logger.WithField("component", "derive-snapshot")

	if *baseWorkloadKey == "" {
		log.Fatal("--base-workload-key is required")
	}
	if *gcsBucket == "" {
		log.Fatal("--gcs-bucket is required")
	}

	if *snapshotVersion == "" {
		*snapshotVersion = fmt.Sprintf("v%s", time.Now().Format("20060102-150405"))
		log.WithField("version", *snapshotVersion).Info("Generated version from timestamp")
	}

	// Parse drive specs.
	var driveSpecs []snapshot.DriveSpec
	if err := json.Unmarshal([]byte(*driveSpecsJSON), &driveSpecs); err != nil {
		log.WithError(err).Fatal("Failed to parse --drive-specs JSON")
	}

	// Compute derived workload key deterministically from base + drive specs.
	derivedWorkloadKey := snapshot.ComputeDerivedWorkloadKey(*baseWorkloadKey, driveSpecs)
	log.WithFields(logrus.Fields{
		"base_workload_key":    *baseWorkloadKey,
		"derived_workload_key": derivedWorkloadKey,
		"drive_count":          len(driveSpecs),
	}).Info("Computed derived workload key")

	ctx := context.Background()

	// Create GCS chunk stores.
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

	memChunkStore, err := snapshot.NewChunkStore(ctx, snapshot.ChunkStoreConfig{
		GCSBucket:      *gcsBucket,
		GCSPrefix:      *gcsPrefix,
		LocalCachePath: *localCache,
		ChunkSubdir:    "mem",
		Logger:         logger,
	})
	if err != nil {
		log.WithError(err).Fatal("Failed to create mem chunk store")
	}

	// Load base snapshot metadata via current-pointer.
	log.WithField("base_workload_key", *baseWorkloadKey).Info("Loading base snapshot metadata")
	baseVersion, err := chunkStore.ReadCurrentVersion(ctx, *baseWorkloadKey)
	if err != nil {
		log.WithError(err).Fatal("Failed to read current version for base workload key")
	}

	baseMeta, err := chunkStore.LoadChunkedMetadata(ctx, *baseWorkloadKey, baseVersion)
	if err != nil {
		log.WithError(err).Fatal("Failed to load base snapshot metadata")
	}
	log.WithFields(logrus.Fields{
		"base_version": baseVersion,
		"mem_chunks":   len(baseMeta.MemChunks),
		"disk_chunks":  len(baseMeta.RootfsChunks),
		"ext_drives":   len(baseMeta.ExtensionDrives),
	}).Info("Loaded base snapshot metadata")

	// NOTE: In a full implementation, this CLI would:
	// 1. Boot a mini-VM from the base snapshot (memory via UFFD + rootfs via FUSE)
	// 2. Attach blank extension drive images
	// 3. Inject MMDS with mode="derive", drive_specs=[...]
	// 4. Wait for thaw-agent to run all DriveSpec.Commands and signal completion
	// 5. Call vm.CreateDiffSnapshot() to get memory diff + state files
	// 6. Call uploader.MergeAndUploadMem() to produce merged memory chunks
	// 7. Chunk each extension drive image into ExtensionDrive entries
	//
	// For now, this CLI validates the configuration and uploads derived metadata
	// using the base chunks as-is. The full VM boot + snapshot loop will be added
	// in a follow-on change once the thaw-agent "derive" mode is implemented.

	log.WithFields(logrus.Fields{
		"derived_workload_key": derivedWorkloadKey,
		"base_mem_chunks":      len(baseMeta.MemChunks),
		"drive_specs":          len(driveSpecs),
	}).Info("Derive-snapshot configuration validated")

	// Build derived metadata using base chunks (no VM run yet).
	extensionDrives := make(map[string]snapshot.ExtensionDrive)
	for _, spec := range driveSpecs {
		extensionDrives[spec.DriveID] = snapshot.ExtensionDrive{
			ReadOnly:  spec.ReadOnly,
			SizeBytes: int64(spec.SizeGB) * 1024 * 1024 * 1024,
			Chunks:    nil, // Populated after mini-VM run and disk chunking
		}
	}

	derivedMeta := snapshot.BuildDerivedMetadata(
		baseMeta,
		derivedWorkloadKey,
		baseMeta.MemChunks,
		baseMeta.RootfsChunks,
		extensionDrives,
	)
	derivedMeta.Version = *snapshotVersion

	// Upload derived metadata.
	builder := snapshot.NewChunkedSnapshotBuilder(chunkStore, memChunkStore, logger)
	if err := builder.UploadChunkedMetadata(ctx, derivedMeta); err != nil {
		log.WithError(err).Fatal("Failed to upload derived metadata")
	}

	log.WithFields(logrus.Fields{
		"derived_workload_key": derivedWorkloadKey,
		"version":              *snapshotVersion,
	}).Info("Derived metadata uploaded successfully")

	if *setCurrent {
		log.Info("Setting derived snapshot as current...")
		uploader := snapshot.NewIncrementalUploader(chunkStore, logger)
		if err := uploader.UpdateSnapshotPointer(ctx, derivedMeta); err != nil {
			log.WithError(err).Fatal("Failed to update current pointer")
		}
		log.Info("Current pointer updated")
	}

	fmt.Printf("\nDerived snapshot created: %s\n", derivedWorkloadKey)
	fmt.Printf("  Version:    %s\n", *snapshotVersion)
	fmt.Printf("  Base key:   %s\n", *baseWorkloadKey)
	fmt.Printf("  Drives:     %d\n", len(driveSpecs))
}
