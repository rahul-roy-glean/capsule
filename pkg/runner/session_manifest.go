package runner

import (
	"time"

	"github.com/google/uuid"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

type sessionManifestBuildResult struct {
	manifest         *snapshot.SnapshotManifest
	diskIndexesToPut map[string]*snapshot.ChunkIndex
	diskIndexObjects map[string]string
	manifestPath     string
	memIndexPath     string
}

func buildSessionManifest(
	uploader *snapshot.SessionChunkUploader,
	gcsBase string,
	runner *Runner,
	generation int,
	vmStateGCSPath string,
	memIndex *snapshot.ChunkIndex,
	prevManifest *snapshot.SnapshotManifest,
	newExtDiskIndexes map[string]*snapshot.ChunkIndex,
	newRootfsDiskIndex *snapshot.ChunkIndex,
	goldenMeta *snapshot.ChunkedSnapshotMetadata,
) *sessionManifestBuildResult {
	man := &snapshot.SnapshotManifest{
		Version:     "1",
		SnapshotID:  uuid.New().String(),
		CreatedAt:   time.Now(),
		WorkloadKey: runner.WorkloadKey,
		Runtime: &snapshot.SessionRuntime{
			SessionID:       runner.SessionID,
			Generation:      generation,
			RunnerID:        runner.ID,
			VCPUs:           runner.Resources.VCPUs,
			MemoryMB:        runner.Resources.MemoryMB,
			ServicePort:     runner.ServicePort,
			SnapshotVersion: runner.SnapshotVersion,
			CreatedAt:       runner.CreatedAt,
			AuthConfig:      runner.AuthConfig,
		},
	}
	man.Firecracker.VMStateObject = vmStateGCSPath
	man.Memory.Mode = "chunked"
	man.Memory.TotalSizeBytes = memIndex.Region.LogicalSizeBytes
	man.Memory.ChunkIndexObject = uploader.FullGCSPath(gcsBase + "/chunked-metadata.json")
	man.Integrity.Algo = "sha256"

	result := &sessionManifestBuildResult{
		manifest:         man,
		diskIndexesToPut: make(map[string]*snapshot.ChunkIndex),
		diskIndexObjects: make(map[string]string),
		manifestPath:     uploader.FullGCSPath(gcsBase + "/snapshot_manifest.json"),
		memIndexPath:     uploader.FullGCSPath(gcsBase + "/chunked-metadata.json"),
	}

	if prevManifest != nil && prevManifest.Disk.ChunkIndexObject != "" {
		man.Disk = prevManifest.Disk
		result.diskIndexObjects["__rootfs__"] = prevManifest.Disk.ChunkIndexObject
	}
	if newRootfsDiskIndex != nil {
		rootfsPath := uploader.FullGCSPath(gcsBase + "/__rootfs__-disk.json")
		man.Disk = snapshot.DiskSection{
			Mode:             "chunked",
			TotalSizeBytes:   newRootfsDiskIndex.Region.LogicalSizeBytes,
			ChunkIndexObject: rootfsPath,
		}
		result.diskIndexesToPut["__rootfs__"] = newRootfsDiskIndex
		result.diskIndexObjects["__rootfs__"] = rootfsPath
	} else if man.Disk.ChunkIndexObject == "" && goldenMeta != nil && len(goldenMeta.RootfsChunks) > 0 {
		rootfsIdx := buildRootfsDriveBaseIndex(goldenMeta)
		rootfsPath := uploader.FullGCSPath(gcsBase + "/__rootfs__-disk.json")
		man.Disk = snapshot.DiskSection{
			Mode:             "chunked",
			TotalSizeBytes:   rootfsIdx.Region.LogicalSizeBytes,
			ChunkIndexObject: rootfsPath,
		}
		result.diskIndexesToPut["__rootfs__"] = rootfsIdx
		result.diskIndexObjects["__rootfs__"] = rootfsPath
	}

	if prevManifest != nil && len(prevManifest.ExtensionDisks) > 0 {
		man.ExtensionDisks = make(map[string]snapshot.DiskSection, len(prevManifest.ExtensionDisks))
		for driveID, section := range prevManifest.ExtensionDisks {
			man.ExtensionDisks[driveID] = section
			if section.ChunkIndexObject != "" {
				result.diskIndexObjects[driveID] = section.ChunkIndexObject
			}
		}
	}

	for driveID, diskIdx := range newExtDiskIndexes {
		if man.ExtensionDisks == nil {
			man.ExtensionDisks = make(map[string]snapshot.DiskSection)
		}
		diskPath := uploader.FullGCSPath(gcsBase + "/" + driveID + "-disk.json")
		man.ExtensionDisks[driveID] = snapshot.DiskSection{
			Mode:             "chunked",
			TotalSizeBytes:   diskIdx.Region.LogicalSizeBytes,
			ChunkIndexObject: diskPath,
		}
		result.diskIndexesToPut[driveID] = diskIdx
		result.diskIndexObjects[driveID] = diskPath
	}

	if goldenMeta != nil {
		for driveID, extDrive := range goldenMeta.ExtensionDrives {
			if len(extDrive.Chunks) == 0 {
				continue
			}
			if _, exists := man.ExtensionDisks[driveID]; exists {
				continue
			}
			diskIdx := buildExtensionDriveBaseIndex(goldenMeta, driveID)
			diskPath := uploader.FullGCSPath(gcsBase + "/" + driveID + "-disk.json")
			if man.ExtensionDisks == nil {
				man.ExtensionDisks = make(map[string]snapshot.DiskSection)
			}
			man.ExtensionDisks[driveID] = snapshot.DiskSection{
				Mode:             "chunked",
				TotalSizeBytes:   diskIdx.Region.LogicalSizeBytes,
				ChunkIndexObject: diskPath,
			}
			result.diskIndexesToPut[driveID] = diskIdx
			result.diskIndexObjects[driveID] = diskPath
		}
	}

	if len(man.ExtensionDisks) == 0 {
		man.ExtensionDisks = nil
	}
	if len(result.diskIndexObjects) == 0 {
		result.diskIndexObjects = nil
	}
	return result
}
