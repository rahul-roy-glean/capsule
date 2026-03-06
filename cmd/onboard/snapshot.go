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
	cmdJSON, err := json.Marshal(cfg.Workload.SnapshotCommands)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot commands: %w", err)
	}

	args := []string{
		"--gcs-bucket", gcsBucket,
		"--snapshot-commands", string(cmdJSON),
	}

	// Pass the start command and microVM sizing.
	if len(cfg.Workload.StartCommand.Command) > 0 {
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

	snapshotCmd := exec.Command("./bin/snapshot-builder", args...)
	snapshotCmd.Stdout = os.Stdout
	snapshotCmd.Stderr = os.Stderr
	if err := snapshotCmd.Run(); err != nil {
		return fmt.Errorf("snapshot build failed: %w", err)
	}

	return nil
}
