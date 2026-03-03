package main

import (
	"fmt"
	"os"
	"os/exec"

	"gopkg.in/yaml.v3"
)

// lookPath is a variable so tests can override it.
var lookPath = exec.LookPath

// Config represents the onboard configuration.
//
// Design principle (mirrors pkg/runner/types.go): the platform core handles generic
// VM snapshot/restore lifecycle. CI and Bazel are optional add-ons that layer on top.
//
//	platform + microvm + hosts  →  always required
//	workload                    →  required when ci.system is "none"
//	session                     →  optional; enables persistent cross-host sessions
//	ci                          →  add-on: CI system integration (default: "none")
//	bazel                       →  add-on: Bazel-specific snapshot warmup settings
//	repository                  →  required when snapshot uses git-clone commands
//	credentials                 →  optional: secrets and host dirs injected into VMs
type Config struct {
	Platform    PlatformConfig    `yaml:"platform"`
	MicroVM     MicroVMConfig     `yaml:"microvm"`
	Hosts       HostsConfig       `yaml:"hosts"`
	Workload    WorkloadConfig    `yaml:"workload"`
	Session     SessionConfig     `yaml:"session"`
	CI          CIConfig          `yaml:"ci"`
	Bazel       BazelConfig       `yaml:"bazel"`
	Repository  RepositoryConfig  `yaml:"repository"`
	Credentials CredentialsConfig `yaml:"credentials"`
}

// --- Core fields ---

type PlatformConfig struct {
	GCPProject string `yaml:"gcp_project"`
	Region     string `yaml:"region"`
	Zone       string `yaml:"zone"`
}

type MicroVMConfig struct {
	VCPUs      int `yaml:"vcpus"`
	MemoryMB   int `yaml:"memory_mb"`
	MaxPerHost int `yaml:"max_per_host"`
	IdleTarget int `yaml:"idle_target"`
}

type HostsConfig struct {
	MachineType string `yaml:"machine_type"`
	MinCount    int    `yaml:"min_count"`
	MaxCount    int    `yaml:"max_count"`
	DataDiskGB  int    `yaml:"data_disk_gb"`
}

// --- Workload: what runs inside the VM (required for ci.system: none) ---

// WorkloadConfig describes the golden snapshot content and the service the VM runs
// after restore. Required when ci.system is "none". Ignored when ci.system is
// "github-actions" (the snapshot commands and runner registration are derived from
// the CI and Bazel config blocks instead).
type WorkloadConfig struct {
	// SnapshotCommands lists warmup steps baked into the golden snapshot during build.
	// Valid types: "shell", "git-clone", "gcp-auth" (see examples/README.md for args).
	SnapshotCommands []SnapshotCommandConfig `yaml:"snapshot_commands"`

	// StartCommand describes the service to launch inside the VM after restore.
	// The thaw-agent starts Command, waits for GET HealthPath on Port to return 2xx,
	// then signals host readiness.
	StartCommand StartCommandConfig `yaml:"start_command"`
}

// SnapshotCommandConfig is one warmup step baked into the golden snapshot.
type SnapshotCommandConfig struct {
	// Type is one of: "shell", "git-clone", "gcp-auth"
	Type      string   `yaml:"type"`
	Args      []string `yaml:"args"`
	RunAsRoot bool     `yaml:"run_as_root"`
}

// StartCommandConfig describes the persistent service the VM runs after restore.
type StartCommandConfig struct {
	Command    []string `yaml:"command"`
	Port       int      `yaml:"port"`
	HealthPath string   `yaml:"health_path"`
}

// --- Session: optional persistent cross-host sessions ---

// SessionConfig controls pause/resume lifecycle.
// Used for dev environments, AI sandbox multi-turn sessions, and stateful serverless.
type SessionConfig struct {
	// Enabled turns on session persistence (pause to GCS on idle, resume on any host).
	Enabled bool `yaml:"enabled"`

	// TTLSeconds is the idle timeout before the VM is auto-paused.
	// 0 means no TTL (pause only on explicit request).
	TTLSeconds int `yaml:"ttl_seconds"`

	// AutoPause controls what happens when TTL fires:
	//   true  → pause VM state to GCS (preserves memory; resumes on next request)
	//   false → destroy VM (cheaper; no resume possible)
	AutoPause bool `yaml:"auto_pause"`
}

