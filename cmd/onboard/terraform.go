package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

func stepTerraformBootstrap(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "terraform-bootstrap")

	tfvarsPath, err := generateTFVars(cfg, false)
	if err != nil {
		return fmt.Errorf("failed to generate tfvars: %w", err)
	}
	log.WithField("tfvars", tfvarsPath).Info("Generated terraform.tfvars")

	tfDir := "deploy/terraform"

	log.Info("Running terraform init...")
	initCmd := exec.Command("terraform", "init")
	initCmd.Dir = tfDir
	initCmd.Stdout = os.Stdout
	initCmd.Stderr = os.Stderr
	if err := initCmd.Run(); err != nil {
		return fmt.Errorf("terraform init failed: %w", err)
	}

	log.Info("Running terraform apply (bootstrap mode - stock images)...")
	applyCmd := exec.Command("terraform", "apply",
		"-auto-approve",
		fmt.Sprintf("-var-file=%s", tfvarsPath))
	applyCmd.Dir = tfDir
	applyCmd.Stdout = os.Stdout
	applyCmd.Stderr = os.Stderr
	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("terraform apply failed: %w", err)
	}

	return nil
}

func stepTerraformFinalize(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "terraform-finalize")

	tfvarsPath, err := generateTFVars(cfg, true)
	if err != nil {
		return fmt.Errorf("failed to generate tfvars: %w", err)
	}
	log.WithField("tfvars", tfvarsPath).Info("Generated terraform.tfvars (finalize mode)")

	tfDir := "deploy/terraform"

	log.Info("Running terraform apply (finalize mode - custom images)...")
	applyCmd := exec.Command("terraform", "apply",
		"-auto-approve",
		fmt.Sprintf("-var-file=%s", tfvarsPath))
	applyCmd.Dir = tfDir
	applyCmd.Stdout = os.Stdout
	applyCmd.Stderr = os.Stderr
	if err := applyCmd.Run(); err != nil {
		return fmt.Errorf("terraform apply failed: %w", err)
	}

	return nil
}

// generateTFVars creates a terraform.auto.tfvars file from the onboard config.
// If targetDir is empty it defaults to "deploy/terraform".
func generateTFVars(cfg *Config, finalize bool, targetDir ...string) (string, error) {
	tfDir := "deploy/terraform"
	if len(targetDir) > 0 && targetDir[0] != "" {
		tfDir = targetDir[0]
	}
	tfvarsPath := filepath.Join(tfDir, "terraform.auto.tfvars")

	var lines []string
	addStr := func(key, value string) {
		lines = append(lines, fmt.Sprintf("%s = %q", key, value))
	}
	addBool := func(key string, value bool) {
		lines = append(lines, fmt.Sprintf("%s = %v", key, value))
	}
	addInt := func(key string, value int) {
		lines = append(lines, fmt.Sprintf("%s = %d", key, value))
	}
	addMap := func(key string, m map[string]string) {
		// Terraform HCL map literal.
		if len(m) == 0 {
			lines = append(lines, fmt.Sprintf("%s = {}", key))
			return
		}
		// Sort keys for deterministic output.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var pairs []string
		for _, k := range keys {
			pairs = append(pairs, fmt.Sprintf("  %q = %q", k, m[k]))
		}
		lines = append(lines, fmt.Sprintf("%s = {\n%s\n}", key, strings.Join(pairs, "\n")))
	}

	// --- Platform ---
	addStr("project_id", cfg.Platform.GCPProject)
	addStr("region", cfg.Platform.Region)
	addStr("zone", cfg.Platform.Zone)

	// --- Hosts ---
	addStr("host_machine_type", cfg.Hosts.MachineType)
	addInt("min_hosts", cfg.Hosts.MinCount)
	addInt("max_hosts", cfg.Hosts.MaxCount)
	addInt("host_data_disk_size_gb", cfg.Hosts.DataDiskGB)

	// --- MicroVMs ---
	addInt("max_runners_per_host", cfg.MicroVM.MaxPerHost)
	addInt("idle_runners_target", cfg.MicroVM.IdleTarget)

	// CI system
	addStr("ci_system", cfg.CI.System)

	// GitHub config
	if cfg.CI.System == "github-actions" {
		addBool("github_runner_enabled", true)
		if cfg.CI.GitHub.Repo != "" {
			addStr("github_repo", cfg.CI.GitHub.Repo)
		}
		if cfg.CI.GitHub.Org != "" {
			addStr("github_org", cfg.CI.GitHub.Org)
		}
		if len(cfg.CI.GitHub.Labels) > 0 {
			addStr("github_runner_labels", strings.Join(cfg.CI.GitHub.Labels, ","))
		}
		addBool("runner_ephemeral", cfg.CI.GitHub.Ephemeral)
	}

	// --- Repository (GitHub App auth for private repos) ---
	if cfg.Repository.GitHubAppID != "" {
		addStr("github_app_id", cfg.Repository.GitHubAppID)
	}
	if cfg.Repository.GitHubAppSecretName != "" {
		addStr("github_app_secret", cfg.Repository.GitHubAppSecretName)
	}

	// --- Bazel add-on ---
	// repo_cache_upper_size_gb: emit whenever non-default, or for github-actions.
	if cfg.CI.System == "github-actions" || cfg.Bazel.RepoCacheUpperSizeGB != 10 {
		addInt("repo_cache_upper_size_gb", cfg.Bazel.RepoCacheUpperSizeGB)
	}

	// Git cache (Bazel sub-feature).
	addBool("git_cache_enabled", cfg.Bazel.GitCache.Enabled)
	if cfg.Bazel.GitCache.Enabled {
		addMap("git_cache_repos", cfg.Bazel.GitCache.Repos)
		if cfg.Bazel.GitCache.WorkspaceDir != "" {
			addStr("git_cache_workspace_dir", cfg.Bazel.GitCache.WorkspaceDir)
		}
	}

	// Buildbarn (Bazel sub-feature).
	if cfg.Bazel.Buildbarn.CertsDir != "" {
		addStr("buildbarn_certs_dir", cfg.Bazel.Buildbarn.CertsDir)
	}

	// --- Finalize mode ---
	addBool("use_custom_host_image", finalize)
	addBool("use_data_snapshot", finalize)

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(tfvarsPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write tfvars: %w", err)
	}

	return tfvarsPath, nil
}
