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

func TestValidateBaseImagePolicy(t *testing.T) {
	if err := validateBaseImagePolicy(false, "ubuntu:22.04"); err != nil {
		t.Fatalf("validateBaseImagePolicy(non-incremental) returned error: %v", err)
	}
	if err := validateBaseImagePolicy(true, ""); err != nil {
		t.Fatalf("validateBaseImagePolicy(no-base-image) returned error: %v", err)
	}
	if err := validateBaseImagePolicy(true, "ubuntu@sha256:1111111111111111111111111111111111111111111111111111111111111111"); err != nil {
		t.Fatalf("validateBaseImagePolicy(pinned) returned error: %v", err)
	}
	if err := validateBaseImagePolicy(true, "ubuntu:22.04"); err == nil {
		t.Fatal("validateBaseImagePolicy(unpinned incremental) returned nil, want error")
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

	hashA, err := computeRootfsSourceHash(rootfsPath, "", "runner", "", 0, "")
	if err != nil {
		t.Fatalf("computeRootfsSourceHash legacy hashA failed: %v", err)
	}
	hashB, err := computeRootfsSourceHash(rootfsPath, "", "runner", "", 10, "")
	if err != nil {
		t.Fatalf("computeRootfsSourceHash legacy hashB failed: %v", err)
	}
	if hashA == hashB {
		t.Fatal("legacy rootfs provenance hash did not change when rootfs size changed")
	}
}

func TestComputeRootfsSourceHashBaseImageTracksInputs(t *testing.T) {
	thawAgentPath := writeTestFile(t, "thaw-agent", []byte("thaw-agent-v1"))

	hashA, err := computeRootfsSourceHash("", "ubuntu@sha256:1111111111111111111111111111111111111111111111111111111111111111", "runner", thawAgentPath, 0, rootfsFlavorDebianLike)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash base image hashA failed: %v", err)
	}
	hashB, err := computeRootfsSourceHash("", "ubuntu@sha256:2222222222222222222222222222222222222222222222222222222222222222", "runner", thawAgentPath, 0, rootfsFlavorDebianLike)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash base image hashB failed: %v", err)
	}
	if hashA == hashB {
		t.Fatal("base-image rootfs provenance hash did not change when image URI changed")
	}

	if err := os.WriteFile(thawAgentPath, []byte("thaw-agent-v2"), 0644); err != nil {
		t.Fatalf("rewrite thaw-agent test file: %v", err)
	}
	hashC, err := computeRootfsSourceHash("", "ubuntu@sha256:1111111111111111111111111111111111111111111111111111111111111111", "runner", thawAgentPath, 0, rootfsFlavorDebianLike)
	if err != nil {
		t.Fatalf("computeRootfsSourceHash base image hashC failed: %v", err)
	}
	if hashA == hashC {
		t.Fatal("base-image rootfs provenance hash did not change when thaw-agent content changed")
	}
}

func TestComputeRootfsSourceHashBaseImageRequiresDigestPin(t *testing.T) {
	thawAgentPath := writeTestFile(t, "thaw-agent", []byte("thaw-agent-v1"))

	if _, err := computeRootfsSourceHash("", "ubuntu:22.04", "runner", thawAgentPath, 0, rootfsFlavorDebianLike); err == nil {
		t.Fatal("computeRootfsSourceHash accepted an unpinned base image tag, want error")
	}
}

func TestComputePlatformShimFingerprintScopedByFlavor(t *testing.T) {
	debianHash, err := computePlatformShimFingerprint(rootfsFlavorDebianLike, "runner")
	if err != nil {
		t.Fatalf("computePlatformShimFingerprint debian failed: %v", err)
	}
	alpineHash, err := computePlatformShimFingerprint(rootfsFlavorAlpineLike, "runner")
	if err != nil {
		t.Fatalf("computePlatformShimFingerprint alpine failed: %v", err)
	}
	if debianHash == alpineHash {
		t.Fatal("platform shim fingerprint should differ across rootfs flavors")
	}
}

func TestComputePlatformShimFingerprintTracksRunnerUser(t *testing.T) {
	hashA, err := computePlatformShimFingerprint(rootfsFlavorDebianLike, "runner")
	if err != nil {
		t.Fatalf("computePlatformShimFingerprint hashA failed: %v", err)
	}
	hashB, err := computePlatformShimFingerprint(rootfsFlavorDebianLike, "builder")
	if err != nil {
		t.Fatalf("computePlatformShimFingerprint hashB failed: %v", err)
	}
	if hashA == hashB {
		t.Fatal("platform shim fingerprint should differ when runner user changes")
	}
}

func TestNormalizePinnedBaseImage(t *testing.T) {
	pinned := "ubuntu@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	got, err := normalizePinnedBaseImage(pinned)
	if err != nil {
		t.Fatalf("normalizePinnedBaseImage returned error for pinned image: %v", err)
	}
	if got != pinned {
		t.Fatalf("normalizePinnedBaseImage returned %q, want %q", got, pinned)
	}

	if _, err := normalizePinnedBaseImage("ubuntu:22.04"); err == nil {
		t.Fatal("normalizePinnedBaseImage accepted unpinned tag, want error")
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