// --- CI add-on: CI system integration ---

// CIConfig holds optional CI system integration settings.
// Mirrors runner.CIConfig in pkg/runner/types.go.
// When System is "none" (or empty), the platform runs as a generic VM host and
// the Workload block is used to configure what runs inside the VM.
type CIConfig struct {
	// System is one of: "github-actions", "none"
	System string       `yaml:"system"`
	GitHub GitHubConfig `yaml:"github"`
}

// GitHubConfig holds GitHub Actions-specific runner registration settings.
// Only used when CIConfig.System is "github-actions".
type GitHubConfig struct {
	Repo      string   `yaml:"repo"`
	Org       string   `yaml:"org"`
	Labels    []string `yaml:"labels"`
	Ephemeral bool     `yaml:"ephemeral"`
}

// --- Bazel add-on: Bazel-specific snapshot warmup ---

// BazelConfig holds optional Bazel-specific settings for CI workloads.
// Mirrors runner.BazelConfig in pkg/runner/types.go.
// Only relevant when ci.system is "github-actions" and the snapshot bakes in a
// warmed Bazel server. For non-Bazel workloads, leave this block empty.
type BazelConfig struct {
	// WarmupTargets are the Bazel targets fetched/built during snapshot warmup.
	// Defaults to "//..." (all targets).
	WarmupTargets string `yaml:"warmup_targets"`

	// RepoCacheUpperSizeGB is the size of the per-runner writable overlay for the
	// Bazel repository cache. Defaults to 10.
	RepoCacheUpperSizeGB int `yaml:"repo_cache_upper_size_gb"`

	// GitCache configures a pre-built git mirror shared across runners on the host.
	// Enables fast reference cloning without re-downloading the full repo per job.
	GitCache GitCacheConfig `yaml:"git_cache"`

	// Buildbarn configures mTLS certs for Buildbarn remote execution.
	// Optional: only needed if your Bazel builds use a Buildbarn cluster.
	Buildbarn BuildbarnConfig `yaml:"buildbarn"`
}

// GitCacheConfig configures a host-local git mirror for fast reference cloning.
type GitCacheConfig struct {
	// Enabled turns on git-cache reference cloning inside microVMs.
	Enabled bool `yaml:"enabled"`

	// Repos maps repo URL patterns to their cache directory names on the host.
	// E.g. {"github.com/myorg/myrepo": "myrepo"} means the mirror is at
	// {host_cache_dir}/myrepo and the VM mounts it read-only.
	Repos map[string]string `yaml:"repos"`

	// WorkspaceDir is the target directory for cloned repos inside microVMs.
	// Defaults to "/mnt/ephemeral/workspace".
	WorkspaceDir string `yaml:"workspace_dir"`
}

// BuildbarnConfig holds Buildbarn mTLS certificate settings.
// Moved under BazelConfig because Buildbarn is a Bazel CI concern, not a
// platform concern. Mirrors runner.BazelConfig.BuildbarnCertsDir et al.
type BuildbarnConfig struct {
	// CertsDir is the host path containing Buildbarn mTLS certs (ca.crt, client.crt, etc.).
	// If set, the host packages this dir into a read-only ext4 image and attaches it to each VM.
	CertsDir string `yaml:"certs_dir"`
}

// --- Repository: source code access (required for git-clone snapshot commands) ---

// RepositoryConfig describes the source repository used in snapshot warmup.
// Required when any snapshot command uses type "git-clone", or when
// ci.system is "github-actions" (where git-clone is implicit).
// Not required for workloads whose snapshot_commands don't clone a repo.
type RepositoryConfig struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch"`

	// GitHubAppID and GitHubAppSecretName enable GitHub App authentication
	// for cloning private repositories. Leave empty for public repos.
	GitHubAppID         string `yaml:"github_app_id"`
	GitHubAppSecretName string `yaml:"github_app_secret_name"`
}

