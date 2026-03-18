package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/rahul-roy-glean/capsule/pkg/runner"
)

func startPeriodicCheckpointLoop(ctx context.Context, mgr *runner.Manager, controlPlane, hostBootstrapToken string, logger *logrus.Logger) {
	if controlPlane == "" || hostBootstrapToken == "" {
		return
	}
	if !strings.HasPrefix(controlPlane, "http://") && !strings.HasPrefix(controlPlane, "https://") {
		controlPlane = "http://" + controlPlane
	}
	checkpointURL := strings.TrimRight(controlPlane, "/") + "/api/v1/hosts/checkpoints"
	log := logger.WithField("component", "periodic-checkpoint")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			candidates := mgr.ListPeriodicCheckpointCandidates(now)
			for _, candidate := range candidates {
				if !mgr.TryBeginPeriodicCheckpoint(candidate.RunnerID) {
					continue
				}
				func(candidate runner.PeriodicCheckpointCandidate) {
					succeeded := false
					defer func() {
						mgr.FinishPeriodicCheckpoint(candidate.RunnerID, succeeded, time.Now())
					}()

					cpCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					defer cancel()

					result, err := mgr.CheckpointRunner(cpCtx, candidate.RunnerID)
					if err != nil {
						log.WithError(err).WithField("runner_id", candidate.RunnerID).Warn("Periodic checkpoint failed")
						return
					}

					body := map[string]any{
						"session_id":                      result.SessionID,
						"runner_id":                       candidate.RunnerID,
						"workload_key":                    candidate.WorkloadKey,
						"host_id":                         candidate.HostID,
						"generation":                      result.Generation,
						"manifest_path":                   result.ManifestPath,
						"snapshot_size_bytes":             result.SnapshotSizeBytes,
						"running":                         result.Running,
						"checkpoint_kind":                 "periodic",
						"trigger_source":                  "host_loop",
						"runner_ttl_seconds":              candidate.RunnerTTLSeconds,
						"auto_pause":                      candidate.AutoPause,
						"checkpoint_interval_seconds":     candidate.CheckpointIntervalSeconds,
						"checkpoint_quiet_window_seconds": candidate.CheckpointQuietWindowSeconds,
						"last_activity_at":                candidate.LastActivityAt,
					}
					payload, _ := json.Marshal(body)
					req, err := http.NewRequestWithContext(cpCtx, http.MethodPost, checkpointURL, bytes.NewReader(payload))
					if err != nil {
						log.WithError(err).WithField("runner_id", candidate.RunnerID).Warn("Failed to create checkpoint update request")
						return
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+hostBootstrapToken)
					resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
					if err != nil {
						log.WithError(err).WithField("runner_id", candidate.RunnerID).Warn("Failed to publish checkpoint update")
						return
					}
					resp.Body.Close()
					if resp.StatusCode >= 300 {
						log.WithFields(logrus.Fields{
							"runner_id": candidate.RunnerID,
							"status":    resp.StatusCode,
						}).Warn("Checkpoint update rejected by control plane")
						return
					}

					succeeded = true
					log.WithFields(logrus.Fields{
						"runner_id":     candidate.RunnerID,
						"session_id":    result.SessionID,
						"generation":    result.Generation,
						"manifest_path": result.ManifestPath,
					}).Info("Periodic checkpoint completed")
				}(candidate)
			}
		}
	}
}
