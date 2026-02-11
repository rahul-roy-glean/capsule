package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/sirupsen/logrus"
)

func stepSnapshotBuild(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "snapshot-build")

	// Build snapshot-builder binary
	log.Info("Building snapshot-builder...")
	buildCmd := exec.Command("make", "snapshot-builder")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("failed to build snapshot-builder: %w", err)
	}

	// Run snapshot-builder
	// In production this runs on a GCE VM with Firecracker, but the CLI orchestrates it
	log.Info("Running snapshot build (this runs on a GCE VM with Firecracker)...")
	log.Info("NOTE: Ensure you are running this on a GCE VM with nested virtualization enabled")

	gcsBucket := fmt.Sprintf("%s-firecracker-snapshots", cfg.Platform.GCPProject)

	args := []string{
		"--repo-url", cfg.Repository.URL,
		"--repo-branch", cfg.Repository.Branch,
		"--gcs-bucket", gcsBucket,
	}

	if cfg.Repository.GitHubAppID != "" {
		args = append(args,
			"--github-app-id", cfg.Repository.GitHubAppID,
			"--github-app-secret", cfg.Repository.GitHubAppSecretName,
			"--gcp-project", cfg.Platform.GCPProject,
		)
	}

	snapshotCmd := exec.Command("./bin/snapshot-builder", args...)
	snapshotCmd.Stdout = os.Stdout
	snapshotCmd.Stderr = os.Stderr
	if err := snapshotCmd.Run(); err != nil {
		return fmt.Errorf("snapshot build failed: %w", err)
	}

	return nil
}

func stepDataSnapshotBuild(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "data-snapshot-build")

	// Build data-snapshot-builder binary
	log.Info("Building data-snapshot-builder...")
	buildCmd := exec.Command("make", "data-snapshot-builder")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("failed to build data-snapshot-builder: %w", err)
	}

	// Run data snapshot build
	log.Info("Creating GCP disk snapshot...")
	gcsBucket := fmt.Sprintf("%s-firecracker-snapshots", cfg.Platform.GCPProject)

	args := []string{
		"--project", cfg.Platform.GCPProject,
		"--zone", cfg.Platform.Zone,
		"--snapshot-gcs", fmt.Sprintf("gs://%s/current/", gcsBucket),
		"--metadata-bucket", gcsBucket,
	}

	dataSnapshotCmd := exec.Command("./bin/data-snapshot-builder", args...)
	dataSnapshotCmd.Stdout = os.Stdout
	dataSnapshotCmd.Stderr = os.Stderr
	if err := dataSnapshotCmd.Run(); err != nil {
		return fmt.Errorf("data snapshot build failed: %w", err)
	}

	return nil
}
