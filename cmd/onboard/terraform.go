package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

// requiredVarDefaults lists terraform variables that are required (no default)
// but are not set by the onboard config. In plan mode we supply placeholder
// values so `terraform plan` can run without prompting. These placeholders
// are never used for apply — the user must provide real values.
var requiredVarDefaults = map[string]string{
	"db_password": "plan-placeholder",
}

// planVarArgs returns -var flags for required variables that have no default
// and are not present in any loaded tfvars file. Only used in plan mode.
func planVarArgs(tfDir string) []string {
	// Read all .tfvars files that terraform auto-loads to see which vars are set.
	set := make(map[string]bool)
	for _, name := range []string{"terraform.tfvars", "terraform.auto.tfvars"} {
		data, err := os.ReadFile(filepath.Join(tfDir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if k, _, ok := strings.Cut(line, "="); ok {
				set[strings.TrimSpace(k)] = true
			}
		}
	}

	var args []string
	for k, v := range requiredVarDefaults {
		if !set[k] {
			args = append(args, fmt.Sprintf("-var=%s=%s", k, v))
		}
	}
	return args
}

func stepTerraformBootstrap(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "terraform-bootstrap")

	if err := resolveRuntimeSecrets(cfg); err != nil {
		return err
	}

	tfvarsPath, err := generateTFVars(cfg, false)
	if err != nil {
		return fmt.Errorf("failed to generate tfvars: %w", err)
	}
	log.WithField("tfvars", tfvarsPath).Info("Generated terraform.tfvars")

	tfDir := "deploy/terraform"

	if !planOnly {
		if err := ensureTerraformStateBucket(cfg, log); err != nil {
			return err
		}
	}

	log.Info("Running terraform init...")
	initArgs := []string{"init"}
	if planOnly {
		initArgs = append(initArgs, "-backend=false")
	} else {
		initArgs = append(initArgs,
			fmt.Sprintf("-backend-config=bucket=%s", cfg.ResolvedStateBucket),
			fmt.Sprintf("-backend-config=prefix=%s", terraformStatePrefix(cfg)),
		)
	}
	initCmd := exec.Command("terraform", initArgs...)
	initCmd.Dir = tfDir
	initCmd.Stdout = os.Stdout
	initCmd.Stderr = os.Stderr
	if err := initCmd.Run(); err != nil {
		return fmt.Errorf("terraform init failed: %w", err)
	}

	if planOnly {
		log.Info("Running terraform plan (bootstrap mode - stock images)...")
		args := append([]string{"plan", "-out=tfplan-bootstrap"}, planVarArgs(tfDir)...)
		planCmd := exec.Command("terraform", args...)
		planCmd.Dir = tfDir
		planCmd.Stdout = os.Stdout
		planCmd.Stderr = os.Stderr
		if err := planCmd.Run(); err != nil {
			return fmt.Errorf("terraform plan failed: %w", err)
		}
		return nil
	}

	log.Info("Running terraform apply (bootstrap mode - stock images)...")
	applyCmd := exec.Command("terraform", "apply", "-auto-approve")
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

	if cfg.ResolvedControlPlaneURL == "" {
		if cfg.ResolvedControlPlaneIP == "" {
			ip, err := resolveControlPlaneServiceIP()
			if err != nil {
				return err
			}
			cfg.ResolvedControlPlaneIP = ip
		}
		cfg.ResolvedControlPlaneURL = "http://" + cfg.ResolvedControlPlaneIP + ":8080"
	}

	tfvarsPath, err := generateTFVars(cfg, true)
	if err != nil {
		return fmt.Errorf("failed to generate tfvars: %w", err)
	}
	log.WithField("tfvars", tfvarsPath).Info("Generated terraform.tfvars (finalize mode)")

	tfDir := "deploy/terraform"

	if planOnly {
		log.Info("Running terraform plan (finalize mode - custom images)...")
		args := append([]string{"plan", "-out=tfplan-finalize"}, planVarArgs(tfDir)...)
		planCmd := exec.Command("terraform", args...)
		planCmd.Dir = tfDir
		planCmd.Stdout = os.Stdout
		planCmd.Stderr = os.Stderr
		if err := planCmd.Run(); err != nil {
			return fmt.Errorf("terraform plan failed: %w", err)
		}
		return nil
	}

	log.Info("Running terraform apply (finalize mode - custom images)...")
	applyCmd := exec.Command("terraform", "apply", "-auto-approve")
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
	addStr("environment", cfg.Platform.Environment)
	if cidrs, err := resolveAdminCIDRs(cfg, finalize); err == nil && len(cidrs) > 0 {
		lines = append(lines, fmt.Sprintf("admin_cidrs = [%s]", quotedList(cidrs)))
	}

	// --- Hosts ---
	addStr("host_machine_type", cfg.Hosts.MachineType)
	minHosts := cfg.Hosts.MinCount
	if !finalize {
		minHosts = 0
	}
	addInt("min_hosts", minHosts)
	addInt("max_hosts", cfg.Hosts.MaxCount)
	addInt("host_data_disk_size_gb", cfg.Hosts.DataDiskGB)
	addInt("chunk_cache_size_gb", cfg.Hosts.ChunkCacheSizeGB)
	addInt("mem_cache_size_gb", cfg.Hosts.MemCacheSizeGB)

	// --- MicroVMs ---
	addInt("max_runners_per_host", cfg.MicroVM.MaxPerHost)
	addInt("idle_runners_target", cfg.MicroVM.IdleTarget)

	// --- Snapshots & networking ---
	addBool("use_custom_host_image", finalize)
	addBool("use_chunked_snapshots", true)
	addBool("enable_session_chunks", cfg.Session.Enabled)
	addBool("use_netns", true)
	addStr("db_password", cfg.ResolvedDBPassword)
	if finalize && cfg.ResolvedControlPlaneURL != "" {
		addStr("control_plane_addr", cfg.ResolvedControlPlaneURL)
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(tfvarsPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write tfvars: %w", err)
	}

	return tfvarsPath, nil
}

func resolveRuntimeSecrets(cfg *Config) error {
	cfg.ResolvedStateBucket = gcsStateBucketForProject(cfg)

	if env := os.Getenv("ONBOARD_DB_PASSWORD"); env != "" {
		cfg.ResolvedDBPassword = env
		return nil
	}
	if cfg.Platform.DBPassword != "" {
		cfg.ResolvedDBPassword = cfg.Platform.DBPassword
		return nil
	}
	if existing, err := readTerraformVar("deploy/terraform", "db_password"); err == nil && existing != "" {
		cfg.ResolvedDBPassword = existing
		return nil
	}
	secret, err := generateSecretString(24)
	if err != nil {
		return fmt.Errorf("failed to generate db password: %w", err)
	}
	cfg.ResolvedDBPassword = secret
	return nil
}

func resolveAdminCIDRs(cfg *Config, finalize bool) ([]string, error) {
	if len(cfg.Platform.AdminCIDRs) > 0 {
		return cfg.Platform.AdminCIDRs, nil
	}
	if planOnlyCIDR := os.Getenv("ONBOARD_ADMIN_CIDR"); planOnlyCIDR != "" {
		return []string{planOnlyCIDR}, nil
	}
	if finalize {
		ip, err := detectPublicIPCIDR()
		if err != nil {
			return []string{"0.0.0.0/0"}, nil
		}
		return []string{ip}, nil
	}
	// Bootstrap must already authorize the operator who will later fetch GKE credentials.
	ip, err := detectPublicIPCIDR()
	if err != nil {
		return []string{"0.0.0.0/0"}, nil
	}
	return []string{ip}, nil
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, fmt.Sprintf("%q", v))
	}
	return strings.Join(quoted, ", ")
}

