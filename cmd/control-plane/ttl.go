package main

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
)

// startTTLEnforcement runs a background loop that auto-pauses idle runners
// whose workload_key has auto_pause enabled and whose idle duration exceeds
// the configured runner_ttl_seconds. This centralises TTL enforcement in the
// control plane instead of each host agent acting independently.
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
	// Build a lookup of workload_key → (ttl, auto_pause) from snapshot_configs.
	type ttlConfig struct {
		ttlSeconds int
		autoPause  bool
	}
	configs := make(map[string]ttlConfig)
	if s.snapshotManager != nil && s.snapshotManager.db != nil {
		rows, err := s.snapshotManager.db.QueryContext(ctx,
			`SELECT workload_key, runner_ttl_seconds, auto_pause FROM snapshot_configs WHERE auto_pause = true AND runner_ttl_seconds > 0`)
		if err != nil {
			s.logger.WithError(err).Warn("TTL enforcement: failed to query snapshot_configs")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var wk string
			var cfg ttlConfig
			if err := rows.Scan(&wk, &cfg.ttlSeconds, &cfg.autoPause); err != nil {
				continue
			}
			configs[wk] = cfg
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
		workloadKey string
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
			cfg, ok := configs[ri.WorkloadKey]
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
					workloadKey: ri.WorkloadKey,
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

		// Update session_snapshots (same as HandlePauseRunner).
		if resp.SessionId != "" && s.scheduler.db != nil {
			_, _ = s.scheduler.db.ExecContext(ctx, `
				INSERT INTO session_snapshots (session_id, runner_id, workload_key, host_id, status, layer_count, paused_at)
				VALUES ($1, $2, $3, $4, 'suspended', $5, NOW())
				ON CONFLICT (session_id) DO UPDATE SET
					status = 'suspended',
					layer_count = EXCLUDED.layer_count,
					paused_at = NOW()
			`, resp.SessionId, c.runnerID, c.workloadKey, c.hostID, resp.Layer+1)
		}

		// Roll back optimistic resource reservation.
		runner, err := s.hostRegistry.GetRunner(c.runnerID)
		if err == nil && (runner.ReservedCPU > 0 || runner.ReservedMemoryMB > 0) {
			host, hostErr := s.hostRegistry.GetHost(c.hostID)
			if hostErr == nil {
				s.hostRegistry.mu.Lock()
				host.UsedCPUMillicores -= runner.ReservedCPU
				host.UsedMemoryMB -= runner.ReservedMemoryMB
				if host.UsedCPUMillicores < 0 {
					host.UsedCPUMillicores = 0
				}
				if host.UsedMemoryMB < 0 {
					host.UsedMemoryMB = 0
				}
				s.hostRegistry.mu.Unlock()
			}
		}

		s.logger.WithFields(logrus.Fields{
			"runner_id":  c.runnerID,
			"session_id": resp.SessionId,
		}).Info("TTL enforcement: runner auto-paused")
	}
}
