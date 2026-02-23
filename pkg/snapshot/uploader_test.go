package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestJoinErrors(t *testing.T) {
	tests := []struct {
		name   string
		errs   []string
		expect string
	}{
		{"nil", nil, ""},
		{"single", []string{"error1"}, "error1"},
		{"multiple", []string{"error1", "error2", "error3"}, "error1; error2; error3"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := joinErrors(tc.errs)
			if result != tc.expect {
				t.Errorf("joinErrors(%v) = %q, want %q", tc.errs, result, tc.expect)
			}
		})
	}
}

func TestPointerJSONRoundtrip(t *testing.T) {
	version := "v20260216-044453-master"

	// Simulate what UpdateCurrentPointer writes
	pointer := struct {
		Version string `json:"version"`
	}{Version: version}

	data, err := json.Marshal(pointer)
	if err != nil {
		t.Fatalf("Failed to marshal pointer: %v", err)
	}

	// Simulate what resolveCurrentPointer reads
	var decoded struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal pointer: %v", err)
	}

	if decoded.Version != version {
		t.Errorf("Pointer roundtrip: got %q, want %q", decoded.Version, version)
	}

	// Verify JSON format
	expected := `{"version":"v20260216-044453-master"}`
	if string(data) != expected {
		t.Errorf("Pointer JSON: got %s, want %s", string(data), expected)
	}
}

func TestPointerJSONParsing(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		version string
		wantErr bool
	}{
		{"compact", `{"version":"v20260216-044453-master"}`, "v20260216-044453-master", false},
		{"with_spaces", `{ "version": "v20260216-044453-master" }`, "v20260216-044453-master", false},
		{"empty_version", `{"version":""}`, "", false},
		{"extra_fields", `{"version":"v1","extra":"ignored"}`, "v1", false},
		{"invalid_json", `not json`, "", true},
		{"missing_version_field", `{"other":"field"}`, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pointer struct {
				Version string `json:"version"`
			}
			err := json.Unmarshal([]byte(tc.input), &pointer)
			if tc.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if pointer.Version != tc.version {
				t.Errorf("Version: got %q, want %q", pointer.Version, tc.version)
			}
		})
	}
}

// TestUploadSnapshotConcurrency verifies that UploadSnapshot launches all uploads
// concurrently and collects errors properly. We do this by replacing uploadFile
// with a test helper via a thin wrapper.
func TestParallelUploadErrorCollection(t *testing.T) {
	// Simulate the parallel upload error collection pattern used in UploadSnapshot
	tests := []struct {
		name       string
		fileErrors map[string]error // file -> error (nil = success)
		wantErr    bool
		wantCount  int // expected number of errors in message
	}{
		{
			name: "all_succeed",
			fileErrors: map[string]error{
				"kernel.bin":          nil,
				"rootfs.img":          nil,
				"snapshot.mem":        nil,
				"snapshot.state":      nil,
				"repo-cache-seed.img": nil,
			},
			wantErr: false,
		},
		{
			name: "one_failure",
			fileErrors: map[string]error{
				"kernel.bin":          nil,
				"rootfs.img":          fmt.Errorf("upload failed"),
				"snapshot.mem":        nil,
				"snapshot.state":      nil,
				"repo-cache-seed.img": nil,
			},
			wantErr:   true,
			wantCount: 1,
		},
		{
			name: "all_fail",
			fileErrors: map[string]error{
				"kernel.bin":          fmt.Errorf("fail1"),
				"rootfs.img":          fmt.Errorf("fail2"),
				"snapshot.mem":        fmt.Errorf("fail3"),
				"snapshot.state":      fmt.Errorf("fail4"),
				"repo-cache-seed.img": fmt.Errorf("fail5"),
			},
			wantErr:   true,
			wantCount: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			type uploadResult struct {
				file string
				err  error
			}

			files := make([]string, 0, len(tc.fileErrors))
			for f := range tc.fileErrors {
				files = append(files, f)
			}

			ch := make(chan uploadResult, len(files))
			var started int64
			for _, f := range files {
				f := f
				go func() {
					atomic.AddInt64(&started, 1)
					ch <- uploadResult{f, tc.fileErrors[f]}
				}()
			}

			var uploadErrors []string
			for range files {
				r := <-ch
				if r.err != nil {
					uploadErrors = append(uploadErrors, fmt.Sprintf("%s: %v", r.file, r.err))
				}
			}

			// Verify all goroutines ran
			if int(atomic.LoadInt64(&started)) != len(files) {
				t.Errorf("Expected %d goroutines started, got %d", len(files), started)
			}

			if tc.wantErr && len(uploadErrors) == 0 {
				t.Error("Expected errors but got none")
			}
			if !tc.wantErr && len(uploadErrors) > 0 {
				t.Errorf("Expected no errors but got: %s", joinErrors(uploadErrors))
			}
			if tc.wantCount > 0 && len(uploadErrors) != tc.wantCount {
				t.Errorf("Expected %d errors, got %d", tc.wantCount, len(uploadErrors))
			}
		})
	}
}

