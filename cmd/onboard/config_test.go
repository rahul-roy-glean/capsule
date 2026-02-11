package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ValidFull(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_full.yaml")
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Platform.GCPProject != "my-project" {
		t.Errorf("GCPProject = %q, want %q", cfg.Platform.GCPProject, "my-project")
	}
	if cfg.Platform.Region != "us-west1" {
		t.Errorf("Region = %q, want %q", cfg.Platform.Region, "us-west1")
	}
	if cfg.Platform.Zone != "us-west1-b" {
		t.Errorf("Zone = %q, want %q", cfg.Platform.Zone, "us-west1-b")
	}
	if cfg.Repository.Branch != "develop" {
		t.Errorf("Branch = %q, want %q", cfg.Repository.Branch, "develop")
	}
	if cfg.MicroVM.VCPUs != 8 {
		t.Errorf("VCPUs = %d, want %d", cfg.MicroVM.VCPUs, 8)
	}
	if cfg.MicroVM.MemoryMB != 16384 {
		t.Errorf("MemoryMB = %d, want %d", cfg.MicroVM.MemoryMB, 16384)
	}
	if cfg.Hosts.MachineType != "n2-standard-128" {
		t.Errorf("MachineType = %q, want %q", cfg.Hosts.MachineType, "n2-standard-128")
	}
	if cfg.CI.System != "github-actions" {
		t.Errorf("CI.System = %q, want %q", cfg.CI.System, "github-actions")
	}
	if cfg.CI.GitHub.Repo != "org/repo" {
		t.Errorf("CI.GitHub.Repo = %q, want %q", cfg.CI.GitHub.Repo, "org/repo")
	}
	if !cfg.CI.GitHub.Ephemeral {
		t.Error("CI.GitHub.Ephemeral = false, want true")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_minimal.yaml")
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Platform.Region != "us-central1" {
		t.Errorf("Region = %q, want default %q", cfg.Platform.Region, "us-central1")
	}
	if cfg.Platform.Zone != "us-central1-a" {
		t.Errorf("Zone = %q, want default %q", cfg.Platform.Zone, "us-central1-a")
	}
	if cfg.Repository.Branch != "main" {
		t.Errorf("Branch = %q, want default %q", cfg.Repository.Branch, "main")
	}
	if cfg.Bazel.WarmupTargets != "//..." {
		t.Errorf("WarmupTargets = %q, want default %q", cfg.Bazel.WarmupTargets, "//...")
	}
	if cfg.MicroVM.VCPUs != 4 {
		t.Errorf("VCPUs = %d, want default %d", cfg.MicroVM.VCPUs, 4)
	}
	if cfg.MicroVM.MemoryMB != 8192 {
		t.Errorf("MemoryMB = %d, want default %d", cfg.MicroVM.MemoryMB, 8192)
	}
	if cfg.MicroVM.MaxPerHost != 16 {
		t.Errorf("MaxPerHost = %d, want default %d", cfg.MicroVM.MaxPerHost, 16)
	}
	if cfg.MicroVM.IdleTarget != 2 {
		t.Errorf("IdleTarget = %d, want default %d", cfg.MicroVM.IdleTarget, 2)
	}
	if cfg.Hosts.MachineType != "n2-standard-64" {
		t.Errorf("MachineType = %q, want default %q", cfg.Hosts.MachineType, "n2-standard-64")
	}
	if cfg.Hosts.MinCount != 2 {
		t.Errorf("MinCount = %d, want default %d", cfg.Hosts.MinCount, 2)
	}
	if cfg.Hosts.MaxCount != 20 {
		t.Errorf("MaxCount = %d, want default %d", cfg.Hosts.MaxCount, 20)
	}
	if cfg.Hosts.DataDiskGB != 500 {
		t.Errorf("DataDiskGB = %d, want default %d", cfg.Hosts.DataDiskGB, 500)
	}
	if cfg.CI.System != "github-actions" {
		t.Errorf("CI.System = %q, want default %q", cfg.CI.System, "github-actions")
	}
}

func TestLoadConfig_MinimalValid(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_minimal.yaml")
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Platform.GCPProject != "minimal-project" {
		t.Errorf("GCPProject = %q, want %q", cfg.Platform.GCPProject, "minimal-project")
	}
	if cfg.Repository.URL != "https://github.com/org/minimal" {
		t.Errorf("URL = %q, want %q", cfg.Repository.URL, "https://github.com/org/minimal")
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	_, err := LoadConfig("testdata/invalid.yaml")
	if err == nil {
		t.Error("LoadConfig() expected error for invalid YAML, got nil")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("testdata/nonexistent.yaml")
	if err == nil {
		t.Error("LoadConfig() expected error for missing file, got nil")
	}
}

func TestValidate_MissingProject(t *testing.T) {
	cfg := &Config{
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "none"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for missing project")
	}
}

func TestValidate_MissingRepoURL(t *testing.T) {
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		CI:       CIConfig{System: "none"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for missing repo URL")
	}
}

func TestValidate_GitHubActionsNoRepo(t *testing.T) {
	cfg := &Config{
		Platform:   PlatformConfig{GCPProject: "my-project"},
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "github-actions"},
	}
	// Override lookPath so tool checks pass
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for github-actions without repo or org")
	}
}

func TestValidate_UnsupportedCISystem(t *testing.T) {
	cfg := &Config{
		Platform:   PlatformConfig{GCPProject: "my-project"},
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "jenkins"},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for unsupported CI system")
	}
}

func TestValidate_NoneCI(t *testing.T) {
	cfg := &Config{
		Platform:   PlatformConfig{GCPProject: "my-project"},
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "none"},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v for valid none CI config", err)
	}
}

func TestValidate_ToolCheck(t *testing.T) {
	cfg := &Config{
		Platform:   PlatformConfig{GCPProject: "my-project"},
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "none"},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) {
		return "", &os.PathError{Op: "exec", Path: name, Err: os.ErrNotExist}
	}
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error when tools are missing")
	}
}

func TestValidate_GitHubActionsWithOrg(t *testing.T) {
	cfg, err := LoadConfig("testdata/github_actions.yaml")
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v for config with org", err)
	}
}

func TestLoadConfig_ZoneDefaultFromRegion(t *testing.T) {
	// Create a temp config with custom region but no zone
	dir := t.TempDir()
	content := []byte("platform:\n  gcp_project: test\n  region: europe-west1\nrepository:\n  url: https://github.com/org/repo\nci:\n  system: none\n")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Platform.Zone != "europe-west1-a" {
		t.Errorf("Zone = %q, want %q (derived from region)", cfg.Platform.Zone, "europe-west1-a")
	}
}
