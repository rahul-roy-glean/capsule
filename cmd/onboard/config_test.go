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
	if cfg.MicroVM.VCPUs != 8 {
		t.Errorf("VCPUs = %d, want %d", cfg.MicroVM.VCPUs, 8)
	}
	if cfg.MicroVM.MemoryMB != 16384 {
		t.Errorf("MemoryMB = %d, want %d", cfg.MicroVM.MemoryMB, 16384)
	}
	if cfg.Hosts.MachineType != "n2-standard-128" {
		t.Errorf("MachineType = %q, want %q", cfg.Hosts.MachineType, "n2-standard-128")
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
