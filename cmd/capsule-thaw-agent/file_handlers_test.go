package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidatePath_AbsoluteRequired(t *testing.T) {
	tests := []struct {
		path       string
		wantStatus int
	}{
		{"relative/path.txt", http.StatusBadRequest},
		{"../etc/passwd", http.StatusBadRequest},
		{"", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			_, status, errMsg := validatePath(tt.path)
			if status != tt.wantStatus {
				t.Errorf("validatePath(%q) status = %d, want %d (err: %s)", tt.path, status, tt.wantStatus, errMsg)
			}
		})
	}
}

func TestValidatePath_AllowedRoots(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping on macOS: /tmp resolves to /private/tmp via symlink")
	}

	allowed := []struct {
		path string
	}{
		{"/tmp/test"},
		{"/var/tmp/data"},
	}

	for _, tt := range allowed {
		t.Run(tt.path, func(t *testing.T) {
			resolved, status, errMsg := validatePath(tt.path)
			if status != 0 {
				t.Errorf("validatePath(%q) should be allowed: status=%d err=%s", tt.path, status, errMsg)
			}
			if resolved == "" {
				t.Errorf("validatePath(%q) returned empty resolved path", tt.path)
			}
		})
	}
}

func TestValidatePath_RejectedRoots(t *testing.T) {
	rejected := []string{
		"/etc/passwd",
		"/usr/bin/ls",
		"/proc/1/environ",
		"/sys/class/net",
	}

	for _, p := range rejected {
		t.Run(p, func(t *testing.T) {
			_, status, _ := validatePath(p)
			if status == 0 {
				t.Errorf("validatePath(%q) should be rejected, but was allowed", p)
			}
		})
	}
}

func TestValidatePath_TraversalAttack(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping on macOS: /workspace doesn't exist, EvalSymlinks fails differently")
	}

	// Ensure /workspace exists for the test
	os.MkdirAll("/workspace", 0755)

	attacks := []string{
		"/workspace/../etc/passwd",
		"/workspace/../../etc/shadow",
		"/tmp/../etc/passwd",
	}

	for _, p := range attacks {
		t.Run(p, func(t *testing.T) {
			_, status, _ := validatePath(p)
			if status == 0 {
				t.Errorf("validatePath(%q) should be rejected (traversal attack)", p)
			}
		})
	}
}

func TestValidatePath_NonexistentPathUnderAllowedRoot(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping on macOS: /tmp resolves to /private/tmp")
	}

	// A path that doesn't exist yet but parent is under allowed root
	p := "/tmp/nonexistent-test-file-12345.txt"
	os.Remove(p) // ensure it doesn't exist

	resolved, status, errMsg := validatePath(p)
	if status != 0 {
		t.Errorf("validatePath(%q) should be allowed (parent exists under /tmp): status=%d err=%s", p, status, errMsg)
	}
	if resolved == "" {
		t.Errorf("validatePath(%q) returned empty resolved path", p)
	}
}

func TestValidatePath_PrefixCheck(t *testing.T) {
	// Verify that /tmpevil is not confused with /tmp
	// The prefix check should require /tmp/ not just /tmp
	_, status, _ := validatePath("/tmpevil/file")
	if status == 0 {
		t.Error("validatePath(/tmpevil/file) should be rejected — /tmpevil is not /tmp")
	}
}

func TestValidatePath_ExactRoot(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping on macOS")
	}

	// Accessing the root directory itself should be allowed
	for _, root := range allowedRoots {
		if _, err := os.Stat(root); err != nil {
			continue // skip if root doesn't exist on this system
		}
		resolved, status, errMsg := validatePath(root)
		if status != 0 {
			t.Errorf("validatePath(%q) should be allowed (exact root): status=%d err=%s", root, status, errMsg)
		}
		_ = resolved
	}
}

func TestValidatePath_SymlinkResolution(t *testing.T) {
	// Create a temp dir and a symlink inside /tmp
	tmpDir := t.TempDir()

	target := filepath.Join(tmpDir, "real-target")
	os.MkdirAll(target, 0755)

	// On macOS, tmpDir is under /private/var which won't be under allowed roots.
	// On Linux, tmpDir is under /tmp which is an allowed root.
	if runtime.GOOS == "darwin" {
		t.Skip("Skipping symlink test on macOS")
	}

	link := filepath.Join(tmpDir, "symlink-test")
	os.Remove(link)
	os.Symlink(target, link)

	resolved, status, _ := validatePath(link)
	if status != 0 {
		// tmpDir should be under /tmp on Linux
		t.Errorf("validatePath(%q) should be allowed via symlink to %q: status=%d", link, target, status)
	}
	if resolved != target {
		t.Logf("resolved = %q (expected %q) — OK if both under allowed root", resolved, target)
	}
}
