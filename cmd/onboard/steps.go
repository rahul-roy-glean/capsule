package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/sirupsen/logrus"
)

// Step represents an onboard step.
type Step struct {
	Name        string
	Description string
	Run         func(cfg *Config, logger *logrus.Logger) error
}

// GetAllSteps returns all onboard steps in order.
func GetAllSteps() []Step {
	return []Step{
		{
			Name:        "validate",
			Description: "Validate configuration and check prerequisites",
			Run:         stepValidate,
		},
		{
			Name:        "terraform-bootstrap",
			Description: "Bootstrap infrastructure with Terraform (stock images)",
			Run:         stepTerraformBootstrap,
		},
		{
			Name:        "packer-build",
			Description: "Build custom GCE host image with Packer",
			Run:         stepPackerBuild,
		},
		{
			Name:        "snapshot-build",
			Description: "Build Firecracker snapshot with warmed Bazel",
			Run:         stepSnapshotBuild,
		},
		{
			Name:        "data-snapshot-build",
			Description: "Create GCP disk snapshot with all artifacts",
			Run:         stepDataSnapshotBuild,
		},
		{
			Name:        "terraform-finalize",
			Description: "Finalize infrastructure with custom images",
			Run:         stepTerraformFinalize,
		},
		{
			Name:        "control-plane-deploy",
			Description: "Deploy control plane to GKE",
			Run:         stepControlPlaneDeploy,
		},
		{
			Name:        "verify",
			Description: "Verify deployment health",
			Run:         stepVerify,
		},
	}
}

// GetStepByName finds a step by name.
func GetStepByName(steps []Step, name string) (Step, bool) {
	for _, s := range steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

func stepValidate(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "validate")

	// Check GCP access
	log.Info("Checking GCP access...")
	cmd := exec.Command("gcloud", "projects", "describe", cfg.Platform.GCPProject, "--format=value(projectId)")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot access GCP project %s: %w\nOutput: %s", cfg.Platform.GCPProject, err, string(output))
	}
	log.Info("GCP access verified")

	// Check optional tools
	for _, tool := range []string{"packer", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			log.WithField("tool", tool).Warn("Optional tool not found (may be needed for later steps)")
		}
	}

	return nil
}

func stepPackerBuild(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "packer-build")

	// Cross-compile firecracker-manager for linux
	log.Info("Building firecracker-manager for linux/amd64...")
	buildCmd := exec.Command("make", "firecracker-manager-linux", fmt.Sprintf("PROJECT_ID=%s", cfg.Platform.GCPProject))
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("failed to build firecracker-manager: %w", err)
	}

	// Run packer build
	log.Info("Building GCE host image with Packer...")
	packerCmd := exec.Command("make", "packer-build", fmt.Sprintf("PROJECT_ID=%s", cfg.Platform.GCPProject))
	packerCmd.Stdout = os.Stdout
	packerCmd.Stderr = os.Stderr
	if err := packerCmd.Run(); err != nil {
		return fmt.Errorf("packer build failed: %w", err)
	}

	return nil
}

func stepControlPlaneDeploy(cfg *Config, logger *logrus.Logger) error {
	log := logger.WithField("step", "control-plane-deploy")

	log.Info("Building control plane Docker image...")
	buildCmd := exec.Command("make", "docker-build-control-plane",
		fmt.Sprintf("PROJECT_ID=%s", cfg.Platform.GCPProject))
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("failed to build control plane image: %w", err)
	}

	log.Info("Pushing control plane Docker image...")
	pushCmd := exec.Command("make", "docker-push",
		fmt.Sprintf("PROJECT_ID=%s", cfg.Platform.GCPProject))
	pushCmd.Stdout = os.Stdout
	pushCmd.Stderr = os.Stderr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("failed to push control plane image: %w", err)
	}

	log.Info("Deploying to GKE...")
	deployCmd := exec.Command("make", "k8s-deploy")
	deployCmd.Stdout = os.Stdout
	deployCmd.Stderr = os.Stderr
	if err := deployCmd.Run(); err != nil {
		return fmt.Errorf("failed to deploy to GKE: %w", err)
	}

	return nil
}
