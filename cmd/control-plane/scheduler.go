package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/telemetry"
)

// Scheduler handles runner allocation across hosts
type Scheduler struct {
	hostRegistry    *HostRegistry
	db              *sql.DB
	snapshotManager *SnapshotManager
	metricsClient   *telemetry.Client
	logger          *logrus.Entry

	// connPool caches gRPC connections to host agents, keyed by address.
	// gRPC connections are multiplexed and designed to be long-lived, so
	// reusing them avoids TCP+TLS handshake latency on every RPC.
	connPool sync.Map // map[string]*grpc.ClientConn
}

// NewScheduler creates a new scheduler
func NewScheduler(hr *HostRegistry, db *sql.DB, sm *SnapshotManager, logger *logrus.Logger) *Scheduler {
	return &Scheduler{
		hostRegistry:    hr,
		db:              db,
		snapshotManager: sm,
		logger:          logger.WithField("component", "scheduler"),
	}
}

// SetMetricsClient attaches a telemetry client for GCP Cloud Monitoring.
func (s *Scheduler) SetMetricsClient(c *telemetry.Client) {
	s.metricsClient = c
}

// getHostConn returns a cached gRPC connection to the given address, creating
// one if needed. Connections are long-lived and multiplexed.
func (s *Scheduler) getHostConn(address string) (*grpc.ClientConn, error) {
	if v, ok := s.connPool.Load(address); ok {
		return v.(*grpc.ClientConn), nil
	}

	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to host %s: %w", address, err)
	}

	// Store-or-load to handle concurrent creation for the same address.
	if actual, loaded := s.connPool.LoadOrStore(address, conn); loaded {
		// Another goroutine won the race; close our duplicate.
		conn.Close()
		return actual.(*grpc.ClientConn), nil
	}

	return conn, nil
}

// Close closes all cached gRPC connections.
func (s *Scheduler) Close() {
	s.connPool.Range(func(key, value any) bool {
		if conn, ok := value.(*grpc.ClientConn); ok {
			conn.Close()
		}
		s.connPool.Delete(key)
		return true
	})
}

// AllocateRunnerRequest represents a request to allocate a runner
type AllocateRunnerRequest struct {
	RequestID         string
	WorkloadKey       string
	Labels            map[string]string
	GitHubRunnerToken string
	CISystem          string
	SessionID         string
	VCPUs             int
	MemoryMB          int
}

// AllocateRunnerResponse represents the response from runner allocation
type AllocateRunnerResponse struct {
	RunnerID    string
	HostID      string
	HostAddress string
	InternalIP  string
	SessionID   string
	Resumed     bool
	Error       string
}

