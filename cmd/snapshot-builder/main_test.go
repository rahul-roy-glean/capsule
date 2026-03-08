package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

func TestValidateBuildModeAllowsColdBootWithoutChunked(t *testing.T) {
	if err := validateBuildMode(false, false); err != nil {
		t.Fatalf("validateBuildMode(false, false) returned error: %v", err)
	}
}

func TestValidateBuildModeRejectsIncrementalWithoutChunked(t *testing.T) {
	err := validateBuildMode(true, false)
	if err == nil {
		t.Fatal("validateBuildMode(true, false) returned nil, want error")
	}
}

func TestShouldPublishCurrentPointerRejectsMissingUpload(t *testing.T) {
	err := shouldPublishCurrentPointer(false)
	if err == nil {
		t.Fatal("shouldPublishCurrentPointer(false) returned nil, want error")
	}
}

func TestShouldPublishCurrentPointerAllowsUploadedSnapshot(t *testing.T) {
	if err := shouldPublishCurrentPointer(true); err != nil {
		t.Fatalf("shouldPublishCurrentPointer(true) returned error: %v", err)
	}
}

func TestResolveSnapshotLookupKeyPrefersLayerHash(t *testing.T) {
	commands := []snapshot.SnapshotCommand{{Type: "shell", Args: []string{"echo", "hello"}}}
	workloadKey := snapshot.ComputeWorkloadKey(commands)
	layerHash := "layer-123"

	got := resolveSnapshotLookupKey(workloadKey, layerHash)
	if got != layerHash {
		t.Fatalf("resolveSnapshotLookupKey(%q, %q) = %q, want %q", workloadKey, layerHash, got, layerHash)
	}
}

func TestResolveSnapshotLookupKeyFallsBackToWorkloadKey(t *testing.T) {
	commands := []snapshot.SnapshotCommand{{Type: "shell", Args: []string{"echo", "hello"}}}
	workloadKey := snapshot.ComputeWorkloadKey(commands)

	got := resolveSnapshotLookupKey(workloadKey, "")
	if got != workloadKey {
		t.Fatalf("resolveSnapshotLookupKey(%q, \"\") = %q, want %q", workloadKey, got, workloadKey)
	}
}

func TestComputeRootfsSourceHashLegacyIncludesResizeSetting(t *testing.T) {
	rootfsPath := writeTestFile(t, "rootfs.img", []byte("rootfs-data"))

	hashA, err := computeRootfsSourceHash(rootfsPath, "", "runner", "", 0)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash legacy hashA failed: %v", err)
	}
	hashB, err := computeRootfsSourceHash(rootfsPath, "", "runner", "", 10)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash legacy hashB failed: %v", err)
	}
	if hashA == hashB {
		t.Fatal("legacy rootfs provenance hash did not change when rootfs size changed")
	}
}

func TestComputeRootfsSourceHashBaseImageTracksInputs(t *testing.T) {
	thawAgentPath := writeTestFile(t, "thaw-agent", []byte("thaw-agent-v1"))

	hashA, err := computeRootfsSourceHash("", "ubuntu:22.04", "runner", thawAgentPath, 0)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash base image hashA failed: %v", err)
	}
	hashB, err := computeRootfsSourceHash("", "ubuntu:24.04", "runner", thawAgentPath, 0)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash base image hashB failed: %v", err)
	}
	if hashA == hashB {
		t.Fatal("base-image rootfs provenance hash did not change when image URI changed")
	}

	if err := os.WriteFile(thawAgentPath, []byte("thaw-agent-v2"), 0644); err != nil {
		t.Fatalf("rewrite thaw-agent test file: %v", err)
	}
	hashC, err := computeRootfsSourceHash("", "ubuntu:22.04", "runner", thawAgentPath, 0)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash base image hashC failed: %v", err)
	}
	if hashA == hashC {
		t.Fatal("base-image rootfs provenance hash did not change when thaw-agent content changed")
	}
}

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write test file %s: %v", name, err)
	}
	return path
}