// --- Credentials: secrets and host dirs injected into VMs ---

type CredentialsConfig struct {
	Secrets  []SecretRef       `yaml:"secrets"`
	HostDirs []HostDirRef      `yaml:"host_dirs"`
	Env      map[string]string `yaml:"env"`
}

type SecretRef struct {
	Name       string `yaml:"name"`
	SecretName string `yaml:"secret_name"`
	Target     string `yaml:"target"`
}

type HostDirRef struct {
	Name     string `yaml:"name"`
	HostPath string `yaml:"host_path"`
	Target   string `yaml:"target"`
}

// LoadConfig loads and parses the onboard configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Apply defaults
	if cfg.Platform.Region == "" {
		cfg.Platform.Region = "us-central1"
	}
	if cfg.Platform.Zone == "" {
		cfg.Platform.Zone = cfg.Platform.Region + "-a"
	}
	if cfg.Repository.Branch == "" {
		cfg.Repository.Branch = "main"
	}
	if cfg.CI.System == "" {
		cfg.CI.System = "none"
	}
	if cfg.MicroVM.VCPUs == 0 {
		cfg.MicroVM.VCPUs = 4
	}
	if cfg.MicroVM.MemoryMB == 0 {
		cfg.MicroVM.MemoryMB = 8192
	}
	if cfg.MicroVM.MaxPerHost == 0 {
		cfg.MicroVM.MaxPerHost = 16
	}
	if cfg.MicroVM.IdleTarget == 0 {
		cfg.MicroVM.IdleTarget = 2
	}
	if cfg.Hosts.MachineType == "" {
		cfg.Hosts.MachineType = "n2-standard-64"
	}
	if cfg.Hosts.MinCount == 0 {
		cfg.Hosts.MinCount = 2
	}
	if cfg.Hosts.MaxCount == 0 {
		cfg.Hosts.MaxCount = 20
	}
	if cfg.Hosts.DataDiskGB == 0 {
		cfg.Hosts.DataDiskGB = 500
	}
	if cfg.Bazel.WarmupTargets == "" {
		cfg.Bazel.WarmupTargets = "//..."
	}
	if cfg.Bazel.RepoCacheUpperSizeGB == 0 {
		cfg.Bazel.RepoCacheUpperSizeGB = 10
	}

	return &cfg, nil
}

// needsGitClone reports whether the config requires a repository URL.
// True when ci.system is "github-actions" (implicit git-clone) or when
// any snapshot command has type "git-clone".
func (c *Config) needsGitClone() bool {
	if c.CI.System == "github-actions" {
		return true
	}
	for _, cmd := range c.Workload.SnapshotCommands {
		if cmd.Type == "git-clone" {
			return true
		}
	}
	return false
}

// Validate checks the configuration for required fields and consistency.
func (c *Config) Validate() error {
	if c.Platform.GCPProject == "" {
		return fmt.Errorf("platform.gcp_project is required")
	}

	// repository.url is only required when the snapshot needs to clone a repo.
	if c.needsGitClone() && c.Repository.URL == "" {
		return fmt.Errorf("repository.url is required when ci.system is github-actions or snapshot_commands include git-clone")
	}

	switch c.CI.System {
	case "github-actions":
		if c.CI.GitHub.Repo == "" && c.CI.GitHub.Org == "" {
			return fmt.Errorf("ci.github.repo or ci.github.org is required when ci.system is github-actions")
		}
	case "none":
		// Non-CI workloads must declare what to run inside the VM.
		if len(c.Workload.StartCommand.Command) == 0 {
			return fmt.Errorf("workload.start_command.command is required when ci.system is none")
		}
		if c.Workload.StartCommand.Port == 0 {
			return fmt.Errorf("workload.start_command.port is required when ci.system is none")
		}
	default:
		return fmt.Errorf("unsupported ci.system: %q (must be github-actions or none)", c.CI.System)
	}

	// Check toolchain availability.
	for _, tool := range []string{"gcloud", "terraform"} {
		if _, err := lookPath(tool); err != nil {
			return fmt.Errorf("required tool not found: %s", tool)
		}
	}

	return nil
}
