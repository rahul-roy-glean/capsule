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
// VM snapshot/restore lifecycle.
//
//	platform + microvm + hosts  →  always required
//	workload                    →  what runs inside the VM
//	session                     →  optional; enables persistent cross-host sessions
//	credentials                 →  optional: secrets and host dirs injected into VMs
type Config struct {
	Platform    PlatformConfig    `yaml:"platform"`
	MicroVM     MicroVMConfig     `yaml:"microvm"`
	Hosts       HostsConfig       `yaml:"hosts"`
	Workload    WorkloadConfig    `yaml:"workload"`
	Session     SessionConfig     `yaml:"session"`
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

// --- Workload: what runs inside the VM ---

// WorkloadConfig describes the golden snapshot content and the service the VM runs
// after restore.
type WorkloadConfig struct {
	// SnapshotCommands lists warmup steps baked into the golden snapshot during build.
	// Valid types: "shell", "gcp-auth", "exec" (see examples/README.md for args).
	SnapshotCommands []SnapshotCommandConfig `yaml:"snapshot_commands"`

	// StartCommand describes the service to launch inside the VM after restore.
	// The thaw-agent starts Command, waits for GET HealthPath on Port to return 2xx,
	// then signals host readiness.
	StartCommand StartCommandConfig `yaml:"start_command"`
}

// SnapshotCommandConfig is one warmup step baked into the golden snapshot.
type SnapshotCommandConfig struct {
	// Type is one of: "shell", "gcp-auth", "exec"
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

	return &cfg, nil
}

// Validate checks the configuration for required fields and consistency.
func (c *Config) Validate() error {
	if c.Platform.GCPProject == "" {
		return fmt.Errorf("platform.gcp_project is required")
	}

	// Check toolchain availability.
	for _, tool := range []string{"gcloud", "terraform"} {
		if _, err := lookPath(tool); err != nil {
			return fmt.Errorf("required tool not found: %s", tool)
		}
	}

	return nil
}
