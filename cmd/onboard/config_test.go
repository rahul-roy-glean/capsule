package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
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
	if cfg.MicroVM.VCPUs != 8 {
		t.Errorf("VCPUs = %d, want %d", cfg.MicroVM.VCPUs, 8)
	}
	if cfg.MicroVM.MemoryMB != 16384 {
		t.Errorf("MemoryMB = %d, want %d", cfg.MicroVM.MemoryMB, 16384)
	}
	if cfg.Hosts.MachineType != "n2-standard-128" {
		t.Errorf("MachineType = %q, want %q", cfg.Hosts.MachineType, "n2-standard-128")
	}
	if cfg.Platform.ControlPlaneDomain != "cp.example.com" {
		t.Errorf("ControlPlaneDomain = %q, want %q", cfg.Platform.ControlPlaneDomain, "cp.example.com")
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
}

func TestLoadConfig_MinimalValid(t *testing.T) {
	cfg, err := LoadConfig("testdata/valid_minimal.yaml")
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Platform.GCPProject != "minimal-project" {
		t.Errorf("GCPProject = %q, want %q", cfg.Platform.GCPProject, "minimal-project")
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
	cfg := &Config{}
	err := cfg.Validate()
	if err == nil {
		t.Error("Validate() expected error for missing project")
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		Workload: WorkloadConfig{
			Layers: []snapshot.LayerDef{{
				Name: "base",
				InitCommands: []snapshot.SnapshotCommand{{
					Type: "shell",
					Args: []string{"echo", "hi"},
				}},
			}},
		},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v for valid config", err)
	}
}

func TestValidate_ToolCheck(t *testing.T) {
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		Workload: WorkloadConfig{
			Layers: []snapshot.LayerDef{{
				Name: "base",
				InitCommands: []snapshot.SnapshotCommand{{
					Type: "shell",
					Args: []string{"echo", "hi"},
				}},
			}},
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

func TestValidate_LayeredWorkloadSchemaAccepted(t *testing.T) {
	dir := t.TempDir()
	content := []byte(`
platform:
  gcp_project: test-project
workload:
  base_image: ubuntu:22.04
  layers:
    - name: base
      init_commands:
        - type: shell
          args: ["echo", "hi"]
`)
	path := filepath.Join(dir, "layered.yaml")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
}

func TestLoadConfig_ZoneDefaultFromRegion(t *testing.T) {
	// Create a temp config with custom region but no zone
	dir := t.TempDir()
	content := []byte("platform:\n  gcp_project: test\n  region: europe-west1\n")
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

func TestToLayeredConfig_LegacySnapshotCommands(t *testing.T) {
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "proj", Environment: "dev"},
		Workload: WorkloadConfig{
			SnapshotCommands: []SnapshotCommandConfig{{
				Type: "shell",
				Args: []string{"echo", "hello"},
			}},
			StartCommand: StartCommandConfig{
				Command:    []string{"python3", "-m", "http.server", "8080"},
				Port:       8080,
				HealthPath: "/",
			},
		},
		Session: SessionConfig{
			Enabled:    true,
			TTLSeconds: 300,
			AutoPause:  true,
		},
	}

	layered := cfg.ToLayeredConfig()
	if len(layered.Layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layered.Layers))
	}
	if layered.Layers[0].Name != "workload" {
		t.Fatalf("layer name = %q, want workload", layered.Layers[0].Name)
	}
	if len(layered.Layers[0].InitCommands) != 1 || layered.Layers[0].InitCommands[0].Type != "shell" {
		t.Fatalf("unexpected init commands: %+v", layered.Layers[0].InitCommands)
	}
	if layered.Config.TTL != 300 || !layered.Config.AutoPause {
		t.Fatalf("unexpected session-derived config: ttl=%d auto_pause=%v", layered.Config.TTL, layered.Config.AutoPause)
	}
	if layered.StartCommand == nil || layered.StartCommand.Port != 8080 {
		t.Fatalf("unexpected start command: %+v", layered.StartCommand)
	}
}

func TestToLayeredConfig_ModernSchema(t *testing.T) {
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "proj", Environment: "prod"},
		Workload: WorkloadConfig{
			BaseImage: "ubuntu:22.04",
			Layers: []snapshot.LayerDef{{
				Name: "base",
				InitCommands: []snapshot.SnapshotCommand{{
					Type: "shell",
					Args: []string{"echo", "hi"},
				}},
			}},
			Config: WorkloadRuntimeConfig{
				Tier:                 "xs",
				RunnerUser:           "user",
				WorkspaceSizeGB:      10,
				SessionMaxAgeSeconds: 86400,
			},
		},
	}
	autoRollout := true
	autoPause := true
	cfg.Workload.Config.AutoRollout = &autoRollout
	cfg.Workload.Config.AutoPause = &autoPause

	layered := cfg.ToLayeredConfig()
	if layered.BaseImage != "ubuntu:22.04" {
		t.Fatalf("base image = %q", layered.BaseImage)
	}
	if len(layered.Layers) != 1 || layered.Layers[0].Name != "base" {
		t.Fatalf("unexpected layers: %+v", layered.Layers)
	}
	if layered.Config.Tier != "xs" || layered.Config.RunnerUser != "user" {
		t.Fatalf("unexpected layered config runtime fields: %+v", layered.Config)
	}
}

func TestValidate_CredentialsWrapperRejected(t *testing.T) {
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "my-project"},
		Workload: WorkloadConfig{
			Layers: []snapshot.LayerDef{{
				Name: "base",
				InitCommands: []snapshot.SnapshotCommand{{
					Type: "shell",
					Args: []string{"echo", "hi"},
				}},
			}},
		},
		Credentials: CredentialsConfig{
			Secrets: []SecretRef{{
				Name:       "api-key",
				SecretName: "projects/x/secrets/y/versions/latest",
				Target:     "api.key",
			}},
		},
	}
	origLookPath := lookPath
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	defer func() { lookPath = origLookPath }()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() expected credentials wrapper rejection")
	}
}
