package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/sirupsen/logrus"
)

func stepSnapshotBuild(cfg *Config, logger *logrus.Logger, planOnly bool) error {
	log := logger.WithField("step", "snapshot-build")

	gcsBucket := fmt.Sprintf("%s-firecracker-snapshots", cfg.Platform.GCPProject)
	layeredCfg := cfg.ToLayeredConfig()
	if err := snapshot.ValidateLayeredConfig(layeredCfg); err != nil {
		return fmt.Errorf("invalid layered workload config: %w", err)
	}

	if planOnly {
		fmt.Println("\n[plan] snapshot-build:")
		fmt.Printf("  GCS bucket:     %s\n", gcsBucket)
		fmt.Printf("  Base image:     %s\n", layeredCfg.BaseImage)
		fmt.Printf("  Layers:         %d\n", len(layeredCfg.Layers))
		fmt.Printf("  Build assets:   snapshot-builder, thaw-agent, rootfs.img, kernel.bin\n")
		fmt.Printf("  vCPUs: %d, Memory: %dMB\n", cfg.MicroVM.VCPUs, cfg.MicroVM.MemoryMB)
		if len(cfg.Workload.StartCommand.Command) > 0 {
			fmt.Printf("  Start command:  %s\n", cfg.Workload.StartCommand.Command[0])
			fmt.Printf("  Health check:   :%d%s\n", cfg.Workload.StartCommand.Port, cfg.Workload.StartCommand.HealthPath)
		}
		return nil
	}

	log.Info("Building and staging builder artifacts...")
	buildCmd := exec.Command("make", "snapshot-builder", "thaw-agent", "rootfs")
	if err := runCommandStreaming(buildCmd); err != nil {
		return fmt.Errorf("failed to build builder artifacts: %w", err)
	}

	artifactPrefix := "gs://" + gcsBucket + "/v1/build-artifacts"
	for _, pair := range []struct {
		src string
		dst string
	}{
		{"bin/snapshot-builder", artifactPrefix + "/snapshot-builder"},
		{"bin/thaw-agent", artifactPrefix + "/thaw-agent"},
		{"images/microvm/output/rootfs.img", artifactPrefix + "/rootfs.img"},
		{"images/microvm/output/kernel.bin", artifactPrefix + "/kernel.bin"},
	} {
		uploadCmd := exec.Command("gcloud", "storage", "cp", pair.src, pair.dst)
		if err := runCommandStreaming(uploadCmd); err != nil {
			return fmt.Errorf("failed to upload %s: %w", pair.src, err)
		}
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
		configBody, err := json.Marshal(layeredCfg)
		if err != nil {
			return fmt.Errorf("failed to marshal layered config: %w", err)
		}

		createResp, err := http.Post(baseURL+"/api/v1/layered-configs", "application/json", bytes.NewReader(configBody))
		if err != nil {
			return fmt.Errorf("failed to register layered config: %w", err)
		}
		defer createResp.Body.Close()
		body, _ := io.ReadAll(createResp.Body)
		if createResp.StatusCode < 200 || createResp.StatusCode >= 300 {
			return fmt.Errorf("layered config registration failed (%d): %s", createResp.StatusCode, string(body))
		}

		var createResult struct {
			ConfigID        string `json:"config_id"`
			LeafWorkloadKey string `json:"leaf_workload_key"`
		}
		if err := json.Unmarshal(body, &createResult); err != nil {
			return fmt.Errorf("failed to decode config create response: %w", err)
		}
		cfg.ResolvedConfigID = createResult.ConfigID
		cfg.ResolvedWorkloadKey = createResult.LeafWorkloadKey
		log.WithFields(logrus.Fields{
			"config_id":     cfg.ResolvedConfigID,
			"workload_key":  cfg.ResolvedWorkloadKey,
		}).Info("Registered layered config")

		buildReq, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/layered-configs/"+cfg.ResolvedConfigID+"/build", nil)
		if err != nil {
			return fmt.Errorf("failed to create build request: %w", err)
		}
		buildResp, err := http.DefaultClient.Do(buildReq)
		if err != nil {
			return fmt.Errorf("failed to trigger layered build: %w", err)
		}
		defer buildResp.Body.Close()
		buildBody, _ := io.ReadAll(buildResp.Body)
		if buildResp.StatusCode < 200 || buildResp.StatusCode >= 300 {
			return fmt.Errorf("layered build trigger failed (%d): %s", buildResp.StatusCode, string(buildBody))
		}
		log.Info("Triggered layered build")

		return waitForLayeredBuild(baseURL, cfg.ResolvedConfigID, log)
	})
}

func waitForLayeredBuild(baseURL, configID string, log *logrus.Entry) error {
	client := &http.Client{Timeout: 15 * time.Second}
	deadline := time.Now().Add(45 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/api/v1/layered-configs/" + configID)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			time.Sleep(5 * time.Second)
			continue
		}

		var detail struct {
			Layers []struct {
				Name           string `json:"name"`
				Status         string `json:"status"`
				CurrentVersion string `json:"current_version,omitempty"`
				BuildStatus    string `json:"build_status,omitempty"`
			} `json:"layers"`
		}
		if err := json.Unmarshal(body, &detail); err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		if len(detail.Layers) == 0 {
			time.Sleep(5 * time.Second)
			continue
		}
		leaf := detail.Layers[len(detail.Layers)-1]
		log.WithFields(logrus.Fields{
			"layer":          leaf.Name,
			"status":         leaf.Status,
			"build_status":   leaf.BuildStatus,
			"current_version": leaf.CurrentVersion,
		}).Info("Waiting for layered build")
		if leaf.Status == "active" && leaf.CurrentVersion != "" {
			log.WithField("version", leaf.CurrentVersion).Info("Layered build completed and activated")
			return nil
		}
		if leaf.BuildStatus == "failed" || leaf.Status == "failed" {
			return fmt.Errorf("layered build failed for leaf layer %s", leaf.Name)
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("timed out waiting for layered build to complete")
}
