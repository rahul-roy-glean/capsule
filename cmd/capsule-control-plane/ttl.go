package main

import (
	"context"
	"database/sql"
	"time"

	"github.com/sirupsen/logrus"

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

		conn, err := s.scheduler.getHostConn(c.grpcAddress)
		if err != nil {
			s.logger.WithError(err).WithField("runner_id", c.runnerID).Warn("TTL enforcement: failed to connect to host")
			continue
		}

		client := pb.NewHostAgentClient(conn)
		resp, err := client.PauseRunner(ctx, &pb.PauseRunnerRequest{RunnerId: c.runnerID})
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

		// Update session_snapshots (same as HandlePauseRunner).
		if resp.SessionId != "" && s.scheduler.db != nil {
			var networkPolicy any
			if c.config.networkPolicyJSON != "" {
				networkPolicy = c.config.networkPolicyJSON
			}
			// Derive config_id from the workload_key for migration support.
			var configID *string
			var cfgID string
			if s.scheduler.db.QueryRowContext(ctx,
				`SELECT config_id FROM config_workload_keys WHERE leaf_workload_key = $1 AND status IN ('active','draining') LIMIT 1`,
				c.config.workloadKey).Scan(&cfgID) == nil {
				configID = &cfgID
			}
			_, _ = s.scheduler.db.ExecContext(ctx, `
				INSERT INTO session_snapshots (
					session_id, runner_id, workload_key, host_id, status, layer_count, paused_at,
					runner_ttl_seconds, auto_pause, network_policy_preset, network_policy, config_id
				)
				VALUES ($1, $2, $3, $4, 'suspended', $5, NOW(), $6, $7, $8, $9, $10)
				ON CONFLICT (session_id) DO UPDATE SET
					status = 'suspended',
					layer_count = EXCLUDED.layer_count,
					paused_at = NOW(),
					runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
					auto_pause = EXCLUDED.auto_pause,
					network_policy_preset = EXCLUDED.network_policy_preset,
					network_policy = EXCLUDED.network_policy,
					config_id = EXCLUDED.config_id
			`, resp.SessionId, c.runnerID, c.config.workloadKey, c.hostID, resp.Layer+1,
				c.config.ttlSeconds, c.config.autoPause, c.config.networkPolicyPreset, networkPolicy, configID)
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
