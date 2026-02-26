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
	// Bazel add-on fields
	if cfg.Bazel.WarmupTargets != "//src/..." {
		t.Errorf("Bazel.WarmupTargets = %q, want %q", cfg.Bazel.WarmupTargets, "//src/...")
	}
	if cfg.Bazel.RepoCacheUpperSizeGB != 20 {
		t.Errorf("Bazel.RepoCacheUpperSizeGB = %d, want %d", cfg.Bazel.RepoCacheUpperSizeGB, 20)
	}
	if !cfg.Bazel.GitCache.Enabled {
		t.Error("Bazel.GitCache.Enabled = false, want true")
	}
	if cfg.Bazel.GitCache.Repos["github.com/org/repo"] != "org-repo" {
		t.Errorf("Bazel.GitCache.Repos[github.com/org/repo] = %q, want %q",
			cfg.Bazel.GitCache.Repos["github.com/org/repo"], "org-repo")
	}
	if cfg.Bazel.Buildbarn.CertsDir != "/etc/glean/ci/certs/buildbarn" {
		t.Errorf("Bazel.Buildbarn.CertsDir = %q, want %q",
			cfg.Bazel.Buildbarn.CertsDir, "/etc/glean/ci/certs/buildbarn")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_defaults.yaml")
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
	if cfg.CI.System != "none" {
		t.Errorf("CI.System = %q, want default %q", cfg.CI.System, "none")
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
		Workload: WorkloadConfig{
			StartCommand: StartCommandConfig{Command: []string{"/app/server"}, Port: 8080},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for missing project")
	}
}

func TestValidate_MissingRepoURL(t *testing.T) {
	// repository.url is required when ci.system is github-actions (implicit git-clone).
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		CI:       CIConfig{System: "github-actions", GitHub: GitHubConfig{Repo: "org/repo"}},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for missing repo URL with github-actions")
	}
}

func TestValidate_MissingRepoURL_GitCloneCommand(t *testing.T) {
	// repository.url is also required when a snapshot command uses git-clone.
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		CI:       CIConfig{System: "none"},
		Workload: WorkloadConfig{
			SnapshotCommands: []SnapshotCommandConfig{{Type: "git-clone", Args: []string{"main"}}},
			StartCommand:     StartCommandConfig{Command: []string{"/app/server"}, Port: 8080},
		},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for missing repo URL with git-clone snapshot command")
	}
}

func TestValidate_NoRepoURL_NoGitClone(t *testing.T) {
	// repository.url is NOT required when ci.system is none and no git-clone is used.
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		CI:       CIConfig{System: "none"},
		Workload: WorkloadConfig{
			SnapshotCommands: []SnapshotCommandConfig{
				{Type: "shell", Args: []string{"pip3", "install", "-r", "/app/requirements.txt"}, RunAsRoot: true},
			},
			StartCommand: StartCommandConfig{Command: []string{"/app/server"}, Port: 8080},
		},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() unexpected error when no git-clone needed: %v", err)
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
		Workload: WorkloadConfig{
			StartCommand: StartCommandConfig{
				Command:    []string{"/app/server", "--port=8080"},
				Port:       8080,
				HealthPath: "/health",
			},
		},
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
		Workload: WorkloadConfig{
			StartCommand: StartCommandConfig{Command: []string{"/app/server"}, Port: 8080},
		},
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

func TestValidate_NoneCIMissingStartCommand(t *testing.T) {
	cfg := &Config{
		Platform:   PlatformConfig{GCPProject: "my-project"},
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "none"},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for none CI without start_command")
	}
}

func TestValidate_NoneCIMissingPort(t *testing.T) {
	cfg := &Config{
		Platform:   PlatformConfig{GCPProject: "my-project"},
		Repository: RepositoryConfig{URL: "https://github.com/org/repo"},
		CI:         CIConfig{System: "none"},
		Workload: WorkloadConfig{
			StartCommand: StartCommandConfig{Command: []string{"/app/server"}},
		},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for none CI with start_command but no port")
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
