package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Step represents an onboard step.
type Step struct {
	Name        string
	Description string
	Run         func(cfg *Config, logger *logrus.Logger, planOnly bool) error
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
			Name:        "control-plane-deploy",
			Description: "Deploy control plane to GKE",
			Run:         stepControlPlaneDeploy,
		},
		{
			Name:        "snapshot-build",
			Description: "Stage builder artifacts and build workload snapshot",
			Run:         stepSnapshotBuild,
		},
		{
			Name:        "terraform-finalize",
			Description: "Finalize infrastructure with custom images",
			Run:         stepTerraformFinalize,
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

func stepValidate(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "validate")

	// Check GCP access
	log.Info("Checking GCP access...")
	cmd := exec.Command("gcloud", "projects", "describe", cfg.Platform.GCPProject, "--format=value(projectId)")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot access GCP project %s: %w\nOutput: %s", cfg.Platform.GCPProject, err, string(output))
	}
	log.Info("GCP access verified")

	return nil
}

func stepPackerBuild(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "packer-build")

	if planOnly {
		fmt.Println("\n[plan] packer-build:")
		fmt.Printf("  GCP project:    %s\n", cfg.Platform.GCPProject)
		fmt.Printf("  Image family:   firecracker-host\n")
		fmt.Printf("  Source image:   ubuntu-2204-lts (ubuntu-os-cloud)\n")
		fmt.Printf("  Machine type:   n2-standard-4\n")
		fmt.Printf("  Binary:         bin/firecracker-manager (linux/amd64)\n")
		fmt.Printf("  Provisioners:   7 shell, 1 file upload\n")
		return nil
	}

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

