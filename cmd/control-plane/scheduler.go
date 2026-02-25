package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
)

// Scheduler handles runner allocation across hosts
type Scheduler struct {
	hostRegistry *HostRegistry
	db           *sql.DB
	logger       *logrus.Entry
}

// NewScheduler creates a new scheduler
func NewScheduler(hr *HostRegistry, db *sql.DB, logger *logrus.Logger) *Scheduler {
	return &Scheduler{
		hostRegistry: hr,
		db:           db,
		logger:       logger.WithField("component", "scheduler"),
	}
}

// AllocateRunnerRequest represents a request to allocate a runner
type AllocateRunnerRequest struct {
	RequestID         string
	ChunkKey          string
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
		"request_id": req.RequestID,
		"chunk_key":  req.ChunkKey,
	}).Info("Allocating runner")

	// Derive repo slug for multi-repo support
	chunkKey := req.ChunkKey

	// Look up snapshot config for fairness checks, ci_system, and start_command
	var ciSystem string
	var startCmd *snapshot.StartCommand
	if chunkKey != "" && s.db != nil {
		var maxConcurrent int
		var startCommandJSON sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_runners, ci_system, start_command FROM snapshot_configs WHERE chunk_key = $1`, chunkKey).Scan(&maxConcurrent, &ciSystem, &startCommandJSON)
		if err == nil {
			if maxConcurrent > 0 {
				var currentCount int
				_ = s.db.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM runners WHERE chunk_key = $1 AND status IN ('running','busy','initializing')
				`, chunkKey).Scan(&currentCount)
				if currentCount >= maxConcurrent {
					return nil, fmt.Errorf("chunk_key %s at max concurrent runners (%d/%d)", chunkKey, currentCount, maxConcurrent)
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

	// Score and select best host (with chunk-key cache affinity)
	host := s.selectBestHostForChunkKey(hosts, chunkKey)
	if host == nil {
		return nil, fmt.Errorf("no suitable host found")
	}

	s.logger.WithFields(logrus.Fields{
		"host_id":       host.ID,
		"instance_name": host.InstanceName,
		"grpc_address":  host.GRPCAddress,
	}).Debug("Selected host")

	// Connect to host agent
	conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to host %s: %w", host.GRPCAddress, err)
	}
	defer conn.Close()

	// Create host agent client
	client := pb.NewHostAgentClient(conn)

	// Build the proto request
	protoReq := &pb.AllocateRunnerRequest{
		RequestId:         req.RequestID,
		Labels:            req.Labels,
		GithubRunnerToken: req.GitHubRunnerToken,
		ChunkKey:          chunkKey,
		CiSystem:          ciSystem,
		SessionId:         req.SessionID,
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
			ID:         resp.Runner.Id,
			HostID:     host.ID,
			InternalIP: resp.Runner.InternalIp,
			Status:     "running",
			ChunkKey:   chunkKey,
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

// selectBestHostForChunkKey selects the best host with chunk-key cache affinity.
func (s *Scheduler) selectBestHostForChunkKey(hosts []*Host, chunkKey string) *Host {
	if len(hosts) == 0 {
		return nil
	}

	type scoredHost struct {
		host  *Host
		score float64
	}

	var scored []scoredHost
	for _, h := range hosts {
		score := s.scoreHostForChunkKey(h, chunkKey)
		scored = append(scored, scoredHost{host: h, score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	return scored[0].host
}

// scoreHostForChunkKey calculates a score for a host with chunk-key cache affinity.
func (s *Scheduler) scoreHostForChunkKey(h *Host, chunkKey string) float64 {
	score := s.scoreHost(h)

	// Repo-aware cache affinity scoring:
	// If we have loaded manifests info (from heartbeat), prefer hosts with warm caches.
	if chunkKey != "" && h.LoadedManifests != nil {
		if version, ok := h.LoadedManifests[chunkKey]; ok {
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
	conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to host: %w", err)
	}
	defer conn.Close()

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

	conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("failed to connect to host: %w", err)
	}
	defer conn.Close()

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

	conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to host: %w", err)
	}
	defer conn.Close()

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