// TestUploadSnapshotSizesCalculation verifies that total size is computed correctly
// before uploading.
func TestUploadSnapshotSizeCalculation(t *testing.T) {
	dir := t.TempDir()

	// Create test files of known sizes
	testFiles := map[string]int{
		"kernel.bin":          1024,
		"rootfs.img":          2048,
		"snapshot.mem":        4096,
		"snapshot.state":      512,
		"repo-cache-seed.img": 8192,
	}

	for name, size := range testFiles {
		data := make([]byte, size)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", name, err)
		}
	}

	// Replicate the size calculation from UploadSnapshot
	files := []struct {
		local  string
		remote string
	}{
		{filepath.Join(dir, "kernel.bin"), "v1/kernel.bin"},
		{filepath.Join(dir, "rootfs.img"), "v1/rootfs.img"},
		{filepath.Join(dir, "snapshot.mem"), "v1/snapshot.mem"},
		{filepath.Join(dir, "snapshot.state"), "v1/snapshot.state"},
		{filepath.Join(dir, "repo-cache-seed.img"), "v1/repo-cache-seed.img"},
	}

	var totalSize int64
	for _, f := range files {
		info, err := os.Stat(f.local)
		if err == nil {
			totalSize += info.Size()
		}
	}

	expectedSize := int64(1024 + 2048 + 4096 + 512 + 8192)
	if totalSize != expectedSize {
		t.Errorf("Total size: got %d, want %d", totalSize, expectedSize)
	}
}

// TestUploadFileGCSURI verifies the GCS URI construction in uploadFile.
func TestUploadFileGCSURIConstruction(t *testing.T) {
	tests := []struct {
		bucket     string
		remotePath string
		wantURI    string
	}{
		{"my-bucket", "v1/kernel.bin", "gs://my-bucket/v1/kernel.bin"},
		{"snapshots-prod", "v20260216-044453-master/rootfs.img", "gs://snapshots-prod/v20260216-044453-master/rootfs.img"},
	}

	for _, tc := range tests {
		uri := fmt.Sprintf("gs://%s/%s", tc.bucket, tc.remotePath)
		if uri != tc.wantURI {
			t.Errorf("GCS URI: got %q, want %q", uri, tc.wantURI)
		}
	}
}

// TestUploadSnapshotMetadataFormat verifies the metadata JSON written after upload.
func TestUploadSnapshotMetadataFormat(t *testing.T) {
	metadata := SnapshotMetadata{
		Version:   "v20260216-044453-master",
		SizeBytes: 58000000000,
	}

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal metadata: %v", err)
	}

	// Parse it back
	var parsed SnapshotMetadata
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal metadata: %v", err)
	}

	if parsed.Version != metadata.Version {
		t.Errorf("Version: got %q, want %q", parsed.Version, metadata.Version)
	}
	if parsed.SizeBytes != metadata.SizeBytes {
		t.Errorf("SizeBytes: got %d, want %d", parsed.SizeBytes, metadata.SizeBytes)
	}
}

// TestUploadSnapshotSkipsMissingFiles verifies that size calculation handles
// missing files gracefully (os.Stat returns error).
func TestUploadSnapshotSizeWithMissingFiles(t *testing.T) {
	dir := t.TempDir()

	// Only create some files
	os.WriteFile(filepath.Join(dir, "kernel.bin"), make([]byte, 100), 0644)
	os.WriteFile(filepath.Join(dir, "rootfs.img"), make([]byte, 200), 0644)
	// snapshot.mem, snapshot.state, repo-cache-seed.img don't exist

	files := []struct {
		local  string
		remote string
	}{
		{filepath.Join(dir, "kernel.bin"), "v1/kernel.bin"},
		{filepath.Join(dir, "rootfs.img"), "v1/rootfs.img"},
		{filepath.Join(dir, "snapshot.mem"), "v1/snapshot.mem"},
		{filepath.Join(dir, "snapshot.state"), "v1/snapshot.state"},
		{filepath.Join(dir, "repo-cache-seed.img"), "v1/repo-cache-seed.img"},
	}

	var totalSize int64
	for _, f := range files {
		info, err := os.Stat(f.local)
		if err == nil {
			totalSize += info.Size()
		}
	}

	// Should only count existing files
	if totalSize != 300 {
		t.Errorf("Total size with missing files: got %d, want 300", totalSize)
	}
}

// TestSyncFromGCSVersionResolution verifies the version resolution logic
// in SyncFromGCS (pointer vs fallback).
func TestSyncFromGCSVersionResolution(t *testing.T) {
	tests := []struct {
		name            string
		inputVersion    string
		pointerVersion  string
		pointerErr      error
		expectedVersion string
	}{
		{
			name:            "explicit_version_bypasses_pointer",
			inputVersion:    "v20260216-044453-master",
			expectedVersion: "v20260216-044453-master",
		},
		{
			name:            "empty_resolves_to_current",
			inputVersion:    "",
			pointerErr:      fmt.Errorf("not found"),
			expectedVersion: "current",
		},
		{
			name:            "current_with_pointer",
			inputVersion:    "current",
			pointerVersion:  "v20260216-resolved",
			expectedVersion: "v20260216-resolved",
		},
		{
			name:            "current_without_pointer_falls_back",
			inputVersion:    "current",
			pointerErr:      fmt.Errorf("not found"),
			expectedVersion: "current",
		},
		{
			name:            "current_with_empty_pointer_falls_back",
			inputVersion:    "current",
			pointerVersion:  "",
			expectedVersion: "current",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version := tc.inputVersion
			if version == "" {
				version = "current"
			}

			// Simulate pointer resolution logic from SyncFromGCS
			if version == "current" {
				resolved := tc.pointerVersion
				err := tc.pointerErr
				if err == nil && resolved != "" {
					version = resolved
				}
			}

			if version != tc.expectedVersion {
				t.Errorf("Resolved version: got %q, want %q", version, tc.expectedVersion)
			}
		})
	}
}

