package main

import (
	"path/filepath"
	"testing"
)

func TestExtractRepoDir(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"https URL", "https://github.com/org/repo", filepath.Join("repo", "repo")},
		{"https with .git", "https://github.com/org/repo.git", filepath.Join("repo", "repo")},
		{"short form", "askscio/scio", filepath.Join("scio", "scio")},
		{"git@ SSH", "git@github.com:org/repo.git", filepath.Join("repo", "repo")},
		{"http URL", "http://github.com/org/repo", filepath.Join("repo", "repo")},
		{"bare repo name", "myrepo", filepath.Join("myrepo", "myrepo")},
		{"deep path", "https://github.com/org/sub/repo", filepath.Join("repo", "repo")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRepoDir(tt.url)
			if got != tt.want {
				t.Errorf("extractRepoDir(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestGetPreClonedPath(t *testing.T) {
	tests := []struct {
		name string
		data *MMDSData
		want string
	}{
		{"nil data", nil, ""},
		{"explicit path", func() *MMDSData {
			d := &MMDSData{}
			d.Latest.GitCache.PreClonedPath = "/custom/path"
			return d
		}(), "/custom/path"},
		{"derived from repo", func() *MMDSData {
			d := &MMDSData{}
			d.Latest.Job.Repo = "askscio/scio"
			return d
		}(), filepath.Join("/workspace", "scio", "scio")},
		{"no repo", func() *MMDSData {
			return &MMDSData{}
		}(), ""},
		{"explicit overrides derived", func() *MMDSData {
			d := &MMDSData{}
			d.Latest.GitCache.PreClonedPath = "/explicit"
			d.Latest.Job.Repo = "askscio/scio"
			return d
		}(), "/explicit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPreClonedPath(tt.data)
			if got != tt.want {
				t.Errorf("getPreClonedPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetWorkspaceRepoPath(t *testing.T) {
	tests := []struct {
		name string
		data *MMDSData
		want string
	}{
		{"nil data", nil, ""},
		{"custom workspace dir", func() *MMDSData {
			d := &MMDSData{}
			d.Latest.GitCache.WorkspaceDir = "/custom/workdir"
			d.Latest.Job.Repo = "org/repo"
			return d
		}(), filepath.Join("/custom/workdir", "repo", "repo")},
		{"default workspace dir", func() *MMDSData {
			d := &MMDSData{}
			d.Latest.Job.Repo = "org/repo"
			return d
		}(), filepath.Join("/mnt/ephemeral/workdir", "repo", "repo")},
		{"no repo", func() *MMDSData {
			d := &MMDSData{}
			d.Latest.GitCache.WorkspaceDir = "/workdir"
			return d
		}(), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getWorkspaceRepoPath(tt.data)
			if got != tt.want {
				t.Errorf("getWorkspaceRepoPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name string
		data string
		want map[string]string
	}{
		{"valid pairs", "FOO=bar\nBAZ=qux\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"comments", "# comment\nFOO=bar\n# another\n", map[string]string{"FOO": "bar"}},
		{"empty lines", "\n\nFOO=bar\n\n", map[string]string{"FOO": "bar"}},
		{"no equals", "INVALID_LINE\nFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"value with equals", "URL=https://example.com?a=1&b=2\n", map[string]string{"URL": "https://example.com?a=1&b=2"}},
		{"empty input", "", map[string]string{}},
		{"whitespace lines", "  \n\t\n  FOO=bar  \n", map[string]string{"FOO": "bar"}},
		{"value with spaces", "MSG=hello world\n", map[string]string{"MSG": "hello world"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEnvFile([]byte(tt.data))
			if len(got) != len(tt.want) {
				t.Errorf("parseEnvFile() returned %d entries, want %d\ngot: %v\nwant: %v", len(got), len(tt.want), got, tt.want)
				return
			}
			for k, wantV := range tt.want {
				if gotV, ok := got[k]; !ok || gotV != wantV {
					t.Errorf("parseEnvFile()[%q] = %q, want %q", k, gotV, wantV)
				}
			}
		})
	}
}
