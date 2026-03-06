package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateTFVars_Bootstrap(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "test-project", Region: "us-central1", Zone: "us-central1-a"},
		Hosts:    HostsConfig{MachineType: "n2-standard-64", MinCount: 2, MaxCount: 20, DataDiskGB: 500},
		MicroVM:  MicroVMConfig{MaxPerHost: 16, IdleTarget: 2, VCPUs: 4, MemoryMB: 8192},
	}

	path, err := generateTFVars(cfg, false, dir)
	if err != nil {
		t.Fatalf("generateTFVars() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	checks := []struct {
		label    string
		contains string
	}{
		{"project_id", `project_id = "test-project"`},
		{"region", `region = "us-central1"`},
		{"min_hosts", `min_hosts = 2`},
		{"max_hosts", `max_hosts = 20`},
		{"use_custom_host_image false", `use_custom_host_image = false`},
		{"use_data_snapshot false", `use_data_snapshot = false`},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.contains) {
			t.Errorf("bootstrap tfvars missing %s: expected to contain %q", c.label, c.contains)
		}
	}

	// Ensure deleted CI/Bazel variables are not emitted.
	for _, removed := range []string{"ci_system", "github_runner_enabled", "github_repo", "github_org",
		"github_app_id", "github_app_secret", "github_runner_labels",
		"repo_cache_upper_size_gb", "git_cache_enabled", "git_cache_repos",
		"git_cache_workspace_dir", "buildbarn_certs_dir"} {
		if strings.Contains(content, removed) {
			t.Errorf("bootstrap tfvars should not contain %q", removed)
		}
	}
}

func TestGenerateTFVars_Finalize(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "test-project", Region: "us-central1", Zone: "us-central1-a"},
		Hosts:    HostsConfig{MachineType: "n2-standard-64", MinCount: 2, MaxCount: 20, DataDiskGB: 500},
		MicroVM:  MicroVMConfig{MaxPerHost: 16, IdleTarget: 2, VCPUs: 4, MemoryMB: 8192},
	}

	path, err := generateTFVars(cfg, true, dir)
	if err != nil {
		t.Fatalf("generateTFVars() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "use_custom_host_image = true") {
		t.Error("finalize tfvars should have use_custom_host_image = true")
	}
	if !strings.Contains(content, "use_data_snapshot = true") {
		t.Error("finalize tfvars should have use_data_snapshot = true")
	}
}

func TestGenerateTFVars_AllFields(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "proj", Region: "eu-west1", Zone: "eu-west1-c"},
		Hosts:    HostsConfig{MachineType: "c2-standard-60", MinCount: 3, MaxCount: 10, DataDiskGB: 200},
		MicroVM:  MicroVMConfig{MaxPerHost: 8, IdleTarget: 1, VCPUs: 2, MemoryMB: 4096},
	}

	path, err := generateTFVars(cfg, true, dir)
	if err != nil {
		t.Fatalf("generateTFVars() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	checks := []struct {
		label    string
		contains string
	}{
		{"project_id", `project_id = "proj"`},
		{"region", `region = "eu-west1"`},
		{"zone", `zone = "eu-west1-c"`},
		{"host_machine_type", `host_machine_type = "c2-standard-60"`},
		{"min_hosts", `min_hosts = 3`},
		{"max_hosts", `max_hosts = 10`},
		{"host_data_disk_size_gb", `host_data_disk_size_gb = 200`},
		{"max_runners_per_host", `max_runners_per_host = 8`},
		{"idle_runners_target", `idle_runners_target = 1`},
		{"use_custom_host_image", `use_custom_host_image = true`},
		{"use_data_snapshot", `use_data_snapshot = true`},
	}
	for _, c := range checks {
		if !strings.Contains(content, c.contains) {
			t.Errorf("all-fields tfvars missing %s: expected %q\ncontent:\n%s", c.label, c.contains, content)
		}
	}
}

func TestGenerateTFVars_DefaultDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Platform: PlatformConfig{GCPProject: "test"},
		Hosts:    HostsConfig{MachineType: "n2-standard-64", MinCount: 1, MaxCount: 1, DataDiskGB: 100},
		MicroVM:  MicroVMConfig{MaxPerHost: 1, IdleTarget: 1, VCPUs: 1, MemoryMB: 1024},
	}

	path, err := generateTFVars(cfg, false, dir)
	if err != nil {
		t.Fatalf("generateTFVars() error = %v", err)
	}

	if filepath.Base(path) != "terraform.auto.tfvars" {
		t.Errorf("expected filename terraform.auto.tfvars, got %q", filepath.Base(path))
	}
}