// AllocateRunner allocates a runner on the best available host
func (s *Scheduler) AllocateRunner(ctx context.Context, req AllocateRunnerRequest) (*AllocateRunnerResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"request_id":   req.RequestID,
		"workload_key": req.WorkloadKey,
	}).Info("Allocating runner")

	// Derive repo slug for multi-repo support
	workloadKey := req.WorkloadKey

	// Look up snapshot config for fairness checks, ci_system, and start_command
	var ciSystem string
	var startCmd *snapshot.StartCommand
	if workloadKey != "" && s.db != nil {
		var maxConcurrent int
		var startCommandJSON sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_runners, ci_system, start_command FROM snapshot_configs WHERE workload_key = $1`, workloadKey).Scan(&maxConcurrent, &ciSystem, &startCommandJSON)
		if err == nil {
			if maxConcurrent > 0 {
				var currentCount int
				_ = s.db.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM runners WHERE workload_key = $1 AND status IN ('running','busy','initializing')
				`, workloadKey).Scan(&currentCount)
				if currentCount >= maxConcurrent {
					return nil, fmt.Errorf("workload_key %s at max concurrent runners (%d/%d)", workloadKey, currentCount, maxConcurrent)
				}
			}
			if startCommandJSON.Valid && startCommandJSON.String != "" {
				startCmd = &snapshot.StartCommand{}
				if err := json.Unmarshal([]byte(startCommandJSON.String), startCmd); err != nil {
					s.logger.WithError(err).Warn("Failed to parse start_command from snapshot config")
					startCmd = nil
				}
			}
		}
	}

	// Get available hosts
	hosts := s.hostRegistry.GetAvailableHosts()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no available hosts")
	}

	// Session stickiness: if this is a session resume, prefer the host where
	// the session was paused. This keeps the LRU chunk cache warm and avoids
	// cold GCS fetches on resume.
	var host *Host
	if req.SessionID != "" && s.db != nil {
		var sessionHostID string
		var status string
		err := s.db.QueryRowContext(ctx,
			`SELECT host_id, status FROM session_snapshots WHERE session_id = $1`,
			req.SessionID).Scan(&sessionHostID, &status)
		if err == nil && status == "suspended" && sessionHostID != "" {
			for _, h := range hosts {
				if h.ID == sessionHostID {
					host = h
					s.logger.WithFields(logrus.Fields{
						"session_id": req.SessionID,
						"host_id":    sessionHostID,
					}).Info("Session sticky routing: using original host")
					if s.metricsClient != nil {
						s.metricsClient.IncrementCounter(ctx, telemetry.MetricSessionResumeRouting, telemetry.Labels{
							telemetry.LabelRouting: telemetry.RoutingSameHost,
						})
					}
					break
				}
			}
			if host == nil {
				s.logger.WithFields(logrus.Fields{
					"session_id":      req.SessionID,
					"original_host":   sessionHostID,
					"available_hosts": len(hosts),
				}).Warn("Session sticky host not available, falling back to best-fit")
				if s.metricsClient != nil {
					s.metricsClient.IncrementCounter(ctx, telemetry.MetricSessionResumeRouting, telemetry.Labels{
						telemetry.LabelRouting: telemetry.RoutingCrossHost,
					})
				}
			}
		}
	}

	// Fall back to workload-key cache affinity scoring
	if host == nil {
		host = s.selectBestHostForWorkloadKey(hosts, workloadKey)
	}
	if host == nil {
		return nil, fmt.Errorf("no suitable host found")
	}

	s.logger.WithFields(logrus.Fields{
		"host_id":       host.ID,
		"instance_name": host.InstanceName,
		"grpc_address":  host.GRPCAddress,
	}).Debug("Selected host")

	// Connect to host agent (pooled connection)
	conn, err := s.getHostConn(host.GRPCAddress)
	if err != nil {
		return nil, err
	}

	// Create host agent client
	client := pb.NewHostAgentClient(conn)

	// Resolve the desired snapshot version for this workload_key + host
	var snapshotVersion string
	if workloadKey != "" && s.snapshotManager != nil && s.snapshotManager.db != nil {
		desired, _ := s.snapshotManager.GetDesiredVersions(ctx, host.ID)
		if v, ok := desired[workloadKey]; ok {
			snapshotVersion = v
		}
	}
	if snapshotVersion == "" && workloadKey != "" && s.snapshotManager != nil {
		snapshotVersion = s.snapshotManager.GetCurrentVersionForKey(workloadKey)
	}

	// Build the proto request
	protoReq := &pb.AllocateRunnerRequest{
		RequestId:         req.RequestID,
		Labels:            req.Labels,
		GithubRunnerToken: req.GitHubRunnerToken,
		WorkloadKey:       workloadKey,
		CiSystem:          ciSystem,
		SessionId:         req.SessionID,
		SnapshotVersion:   snapshotVersion,
	}
	if req.VCPUs > 0 || req.MemoryMB > 0 {
		protoReq.Resources = &pb.Resources{
			Vcpus:    int32(req.VCPUs),
			MemoryMb: int32(req.MemoryMB),
		}
	}
	if startCmd != nil {
		protoReq.StartCommand = &pb.StartCommand{
			Command:    startCmd.Command,
			Port:       int32(startCmd.Port),
			HealthPath: startCmd.HealthPath,
		}
	}

	// Call host agent to allocate runner
	resp, err := client.AllocateRunner(ctx, protoReq)
	if err != nil {
		s.logger.WithError(err).WithField("host", host.InstanceName).Error("gRPC AllocateRunner failed")
		return nil, fmt.Errorf("host agent AllocateRunner failed: %w", err)
	}

	if resp.Error != "" {
		return &AllocateRunnerResponse{
			HostID:      host.ID,
			HostAddress: host.GRPCAddress,
			Error:       resp.Error,
		}, fmt.Errorf("host agent returned error: %s", resp.Error)
	}

	// Register runner in our registry
	if resp.Runner != nil {
		if err := s.hostRegistry.AddRunner(ctx, &Runner{
			ID:          resp.Runner.Id,
			HostID:      host.ID,
			InternalIP:  resp.Runner.InternalIp,
			Status:      "running",
			WorkloadKey: workloadKey,
		}); err != nil {
			s.logger.WithError(err).Warn("Failed to register runner in control plane registry")
		}
	}

	return &AllocateRunnerResponse{
		RunnerID:    resp.Runner.GetId(),
		HostID:      host.ID,
		HostAddress: host.GRPCAddress,
		InternalIP:  resp.Runner.GetInternalIp(),
		SessionID:   resp.GetSessionId(),
		Resumed:     resp.GetResumed(),
	}, nil
}

