package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

func stepVerify(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "verify")

	if planOnly {
		log.Info("Skipped (nothing to verify in plan mode)")
		return nil
	}

	// Get host instances
	log.Info("Checking host instances...")
	cmd := exec.Command("gcloud", "compute", "instances", "list",
		"--project", cfg.Platform.GCPProject,
		"--filter", "name~firecracker-runner",
		"--format", "value(name,networkInterfaces[0].networkIP,status)")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to list instances: %w\n%s", err, string(output))
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return fmt.Errorf("no host instances found")
	}

	log.WithField("count", len(lines)).Info("Found host instances")

	// Health check each host
	healthyHosts := 0
	client := &http.Client{Timeout: 5 * time.Second}

	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		name, ip, status := parts[0], parts[1], parts[2]

		log.WithFields(logrus.Fields{
			"name":   name,
			"ip":     ip,
			"status": status,
		}).Info("Checking host")

		if status != "RUNNING" {
			log.WithField("name", name).Warn("Host not running")
			continue
		}

		healthURL := fmt.Sprintf("http://%s:8080/health", ip)
		resp, err := client.Get(healthURL)
		if err != nil {
			log.WithError(err).WithField("name", name).Warn("Health check failed")
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			healthyHosts++
			log.WithField("name", name).Info("Host healthy")
		} else {
			log.WithFields(logrus.Fields{
				"name":   name,
				"status": resp.StatusCode,
			}).Warn("Host unhealthy")
		}
	}

	if healthyHosts == 0 {
		return fmt.Errorf("no healthy hosts found")
	}

	log.WithFields(logrus.Fields{
		"healthy": healthyHosts,
		"total":   len(lines),
	}).Info("Host verification complete")

	// Check pool stats on first healthy host
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) < 3 || parts[2] != "RUNNING" {
			continue
		}
		ip := parts[1]
		readyURL := fmt.Sprintf("http://%s:8080/ready", ip)
		resp, err := client.Get(readyURL)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		var readyData map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&readyData)
		log.WithField("ready_data", readyData).Info("Host readiness status")
		break
	}

	log.Info("Verification complete!")
	return nil
}
