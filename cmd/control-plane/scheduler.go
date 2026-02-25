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
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

// Scheduler handles runner allocation across hosts
type Scheduler struct {
	hostRegistry    *HostRegistry
	db              *sql.DB
	snapshotManager *SnapshotManager
	logger          *logrus.Entry
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

	// Look up snapshot config for fairness checks, ci_system, start_command, and tier
	var ciSystem string
	var tierName string
	var startCmd *snapshot.StartCommand
	if workloadKey != "" && s.db != nil {
		var maxConcurrent int
		var startCommandJSON sql.NullString
		var tierCol sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_runners, ci_system, start_command, tier FROM snapshot_configs WHERE workload_key = $1`, workloadKey).Scan(&maxConcurrent, &ciSystem, &startCommandJSON, &tierCol)
		if err == nil {
			if tierCol.Valid && tierCol.String != "" {
				tierName = tierCol.String
			}
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

	// Resolve tier (default to "m")
	if tierName == "" {
		tierName = tiers.DefaultTier
	}
	tier, _ := tiers.Lookup(tierName)

	// Get available hosts
	hosts := s.hostRegistry.GetAvailableHosts()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no available hosts")
	}

	// Filter hosts that can fit this workload's resource requirements
	var eligible []*Host
	for _, h := range hosts {
		if canFitWorkload(h, tier) {
			eligible = append(eligible, h)
		}
	}
	if len(eligible) == 0 {
		// Fall back to all available hosts if none pass resource check
		// (handles hosts that don't yet report resources)
		eligible = hosts
	}

	// Score and select best host (with workload-key cache affinity)
	host := s.selectBestHostForWorkloadKey(eligible, workloadKey)
	if host == nil {
		return nil, fmt.Errorf("no suitable host found")
	}

	// Optimistic reservation: increment used resources on the in-memory host.
	// Next heartbeat from the host will correct any drift.
	effectiveCPU := tiers.EffectiveCPUMillicores(tier)
	s.hostRegistry.mu.Lock()
	host.UsedCPUMillicores += effectiveCPU
	host.UsedMemoryMB += tier.MemoryMB
	s.hostRegistry.mu.Unlock()

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
	// Populate resources from tier (overrides request-level values)
	protoReq.Resources = &pb.Resources{
		Vcpus:    int32(tier.VCPUs),
		MemoryMb: int32(tier.MemoryMB),
	}
	// Allow explicit request-level overrides if set
	if req.VCPUs > 0 {
		protoReq.Resources.Vcpus = int32(req.VCPUs)
	}
	if req.MemoryMB > 0 {
		protoReq.Resources.MemoryMb = int32(req.MemoryMB)
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
		// Roll back optimistic reservation
		s.hostRegistry.mu.Lock()
		host.UsedCPUMillicores -= effectiveCPU
		host.UsedMemoryMB -= tier.MemoryMB
		s.hostRegistry.mu.Unlock()
		s.logger.WithError(err).WithField("host", host.InstanceName).Error("gRPC AllocateRunner failed")
		return nil, fmt.Errorf("host agent AllocateRunner failed: %w", err)
	}

	if resp.Error != "" {
		// Roll back optimistic reservation
		s.hostRegistry.mu.Lock()
		host.UsedCPUMillicores -= effectiveCPU
		host.UsedMemoryMB -= tier.MemoryMB
		s.hostRegistry.mu.Unlock()
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

// canFitWorkload checks whether a host has enough resources to run a workload
// of the given tier.
func canFitWorkload(h *Host, t tiers.Tier) bool {
	if h.TotalCPUMillicores == 0 {
		return false
	}
	effectiveCPU := tiers.EffectiveCPUMillicores(t)
	return (h.TotalCPUMillicores-h.UsedCPUMillicores) >= effectiveCPU &&
		(h.TotalMemoryMB-h.UsedMemoryMB) >= t.MemoryMB
}

// scoreHost calculates a base score for a host using resource-based metrics.
func (s *Scheduler) scoreHost(h *Host) float64 {
	var score float64

	// Prefer hosts with idle runners (cache warmth)
	score += float64(h.IdleRunners) * 10

	if h.TotalCPUMillicores > 0 {
		cpuFree := float64(h.TotalCPUMillicores-h.UsedCPUMillicores) / float64(h.TotalCPUMillicores)
		memFree := float64(h.TotalMemoryMB-h.UsedMemoryMB) / float64(h.TotalMemoryMB)
		score += cpuFree * 20
		score += memFree * 15
	}

	// Prefer hosts with recent heartbeats
	if time.Since(h.LastHeartbeat) < 30*time.Second {
		score += 20
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

	var totalCPU, usedCPU, totalMem, usedMem, idleRunners, busyRunners int
	for _, h := range hosts {
		totalCPU += h.TotalCPUMillicores
		usedCPU += h.UsedCPUMillicores
		totalMem += h.TotalMemoryMB
		usedMem += h.UsedMemoryMB
		idleRunners += h.IdleRunners
		busyRunners += h.BusyRunners
	}

	return SchedulerStats{
		TotalHosts:         len(hosts),
		TotalCPUMillicores: totalCPU,
		UsedCPUMillicores:  usedCPU,
		TotalMemoryMB:      totalMem,
		UsedMemoryMB:       usedMem,
		IdleRunners:        idleRunners,
		BusyRunners:        busyRunners,
		QueueDepth:         s.GetQueueDepth(),
	}
}

// SchedulerStats holds scheduler statistics
type SchedulerStats struct {
	TotalHosts         int
	TotalCPUMillicores int
	UsedCPUMillicores  int
	TotalMemoryMB      int
	UsedMemoryMB       int
	IdleRunners        int
	BusyRunners        int
	QueueDepth         int
}
