package runner

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

func TestBuildSessionManifestCarriesForwardPreviousSessionHead(t *testing.T) {
	uploader := snapshot.NewSessionChunkUploader(nil, nil, logrus.New())
	prevManifest := &snapshot.SnapshotManifest{
		Version:     "1",
		SnapshotID:  "prev",
		CreatedAt:   time.Now().UTC(),
		WorkloadKey: "wk123",
	}
	prevManifest.Disk = snapshot.DiskSection{
		Mode:             "chunked",
		TotalSizeBytes:   64,
		ChunkIndexObject: "v1/sessions/sess-1/checkpoints/000001/__rootfs__-disk.json",
	}
	prevManifest.ExtensionDisks = map[string]snapshot.DiskSection{
		"git_drive": {
			Mode:             "chunked",
			TotalSizeBytes:   128,
			ChunkIndexObject: "v1/sessions/sess-1/checkpoints/000001/git_drive-disk.json",
		},
	}

	memIdx := &snapshot.ChunkIndex{ChunkSizeBytes: snapshot.DefaultChunkSize}
	memIdx.Region.LogicalSizeBytes = 1024
	newExtDisk := buildExtensionDriveBaseIndex(&snapshot.ChunkedSnapshotMetadata{
		ChunkSize: snapshot.DefaultChunkSize,
		ExtensionDrives: map[string]snapshot.ExtensionDrive{
			"bazel_cache": {SizeBytes: 256},
		},
	}, "bazel_cache")

	result := buildSessionManifest(
		uploader,
		sessionCheckpointGCSBase("sess-1", 2),
		&Runner{
			ID:              "runner-1",
			SessionID:       "sess-1",
			WorkloadKey:     "wk123",
			SnapshotVersion: "snap-v2",
			Resources:       Resources{VCPUs: 2, MemoryMB: 4096},
			ServicePort:     8080,
			CreatedAt:       time.Now().UTC(),
		},
		2,
		"v1/sessions/sess-1/checkpoints/000002/snapshot.state",
		memIdx,
		prevManifest,
		map[string]*snapshot.ChunkIndex{"bazel_cache": newExtDisk},
		nil,
		nil,
	)

	if result.manifest.Runtime == nil {
		t.Fatal("runtime should be populated on manifest")
	}
	if result.manifest.Runtime.Generation != 2 {
		t.Fatalf("generation = %d, want 2", result.manifest.Runtime.Generation)
	}
	if got := result.manifest.ExtensionDisks["git_drive"].ChunkIndexObject; got != prevManifest.ExtensionDisks["git_drive"].ChunkIndexObject {
		t.Fatalf("git_drive should carry forward previous chunk index object, got %q", got)
	}
	if got := result.manifest.ExtensionDisks["bazel_cache"].ChunkIndexObject; got == "" {
		t.Fatal("dirty bazel_cache drive should get a chunk index object")
	}
	if got := result.manifest.Disk.ChunkIndexObject; got != prevManifest.Disk.ChunkIndexObject {
		t.Fatalf("rootfs should carry forward previous chunk index object, got %q", got)
	}
	if _, ok := result.diskIndexesToPut["git_drive"]; ok {
		t.Fatal("clean carried-forward drive should not be rewritten in the new generation")
	}
	if _, ok := result.diskIndexesToPut["bazel_cache"]; !ok {
		t.Fatal("dirty drive should be uploaded for the new generation")
	}
}
