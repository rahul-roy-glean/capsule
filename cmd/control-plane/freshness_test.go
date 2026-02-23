package main

import (
	"testing"
)

func TestParseGitHubRepo(t *testing.T) {
	tests := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"https://github.com/org/repo", "org", "repo", false},
		{"https://github.com/org/repo.git", "org", "repo", false},
		{"git@github.com:org/repo.git", "org", "repo", false},
		{"github.com/org/repo", "org", "repo", false},
		{"org/repo", "org", "repo", false},
		{"http://github.com/my-org/my-repo.git", "my-org", "my-repo", false},
		{"noslash", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			owner, repo, err := parseGitHubRepo(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGitHubRepo(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if owner != tt.wantOwner {
				t.Errorf("parseGitHubRepo(%q) owner = %q, want %q", tt.input, owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("parseGitHubRepo(%q) repo = %q, want %q", tt.input, repo, tt.wantRepo)
			}
		})
	}
}

func TestShouldBuildNow(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		// We can't control time.Now() easily, so test structural properties
	}{
		{"empty schedule returns false", ""},
		{"short schedule returns false", "*/5"},
		{"invalid returns false", "abc * * * *"},
	}

	// Empty schedule should always return false
	if shouldBuildNow("") {
		t.Error("shouldBuildNow(\"\") should return false")
	}

	// Too-short schedule should return false
	if shouldBuildNow("*/5") {
		t.Error("shouldBuildNow(\"*/5\") should return false (not enough fields)")
	}

	// Invalid minute field should return false
	if shouldBuildNow("abc * * * *") {
		t.Error("shouldBuildNow(\"abc * * * *\") should return false")
	}

	// Valid schedule: test that it doesn't panic
	_ = shouldBuildNow("*/5 * * * *")
	_ = shouldBuildNow("0 * * * *")
	_ = shouldBuildNow("30 * * * *")

	// Ignore the tests variable to avoid unused warning
	_ = tests
}