// selectBestHostForWorkloadKey selects the best host with workload-key cache affinity.
func (s *Scheduler) selectBestHostForWorkloadKey(hosts []*Host, workloadKey string) *Host {
	if len(hosts) == 0 {
		return nil
	}

	type scoredHost struct {
		host  *Host
		score float64
	}

	var scored []scoredHost
	for _, h := range hosts {
		score := s.scoreHostForWorkloadKey(h, workloadKey)
		scored = append(scored, scoredHost{host: h, score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	return scored[0].host
}

// scoreHostForWorkloadKey calculates a score for a host with workload-key cache affinity.
func (s *Scheduler) scoreHostForWorkloadKey(h *Host, workloadKey string) float64 {
	score := s.scoreHost(h)

	// Repo-aware cache affinity scoring:
	// If we have loaded manifests info (from heartbeat), prefer hosts with warm caches.
	if workloadKey != "" && h.LoadedManifests != nil {
		if version, ok := h.LoadedManifests[workloadKey]; ok {
			// Host has a manifest loaded for this repo
			// Check if it's the current version (ideal) or any version (warm-ish)
			if version != "" {
				score += 100 // Warm cache: manifest loaded for this repo
			} else {
				score += 50 // Manifest loaded but version unknown
			}
		}
		// No manifest for this repo: +0 (cold, but fast in UFFD mode)
	}

	return score
}

// scoreHost calculates a base score for a host
func (s *Scheduler) scoreHost(h *Host) float64 {
	var score float64

	// Prefer hosts with idle runners (faster allocation)
	score += float64(h.IdleRunners) * 10

	// Prefer hosts with more available capacity
	availableSlots := h.TotalSlots - h.UsedSlots
	score += float64(availableSlots) * 5

	// Prefer hosts with recent heartbeats
	if time.Since(h.LastHeartbeat) < 30*time.Second {
		score += 20
	}

	// Penalize hosts with high utilization
	if h.TotalSlots > 0 {
		utilization := float64(h.UsedSlots) / float64(h.TotalSlots)
		if utilization > 0.8 {
			score -= 30
		}
	}

	return score
}

// ReleaseRunner releases a runner
func (s *Scheduler) ReleaseRunner(ctx context.Context, runnerID string, destroy bool) error {
	s.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"destroy":   destroy,
	}).Info("Releasing runner")

	// Get runner's host from registry
	runner, err := s.hostRegistry.GetRunner(runnerID)
	if err != nil {
		return err
	}

	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		return err
	}

	// Connect to host and release
	conn, err := s.getHostConn(host.GRPCAddress)
	if err != nil {
		return err
	}

	client := pb.NewHostAgentClient(conn)
	resp, err := client.ReleaseRunner(ctx, &pb.ReleaseRunnerRequest{
		RunnerId: runnerID,
		Destroy:  destroy,
	})
	if err != nil {
		return fmt.Errorf("host agent ReleaseRunner failed: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("host agent returned error: %s", resp.Error)
	}

	// Update registry
	return s.hostRegistry.RemoveRunner(runnerID)
}

// QuarantineRunner quarantines a runner via its host agent
func (s *Scheduler) QuarantineRunner(ctx context.Context, runnerID, reason string, blockEgress, pauseVM bool) (string, error) {
	s.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"reason":    reason,
	}).Info("Quarantining runner")

	runner, err := s.hostRegistry.GetRunner(runnerID)
	if err != nil {
		return "", err
	}

	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		return "", err
	}

	conn, err := s.getHostConn(host.GRPCAddress)
	if err != nil {
		return "", err
	}

	client := pb.NewHostAgentClient(conn)
	resp, err := client.QuarantineRunner(ctx, &pb.QuarantineRunnerRequest{
		RunnerId:    runnerID,
		Reason:      reason,
		BlockEgress: blockEgress,
		PauseVm:     pauseVM,
	})
	if err != nil {
		return "", fmt.Errorf("host agent QuarantineRunner failed: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("host agent returned error: %s", resp.Error)
	}

	return resp.QuarantineDir, nil
}

