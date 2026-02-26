package main

import (
	"encoding/json"
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

	log.Info("Running snapshot build (this runs on a GCE VM with Firecracker)...")
	log.Info("NOTE: Ensure you are running this on a GCE VM with nested virtualization enabled")

	gcsBucket := fmt.Sprintf("%s-firecracker-snapshots", cfg.Platform.GCPProject)

	// Build the --snapshot-commands JSON from config.
	snapshotCmds, err := buildSnapshotCommands(cfg)
	if err != nil {
		return fmt.Errorf("failed to build snapshot commands: %w", err)
	}
	cmdJSON, err := json.Marshal(snapshotCmds)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot commands: %w", err)
	}

	args := []string{
		"--gcs-bucket", gcsBucket,
		"--snapshot-commands", string(cmdJSON),
	}

	// For non-CI workloads, pass the start command and microVM sizing.
	if cfg.CI.System == "none" {
		sc := cfg.Workload.StartCommand
		startCmdJSON, err := json.Marshal(map[string]any{
			"command":     sc.Command,
			"port":        sc.Port,
			"health_path": sc.HealthPath,
		})
		if err != nil {
			return fmt.Errorf("failed to marshal start command: %w", err)
		}
		args = append(args, "--start-command", string(startCmdJSON))
	}

	// Pass GitHub App credentials if configured (for private repo cloning).
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

// buildSnapshotCommands converts the onboard config into a []SnapshotCommandConfig
// suitable for passing to snapshot-builder as --snapshot-commands JSON.
func buildSnapshotCommands(cfg *Config) ([]SnapshotCommandConfig, error) {
	switch cfg.CI.System {
	case "github-actions":
		// For CI, synthesize the standard warmup: clone the repo, then run bazel fetch.
		cmds := []SnapshotCommandConfig{
			{
				Type: "git-clone",
				Args: []string{cfg.Repository.URL, cfg.Repository.Branch},
			},
		}
		if cfg.Bazel.WarmupTargets != "" {
			cmds = append(cmds, SnapshotCommandConfig{
				Type: "shell",
				Args: []string{"bazel", "fetch", cfg.Bazel.WarmupTargets},
			})
		}
		return cmds, nil

	case "none":
		if len(cfg.Workload.SnapshotCommands) == 0 {
			// No snapshot commands is valid — the golden snapshot will be a bare VM.
			return nil, nil
		}
		return cfg.Workload.SnapshotCommands, nil

	default:
		return nil, fmt.Errorf("unsupported ci.system: %s", cfg.CI.System)
	}
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
		"--snapshot-gcs", fmt.Sprintf("gs://%s/v1/build-artifacts/", gcsBucket),
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