func stepControlPlaneDeploy(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "control-plane-deploy")

	if planOnly {
		fmt.Println("\n[plan] control-plane-deploy:")
		fmt.Printf("  Image:          %s-docker.pkg.dev/%s/firecracker/firecracker-control-plane:latest\n",
			cfg.Platform.Region, cfg.Platform.GCPProject)
		fmt.Printf("  K8s deployer:   Helm chart deploy/helm/firecracker-runner\n")
		fmt.Printf("  Hosts during deploy: none (bootstrap min_hosts forced to 0)\n")
		if cfg.Platform.ControlPlaneDomain != "" {
			fmt.Printf("  Domain:         %s (Ingress included)\n", cfg.Platform.ControlPlaneDomain)
		} else {
			fmt.Printf("  Domain:         internal LoadBalancer only\n")
		}
		fmt.Printf("  Namespace:      firecracker-runner\n")
		return nil
	}

	clusterName, err := terraformOutputRaw("gke_cluster_name")
	if err != nil {
		return err
	}
	dbPrivateIP, err := terraformOutputRaw("db_private_ip")
	if err != nil {
		return err
	}
	controlPlaneGSA, err := terraformOutputRaw("control_plane_service_account")
	if err != nil {
		return err
	}
	builderGSA, err := terraformOutputRaw("snapshot_builder_service_account")
	if err != nil {
		return err
	}
	hostMigName, err := terraformOutputRaw("host_instance_group_manager_name")
	if err != nil {
		return err
	}
	hostAutoscalerName, err := terraformOutputRaw("host_autoscaler_name")
	if err != nil {
		return err
	}
	vpcNetwork, err := terraformOutputRaw("vpc_network")
	if err != nil {
		return err
	}

	log.Info("Fetching GKE credentials...")
	getCredsCmd := exec.Command("gcloud", "container", "clusters", "get-credentials", clusterName,
		"--region="+cfg.Platform.Region,
		"--project="+cfg.Platform.GCPProject,
	)
	if err := runCommandStreaming(getCredsCmd); err != nil {
		return fmt.Errorf("failed to get GKE credentials: %w", err)
	}

	log.Info("Building control plane Docker image...")
	buildCmd := exec.Command("make", "docker-build-control-plane",
		fmt.Sprintf("PROJECT_ID=%s", cfg.Platform.GCPProject),
		fmt.Sprintf("REGION=%s", cfg.Platform.Region))
	if err := runCommandStreaming(buildCmd); err != nil {
		return fmt.Errorf("failed to build control plane image: %w", err)
	}

	log.Info("Pushing control plane Docker image...")
	pushCmd := exec.Command("make", "docker-push-control-plane",
		fmt.Sprintf("PROJECT_ID=%s", cfg.Platform.GCPProject),
		fmt.Sprintf("REGION=%s", cfg.Platform.Region))
	if err := runCommandStreaming(pushCmd); err != nil {
		return fmt.Errorf("failed to push control plane image: %w", err)
	}

	log.Info("Ensuring namespace exists...")
	nsCmd := exec.Command("kubectl", "create", "namespace", onboardNamespace, "--dry-run=client", "-o", "yaml")
	nsOut, err := nsCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to template namespace: %w", err)
	}
	applyNSCmd := exec.Command("kubectl", "apply", "-f", "-")
	applyNSCmd.Stdin = strings.NewReader(string(nsOut))
	if err := runCommandStreaming(applyNSCmd); err != nil {
		return fmt.Errorf("failed to apply namespace: %w", err)
	}

	webhookSecret := cfg.Platform.GitHubWebhookSecret
	if webhookSecret == "" {
		webhookSecret = "unused-for-generic-workloads"
	}

	if err := applySecret("db-credentials", map[string]string{
		"host":     dbPrivateIP,
		"username": "postgres",
		"password": cfg.ResolvedDBPassword,
	}); err != nil {
		return err
	}
	if err := applySecret("github-credentials", map[string]string{
		"webhook_secret": webhookSecret,
	}); err != nil {
		return err
	}

	log.Info("Deploying control plane Helm chart...")
	helmArgs := []string{
		"upgrade", "--install", "control-plane", "deploy/helm/firecracker-runner",
		"--namespace", onboardNamespace,
		"--set", fmt.Sprintf("image.repository=%s-docker.pkg.dev/%s/firecracker/firecracker-control-plane", cfg.Platform.Region, cfg.Platform.GCPProject),
		"--set", "image.tag=latest",
		"--set", fmt.Sprintf("config.gcsBucket=%s-firecracker-snapshots", cfg.Platform.GCPProject),
		"--set", fmt.Sprintf("config.gcpProject=%s", cfg.Platform.GCPProject),
		"--set", fmt.Sprintf("config.environment=%s", cfg.Platform.Environment),
		"--set", fmt.Sprintf("gcp.projectId=%s", cfg.Platform.GCPProject),
		"--set", fmt.Sprintf("gcp.region=%s", cfg.Platform.Region),
		"--set", fmt.Sprintf("gcp.zone=%s", cfg.Platform.Zone),
		"--set", fmt.Sprintf("gcp.hostMigName=%s", hostMigName),
		"--set", fmt.Sprintf("gcp.hostAutoscalerName=%s", hostAutoscalerName),
		"--set", fmt.Sprintf("gcp.builderNetwork=%s", vpcNetwork),
		"--set", fmt.Sprintf("gcp.builderServiceAccount=%s", builderGSA),
		"--set", fmt.Sprintf(`serviceAccount.annotations.iam\.gke\.io/gcp-service-account=%s`, controlPlaneGSA),
	}
	if cfg.Platform.ControlPlaneDomain != "" {
		helmArgs = append(helmArgs,
			"--set", "ingress.enabled=true",
			"--set", fmt.Sprintf("ingress.hosts[0].host=%s", cfg.Platform.ControlPlaneDomain),
		)
	}
	helmCmd := exec.Command("helm", helmArgs...)
	if err := runCommandStreaming(helmCmd); err != nil {
		return fmt.Errorf("failed to deploy helm chart: %w", err)
	}

	log.Info("Waiting for control plane deployment...")
	waitCmd := exec.Command("kubectl", "-n", onboardNamespace, "rollout", "status", "deploy/control-plane", "--timeout=300s")
	if err := runCommandStreaming(waitCmd); err != nil {
		return fmt.Errorf("control plane deployment did not become ready: %w", err)
	}

	for i := 0; i < 30; i++ {
		ip, err := resolveControlPlaneServiceIP()
		if err == nil && ip != "" {
			cfg.ResolvedControlPlaneIP = ip
			cfg.ResolvedControlPlaneURL = "http://" + ip + ":8080"
			log.WithField("control_plane_ip", ip).Info("Resolved control-plane service IP")
			return nil
		}
		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timed out waiting for control-plane service IP")
}

func applySecret(name string, data map[string]string) error {
	args := []string{"-n", onboardNamespace, "create", "secret", "generic", name}
	for k, v := range data {
		args = append(args, fmt.Sprintf("--from-literal=%s=%s", k, v))
	}
	args = append(args, "--dry-run=client", "-o", "yaml")
	createCmd := exec.Command("kubectl", args...)
	manifest, err := createCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to create secret manifest %s: %w", name, err)
	}
	applyCmd := exec.Command("kubectl", "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(string(manifest))
	if err := runCommandStreaming(applyCmd); err != nil {
		return fmt.Errorf("failed to apply secret %s: %w", name, err)
	}
	return nil
}

// stripIngressDocuments removes any YAML document containing "kind: Ingress" from a
// multi-document YAML string.
func stripIngressDocuments(content string) string {
	docs := strings.Split(content, "---")
	var kept []string
	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		if strings.Contains(doc, "kind: Ingress") {
			continue
		}
		kept = append(kept, doc)
	}
	return "---" + strings.Join(kept, "---")
}