func readTerraformVar(tfDir, key string) (string, error) {
	for _, name := range []string{"terraform.auto.tfvars", "terraform.tfvars"} {
		data, err := os.ReadFile(filepath.Join(tfDir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			return strings.Trim(strings.TrimSpace(v), `"`), nil
		}
	}
	return "", fmt.Errorf("terraform var %s not found", key)
}

func ensureTerraformStateBucket(cfg *Config, log *logrus.Entry) error {
	describeCmd := exec.Command("gcloud", "storage", "buckets", "describe", "gs://"+cfg.ResolvedStateBucket, "--project="+cfg.Platform.GCPProject)
	if err := describeCmd.Run(); err == nil {
		return nil
	}

	log.WithField("bucket", cfg.ResolvedStateBucket).Info("Creating Terraform state bucket...")
	createCmd := exec.Command("gcloud", "storage", "buckets", "create", "gs://"+cfg.ResolvedStateBucket,
		"--project="+cfg.Platform.GCPProject,
		"--location="+cfg.Platform.Region,
		"--uniform-bucket-level-access",
	)
	createCmd.Stdout = os.Stdout
	createCmd.Stderr = os.Stderr
	if err := createCmd.Run(); err != nil {
		return fmt.Errorf("failed to create terraform state bucket: %w", err)
	}
	return nil
}

func resolveControlPlaneServiceIP() (string, error) {
	cmd := exec.Command("kubectl", "-n", onboardNamespace, "get", "svc", "control-plane", "-o", "jsonpath={.status.loadBalancer.ingress[0].ip}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to resolve control-plane service IP: %w\n%s", err, string(out))
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("control-plane service does not have a load balancer IP yet")
	}
	return ip, nil
}
