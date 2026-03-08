package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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

	return withControlPlanePortForward(func(baseURL string) error {
		client := &http.Client{Timeout: 10 * time.Second}

		log.Info("Checking control-plane health...")
		resp, err := client.Get(baseURL + "/health")
		if err != nil {
			return fmt.Errorf("control-plane health check failed: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("control-plane health returned %d", resp.StatusCode)
		}

		log.Info("Checking registered hosts...")
		resp, err = client.Get(baseURL + "/api/v1/hosts")
		if err != nil {
			return fmt.Errorf("failed to get hosts: %w", err)
		}
		defer resp.Body.Close()
		var hosts struct {
			Count int `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&hosts); err != nil {
			return fmt.Errorf("failed to decode hosts response: %w", err)
		}
		if hosts.Count == 0 {
			return fmt.Errorf("control plane reports zero healthy hosts")
		}
		log.WithField("host_count", hosts.Count).Info("Host verification complete")

		if cfg.ResolvedWorkloadKey == "" {
			log.Info("No workload key available, skipping allocation probe")
			return nil
		}

		log.WithField("workload_key", cfg.ResolvedWorkloadKey).Info("Allocating verification runner...")
		reqBody := fmt.Sprintf(`{"workload_key":"%s"}`, cfg.ResolvedWorkloadKey)
		resp, err = client.Post(baseURL+"/api/v1/runners/allocate", "application/json", strings.NewReader(reqBody))
		if err != nil {
			return fmt.Errorf("verification allocation failed: %w", err)
		}
		defer resp.Body.Close()
		var alloc struct {
			RunnerID string `json:"runner_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&alloc); err != nil {
			return fmt.Errorf("failed to decode allocation response: %w", err)
		}
		if alloc.RunnerID == "" {
			return fmt.Errorf("verification allocation returned empty runner_id")
		}

		for i := 0; i < 30; i++ {
			statusResp, err := client.Get(baseURL + "/api/v1/runners/status?runner_id=" + alloc.RunnerID)
			if err == nil {
				if statusResp.StatusCode == http.StatusOK {
					statusResp.Body.Close()
					log.WithField("runner_id", alloc.RunnerID).Info("Verification runner became ready")
					break
				}
				statusResp.Body.Close()
			}
			if i == 29 {
				return fmt.Errorf("verification runner %s did not become ready in time", alloc.RunnerID)
			}
			time.Sleep(5 * time.Second)
		}

		releaseBody := fmt.Sprintf(`{"runner_id":"%s"}`, alloc.RunnerID)
		resp, err = client.Post(baseURL+"/api/v1/runners/release", "application/json", strings.NewReader(releaseBody))
		if err != nil {
			return fmt.Errorf("failed to release verification runner: %w", err)
		}
		resp.Body.Close()
		log.WithField("runner_id", alloc.RunnerID).Info("Verification runner released")
		return nil
	})
}
