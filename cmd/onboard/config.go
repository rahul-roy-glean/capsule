package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/rahul-roy-glean/capsule/pkg/accessplane"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
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

	ResolvedDBPassword      string `yaml:"-"`
	ResolvedStateBucket     string `yaml:"-"`
	ResolvedControlPlaneIP  string `yaml:"-"`
	ResolvedControlPlaneURL string `yaml:"-"`
	ResolvedConfigID        string `yaml:"-"`
	ResolvedWorkloadKey     string `yaml:"-"`
}

// --- Core fields ---

type PlatformConfig struct {
	GCPProject           string   `yaml:"gcp_project"`
	Region               string   `yaml:"region"`
	Zone                 string   `yaml:"zone"`
	Environment          string   `yaml:"environment"`
	ControlPlaneDomain   string   `yaml:"control_plane_domain"`
	TerraformStateBucket string   `yaml:"terraform_state_bucket"`
	TerraformStatePrefix string   `yaml:"terraform_state_prefix"`
	DBPassword           string   `yaml:"db_password"`
	AdminCIDRs           []string `yaml:"admin_cidrs"`
}

type MicroVMConfig struct {
	VCPUs      int `yaml:"vcpus"`
	MemoryMB   int `yaml:"memory_mb"`
	IdleTarget int `yaml:"idle_target"`
}

type HostsConfig struct {
	MachineType      string `yaml:"machine_type"`
	MinCount         int    `yaml:"min_count"`
	MaxCount         int    `yaml:"max_count"`
	DataDiskGB       int    `yaml:"data_disk_gb"`
	ChunkCacheSizeGB int    `yaml:"chunk_cache_size_gb"`
	MemCacheSizeGB   int    `yaml:"mem_cache_size_gb"`
}

// --- Workload: what runs inside the VM ---

// WorkloadConfig describes the golden snapshot content and the service the VM runs
// after restore.
type WorkloadConfig struct {
	// SnapshotCommands lists warmup steps baked into the golden snapshot during build.
	// Valid types: "shell", "gcp-auth", "exec" (see examples/README.md for args).
	SnapshotCommands []SnapshotCommandConfig `yaml:"snapshot_commands"`

	// Layered workload schema used by the examples and control-plane API.
	BaseImage string                `yaml:"base_image"`
	Layers    []snapshot.LayerDef   `yaml:"layers"`
	Config    WorkloadRuntimeConfig `yaml:"config"`

	// StartCommand describes the service to launch inside the VM after restore.
	// The capsule-thaw-agent starts Command, waits for GET HealthPath on Port to return 2xx,
	// then signals host readiness.
	StartCommand StartCommandConfig `yaml:"start_command"`
}

type WorkloadRuntimeConfig struct {
	AutoPause            *bool               `yaml:"auto_pause"`
	TTL                  int                 `yaml:"ttl"`
	Tier                 string              `yaml:"tier"`
	AutoRollout          *bool               `yaml:"auto_rollout"`
	SessionMaxAgeSeconds int                 `yaml:"session_max_age_seconds"`
	RootfsSizeGB         int                 `yaml:"rootfs_size_gb"`
	RunnerUser           string              `yaml:"runner_user"`
	WorkspaceSizeGB      int                 `yaml:"workspace_size_gb"`
	NetworkPolicyPreset  string              `yaml:"network_policy_preset"`
	NetworkPolicy        json.RawMessage     `yaml:"network_policy"`
	Auth                 *accessplane.Config `yaml:"auth"`
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
	Command    []string          `yaml:"command"`
	Port       int               `yaml:"port"`
	HealthPath string            `yaml:"health_path"`
	Env        map[string]string `yaml:"env"`
	RunAs      string            `yaml:"run_as"`
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
	if cfg.Platform.Environment == "" {
		cfg.Platform.Environment = "dev"
	}
	if cfg.MicroVM.VCPUs == 0 {
		cfg.MicroVM.VCPUs = 4
	}
	if cfg.MicroVM.MemoryMB == 0 {
		cfg.MicroVM.MemoryMB = 8192
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
	if cfg.Hosts.ChunkCacheSizeGB == 0 {
		cfg.Hosts.ChunkCacheSizeGB = 8
	}
	if cfg.Hosts.MemCacheSizeGB == 0 {
		cfg.Hosts.MemCacheSizeGB = 8
	}

	return &cfg, nil
}

// Validate checks the configuration for required fields and consistency.
func (c *Config) Validate() error {
	if c.Platform.GCPProject == "" {
		return fmt.Errorf("platform.gcp_project is required")
	}
	if len(c.Workload.Layers) == 0 && len(c.Workload.SnapshotCommands) == 0 {
		return fmt.Errorf("workload must define either workload.layers or workload.snapshot_commands")
	}
	if len(c.Credentials.Secrets) > 0 || len(c.Credentials.HostDirs) > 0 || len(c.Credentials.Env) > 0 {
		return fmt.Errorf("cmd/onboard does not yet translate the top-level credentials block into runtime drives or Secret Manager mounts; express credentials inside workload layers/drives for now")
	}

	// Check toolchain availability.
	for _, tool := range []string{"gcloud", "terraform", "packer", "docker", "helm", "kubectl", "python3"} {
		if _, err := lookPath(tool); err != nil {
			return fmt.Errorf("required tool not found: %s", tool)
		}
	}

	return nil
}