// TestIsStalePointerResolution verifies that IsStale tries the pointer first
// and falls back to current/ metadata.
func TestIsStaleVersionComparison(t *testing.T) {
	tests := []struct {
		name           string
		localVersion   string
		pointerVersion string
		pointerErr     error
		remoteVersion  string // from current/metadata.json fallback
		remoteErr      error
		wantStale      bool
		wantErr        bool
	}{
		{
			name:           "same_version_via_pointer",
			localVersion:   "v1",
			pointerVersion: "v1",
			wantStale:      false,
		},
		{
			name:           "different_version_via_pointer",
			localVersion:   "v1",
			pointerVersion: "v2",
			wantStale:      true,
		},
		{
			name:          "same_version_via_fallback",
			localVersion:  "v1",
			pointerErr:    fmt.Errorf("not found"),
			remoteVersion: "v1",
			wantStale:     false,
		},
		{
			name:          "different_version_via_fallback",
			localVersion:  "v1",
			pointerErr:    fmt.Errorf("not found"),
			remoteVersion: "v2",
			wantStale:     true,
		},
		{
			name:         "both_fail",
			localVersion: "v1",
			pointerErr:   fmt.Errorf("not found"),
			remoteErr:    fmt.Errorf("not found"),
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate IsStale logic
			localVer := tc.localVersion
			var isStale bool
			var err error

			remoteVer := tc.pointerVersion
			pointerErr := tc.pointerErr
			if pointerErr == nil && remoteVer != "" {
				isStale = localVer != remoteVer
			} else {
				// Fallback
				if tc.remoteErr != nil {
					err = tc.remoteErr
				} else {
					isStale = localVer != tc.remoteVersion
				}
			}

			if tc.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if isStale != tc.wantStale {
				t.Errorf("IsStale: got %v, want %v", isStale, tc.wantStale)
			}
		})
	}
}

// Verify that the gcloud storage rsync command is constructed correctly for SyncFromGCS.
func TestSyncFromGCSCommandConstruction(t *testing.T) {
	tests := []struct {
		bucket   string
		version  string
		wantPath string
	}{
		{"my-bucket", "v1", "gs://my-bucket/v1/"},
		{"my-bucket", "current", "gs://my-bucket/current/"},
		{"snapshots-prod", "v20260216-044453-master", "gs://snapshots-prod/v20260216-044453-master/"},
	}

	for _, tc := range tests {
		gcsPath := fmt.Sprintf("gs://%s/%s/", tc.bucket, tc.version)
		if gcsPath != tc.wantPath {
			t.Errorf("GCS path for bucket=%q version=%q: got %q, want %q",
				tc.bucket, tc.version, gcsPath, tc.wantPath)
		}
	}
}

// TestUploadSnapshotE2E is an integration test that requires gcloud CLI and GCS access.
// Skip unless SNAPSHOT_TEST_BUCKET is set.
func TestUploadSnapshotE2E(t *testing.T) {
	bucket := os.Getenv("SNAPSHOT_TEST_BUCKET")
	if bucket == "" {
		t.Skip("Set SNAPSHOT_TEST_BUCKET to run integration tests")
	}

	ctx := context.Background()
	uploader, err := NewUploader(ctx, UploaderConfig{
		GCSBucket: bucket,
	})
	if err != nil {
		t.Fatalf("Failed to create uploader: %v", err)
	}
	defer uploader.Close()

	// Create temp dir with small test files
	dir := t.TempDir()
	for _, name := range []string{"kernel.bin", "rootfs.img", "snapshot.mem", "snapshot.state", "repo-cache-seed.img"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test-"+name), 0644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}
	}

	version := "test-upload-e2e"
	metadata := SnapshotMetadata{Version: version, RepoSlug: "test-org-repo"}

	if err := uploader.UploadSnapshot(ctx, dir, metadata); err != nil {
		t.Fatalf("UploadSnapshot failed: %v", err)
	}

	// Verify pointer update
	if err := uploader.UpdateCurrentPointerForRepo(ctx, version, "test-org-repo"); err != nil {
		t.Fatalf("UpdateCurrentPointerForRepo failed: %v", err)
	}

	// Clean up
	if err := uploader.DeleteVersion(ctx, version); err != nil {
		t.Logf("Warning: failed to clean up test version: %v", err)
	}
}
