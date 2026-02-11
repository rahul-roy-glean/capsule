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
type Config struct {
	Platform    PlatformConfig    `yaml:"platform"`
	Repository  RepositoryConfig  `yaml:"repository"`
	Bazel       BazelConfig       `yaml:"bazel"`
	MicroVM     MicroVMConfig     `yaml:"microvm"`
	Hosts       HostsConfig       `yaml:"hosts"`
	CI          CIConfig          `yaml:"ci"`
	Credentials CredentialsConfig `yaml:"credentials"`
	Buildbarn   BuildbarnConfig   `yaml:"buildbarn"`
}

type PlatformConfig struct {
	GCPProject string `yaml:"gcp_project"`
	Region     string `yaml:"region"`
	Zone       string `yaml:"zone"`
}

type RepositoryConfig struct {
	URL                 string `yaml:"url"`
	Branch              string `yaml:"branch"`
	GitHubAppID         string `yaml:"github_app_id"`
	GitHubAppSecretName string `yaml:"github_app_secret_name"`
}

type BazelConfig struct {
	WarmupTargets string `yaml:"warmup_targets"`
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

type CIConfig struct {
	System string       `yaml:"system"`
	GitHub GitHubConfig `yaml:"github"`
}

type GitHubConfig struct {
	Repo      string   `yaml:"repo"`
	Org       string   `yaml:"org"`
	Labels    []string `yaml:"labels"`
	Ephemeral bool     `yaml:"ephemeral"`
}

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

type BuildbarnConfig struct {
	CertsDir string `yaml:"certs_dir"`
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
	if cfg.Bazel.WarmupTargets == "" {
		cfg.Bazel.WarmupTargets = "//..."
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
	if cfg.CI.System == "" {
		cfg.CI.System = "github-actions"
	}

	return &cfg, nil
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if c.Platform.GCPProject == "" {
		return fmt.Errorf("platform.gcp_project is required")
	}
	if c.Repository.URL == "" {
		return fmt.Errorf("repository.url is required")
	}

	// Validate CI system
	switch c.CI.System {
	case "github-actions":
		if c.CI.GitHub.Repo == "" && c.CI.GitHub.Org == "" {
			return fmt.Errorf("ci.github.repo or ci.github.org is required when ci.system is github-actions")
		}
	case "none":
		// OK
	default:
		return fmt.Errorf("unsupported ci.system: %s (must be github-actions or none)", c.CI.System)
	}

	// Check toolchain availability
	for _, tool := range []string{"gcloud", "terraform"} {
		if _, err := lookPath(tool); err != nil {
			return fmt.Errorf("required tool not found: %s", tool)
		}
	}

	return nil
}
