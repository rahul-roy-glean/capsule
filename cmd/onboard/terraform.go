package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

func stepTerraformBootstrap(cfg *Config, logger *logrus.Logger, planOnly bool) error {
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

	if planOnly {
		log.Info("Running terraform plan (bootstrap mode - stock images)...")
		planCmd := exec.Command("terraform", "plan",
			fmt.Sprintf("-var-file=%s", tfvarsPath),
			"-out=tfplan-bootstrap")
		planCmd.Dir = tfDir
		planCmd.Stdout = os.Stdout
		planCmd.Stderr = os.Stderr
		if err := planCmd.Run(); err != nil {
			return fmt.Errorf("terraform plan failed: %w", err)
		}
		return nil
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

func stepTerraformFinalize(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "terraform-finalize")

	tfvarsPath, err := generateTFVars(cfg, true)
	if err != nil {
		return fmt.Errorf("failed to generate tfvars: %w", err)
	}
	log.WithField("tfvars", tfvarsPath).Info("Generated terraform.tfvars (finalize mode)")

	tfDir := "deploy/terraform"

	if planOnly {
		log.Info("Running terraform plan (finalize mode - custom images)...")
		planCmd := exec.Command("terraform", "plan",
			fmt.Sprintf("-var-file=%s", tfvarsPath),
			"-out=tfplan-finalize")
		planCmd.Dir = tfDir
		planCmd.Stdout = os.Stdout
		planCmd.Stderr = os.Stderr
		if err := planCmd.Run(); err != nil {
			return fmt.Errorf("terraform plan failed: %w", err)
		}
		return nil
	}

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

	// --- Finalize mode ---
	addBool("use_custom_host_image", finalize)
	addBool("use_data_snapshot", finalize)

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(tfvarsPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write tfvars: %w", err)
	}

	return tfvarsPath, nil
}