// UnquarantineRunner unquarantines a runner via its host agent
func (s *Scheduler) UnquarantineRunner(ctx context.Context, runnerID string, unblockEgress, resumeVM bool) error {
	s.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
	}).Info("Unquarantining runner")

	runner, err := s.hostRegistry.GetRunner(runnerID)
	if err != nil {
		return err
	}

	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		return err
	}

	conn, err := s.getHostConn(host.GRPCAddress)
	if err != nil {
		return err
	}

	client := pb.NewHostAgentClient(conn)
	resp, err := client.UnquarantineRunner(ctx, &pb.UnquarantineRunnerRequest{
		RunnerId:      runnerID,
		UnblockEgress: unblockEgress,
		ResumeVm:      resumeVM,
	})
	if err != nil {
		return fmt.Errorf("host agent UnquarantineRunner failed: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("host agent returned error: %s", resp.Error)
	}

	return nil
}

// GetQueueDepth returns the current queue depth
func (s *Scheduler) GetQueueDepth() int {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='queued'`).Scan(&count)
	if err != nil {
		s.logger.WithError(err).Warn("Failed to query queue depth")
		return 0
	}
	return count
}

// GetStats returns scheduler statistics
func (s *Scheduler) GetStats() SchedulerStats {
	hosts := s.hostRegistry.GetAllHosts()

	var totalSlots, usedSlots, idleRunners, busyRunners int
	for _, h := range hosts {
		totalSlots += h.TotalSlots
		usedSlots += h.UsedSlots
		idleRunners += h.IdleRunners
		busyRunners += h.BusyRunners
	}

	return SchedulerStats{
		TotalHosts:  len(hosts),
		TotalSlots:  totalSlots,
		UsedSlots:   usedSlots,
		IdleRunners: idleRunners,
		BusyRunners: busyRunners,
		QueueDepth:  s.GetQueueDepth(),
	}
}

// SchedulerStats holds scheduler statistics
type SchedulerStats struct {
	TotalHosts  int
	TotalSlots  int
	UsedSlots   int
	IdleRunners int
	BusyRunners int
	QueueDepth  int
}
