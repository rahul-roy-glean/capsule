package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
)

// startTTLEnforcement runs a background loop that auto-pauses idle runners
// whose persisted runner config has auto_pause enabled and whose idle duration
// exceeds the stored runner_ttl_seconds. This centralises TTL enforcement in
// the control plane instead of each host agent acting independently.
func (s *ControlPlaneServer) startTTLEnforcement(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.enforceTTLs(ctx)
		}
	}
}

func (s *ControlPlaneServer) enforceTTLs(ctx context.Context) {
	// Build a lookup of runner_id → persisted TTL/network policy config from the
	// runners table. This freezes TTL behavior at allocate time instead of
	// re-reading mutable layered-config defaults by workload key.
	type runnerConfig struct {
		ttlSeconds          int
		autoPause           bool
		workloadKey         string
		networkPolicyPreset string
		networkPolicyJSON   string
	}
	configs := make(map[string]runnerConfig)
	if s.snapshotManager != nil && s.snapshotManager.db != nil {
		rows, err := s.snapshotManager.db.QueryContext(ctx,
			`SELECT id, workload_key, runner_ttl_seconds, auto_pause, network_policy_preset, network_policy
			 FROM runners
			 WHERE auto_pause = true AND runner_ttl_seconds > 0`)
		if err != nil {
			s.logger.WithError(err).Warn("TTL enforcement: failed to query runners")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var runnerID string
			var cfg runnerConfig
			var npPreset sql.NullString
			var npJSON sql.NullString
			if err := rows.Scan(&runnerID, &cfg.workloadKey, &cfg.ttlSeconds, &cfg.autoPause, &npPreset, &npJSON); err != nil {
				continue
			}
			if npPreset.Valid {
				cfg.networkPolicyPreset = npPreset.String
			}
			if npJSON.Valid {
				cfg.networkPolicyJSON = npJSON.String
			}
			configs[runnerID] = cfg
		}
	}
	if len(configs) == 0 {
		return
	}

	// Scan all hosts for idle runners that have exceeded their TTL.
	type pauseCandidate struct {
		runnerID    string
		hostID      string
		grpcAddress string
		config      runnerConfig
	}
	var candidates []pauseCandidate

	hosts := s.hostRegistry.GetAllHosts()
	s.hostRegistry.mu.RLock()
	for _, h := range hosts {
		if h.GRPCAddress == "" {
			continue
		}
		for _, ri := range h.RunnerInfos {
			if ri.State != "idle" {
				continue
			}
			cfg, ok := configs[ri.RunnerID]
			if !ok {
				continue
			}
			if ri.IdleSince.IsZero() {
				continue
			}
			if time.Since(ri.IdleSince) >= time.Duration(cfg.ttlSeconds)*time.Second {
				candidates = append(candidates, pauseCandidate{
					runnerID:    ri.RunnerID,
					hostID:      h.ID,
					grpcAddress: h.GRPCAddress,
					config:      cfg,
				})
			}
		}
	}
	s.hostRegistry.mu.RUnlock()

	// Pause each candidate via gRPC to the host agent, same as HandlePauseRunner.
	for _, c := range candidates {
		s.logger.WithFields(logrus.Fields{
			"runner_id": c.runnerID,
			"host_id":   c.hostID,
		}).Info("TTL expired, auto-pausing runner")

		conn, err := grpc.NewClient(c.grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			s.logger.WithError(err).WithField("runner_id", c.runnerID).Warn("TTL enforcement: failed to connect to host")
			continue
		}

		client := pb.NewHostAgentClient(conn)
		resp, err := client.PauseRunner(ctx, &pb.PauseRunnerRequest{RunnerId: c.runnerID})
		conn.Close()
		if err != nil {
			s.logger.WithError(err).WithField("runner_id", c.runnerID).Warn("TTL enforcement: pause RPC failed")
			continue
		}
		if resp.Error != "" {
			s.logger.WithFields(logrus.Fields{
				"runner_id": c.runnerID,
				"error":     resp.Error,
			}).Warn("TTL enforcement: host returned error")
			continue
		}

		// Update durable session head/checkpoint state (same as HandlePauseRunner).
		if resp.SessionId != "" && s.scheduler.db != nil {
			generation := int(resp.Generation)
			if generation == 0 {
				generation = int(resp.Layer) + 1
			}
			_ = upsertSessionHead(ctx, s.scheduler.db, SessionHeadRecord{
				SessionID:                    resp.SessionId,
				WorkloadKey:                  c.config.workloadKey,
				CurrentHostID:                c.hostID,
				CurrentRunnerID:              c.runnerID,
				Status:                       "suspended",
				LatestGeneration:             generation,
				LatestManifestPath:           resp.ManifestPath,
				RunnerTTLSeconds:             c.config.ttlSeconds,
				AutoPause:                    c.config.autoPause,
				CheckpointIntervalSeconds:    0,
				CheckpointQuietWindowSeconds: 0,
				NetworkPolicyPreset:          c.config.networkPolicyPreset,
				NetworkPolicyJSON:            c.config.networkPolicyJSON,
				LastCheckpointedAt:           time.Now(),
			})
			_ = insertSessionCheckpoint(ctx, s.scheduler.db, SessionCheckpointRecord{
				SessionID:         resp.SessionId,
				Generation:        generation,
				ManifestPath:      resp.ManifestPath,
				CheckpointKind:    "pause",
				TriggerSource:     "ttl",
				HostID:            c.hostID,
				RunnerID:          c.runnerID,
				SnapshotSizeBytes: resp.SnapshotSizeBytes,
				CreatedAt:         time.Now(),
			})
		}

		if err := s.hostRegistry.RemoveRunner(c.runnerID); err != nil {
			s.logger.WithError(err).WithField("runner_id", c.runnerID).Warn("TTL enforcement: failed to remove paused runner from registry")
		}

		s.logger.WithFields(logrus.Fields{
			"runner_id":  c.runnerID,
			"session_id": resp.SessionId,
		}).Info("TTL enforcement: runner auto-paused")
	}
}
