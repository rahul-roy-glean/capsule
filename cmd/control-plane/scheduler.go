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
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	fcrotel "github.com/rahul-roy-glean/bazel-firecracker/pkg/otel"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

// Scheduler handles runner allocation across hosts
type Scheduler struct {
	hostRegistry    *HostRegistry
	db              *sql.DB
	snapshotManager *SnapshotManager
	tagRegistry     *SnapshotTagRegistry
	configCache     *ConfigCache
	logger          *logrus.Entry

	sessionResumeRoutingCounter metric.Int64Counter
	otelClient                  *fcrotel.Client

	// connPool caches gRPC connections to host agents, keyed by address.
	// gRPC connections are multiplexed and designed to be long-lived, so
	// reusing them avoids TCP+TLS handshake latency on every RPC.
	connPool sync.Map // map[string]*grpc.ClientConn
}

// NewScheduler creates a new scheduler
func NewScheduler(hr *HostRegistry, db *sql.DB, sm *SnapshotManager, tr *SnapshotTagRegistry, logger *logrus.Logger) *Scheduler {
	return &Scheduler{
		hostRegistry:    hr,
		db:              db,
		snapshotManager: sm,
		tagRegistry:     tr,
		logger:          logger.WithField("component", "scheduler"),
	}
}

// SetOTel attaches OTel instruments for distributed tracing and metrics.
func (s *Scheduler) SetOTel(c *fcrotel.Client, resumeRouting metric.Int64Counter) {
	s.otelClient = c
	s.sessionResumeRoutingCounter = resumeRouting
}

// SetConfigCache sets the in-memory config cache for fast workload config lookups.
func (s *Scheduler) SetConfigCache(cc *ConfigCache) {
	s.configCache = cc
}

// getHostConn returns a cached gRPC connection to the given address, creating
// one if needed. Connections are long-lived and multiplexed.
func (s *Scheduler) getHostConn(address string) (*grpc.ClientConn, error) {
	if v, ok := s.connPool.Load(address); ok {
		return v.(*grpc.ClientConn), nil
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if s.otelClient != nil {
		opts = append(opts, grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(s.otelClient.TracerProvider),
			otelgrpc.WithMeterProvider(s.otelClient.MeterProvider),
		)))
	}
	conn, err := grpc.NewClient(address, opts...)
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
	RequestID           string
	WorkloadKey         string
	Labels              map[string]string
	SessionID           string
	VCPUs               int
	MemoryMB            int
	SnapshotTag         string
	NetworkPolicyPreset string
	NetworkPolicyJSON   string
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
	var taggedSnapshotVersion string

	// Look up config for fairness checks, start_command, tier, and TTL/auto_pause.
	// Uses in-memory cache first, falls back to DB on miss.
	var tierName string
	var startCmd *snapshot.StartCommand
	var runnerTTLSeconds int
	var autoPause bool
	var configNetworkPolicyPreset string
	var configNetworkPolicyJSON string
	if workloadKey != "" {
		var wc *WorkloadConfig
		if s.configCache != nil {
			wc = s.configCache.GetWorkloadConfig(ctx, workloadKey)
		}
		if wc != nil {
			tierName = wc.Tier
			startCmd = wc.StartCommand
			runnerTTLSeconds = wc.RunnerTTLSeconds
			autoPause = wc.AutoPause
			configNetworkPolicyPreset = wc.NetworkPolicyPreset
			configNetworkPolicyJSON = wc.NetworkPolicyJSON
			if wc.MaxConcurrentRunners > 0 && s.db != nil {
				var currentCount int
				_ = s.db.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM runners WHERE workload_key = $1 AND status IN ('running','busy','initializing')
				`, workloadKey).Scan(&currentCount)
				if currentCount >= wc.MaxConcurrentRunners {
					return nil, fmt.Errorf("workload_key %s at max concurrent runners (%d/%d)", workloadKey, currentCount, wc.MaxConcurrentRunners)
				}
			}
		} else if s.db != nil {
			// No cache or cache miss: fall back to DB
			var maxConcurrent int
			var startCommandJSON sql.NullString
			var tierCol sql.NullString
			var ttlCol sql.NullInt64
			var autoPauseCol sql.NullBool
			var npPreset sql.NullString
			var npJSON sql.NullString

			err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_runners, start_command, tier, runner_ttl_seconds, auto_pause, network_policy_preset, network_policy FROM layered_configs WHERE leaf_workload_key = $1`, workloadKey).Scan(&maxConcurrent, &startCommandJSON, &tierCol, &ttlCol, &autoPauseCol, &npPreset, &npJSON)
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
						s.logger.WithError(err).Warn("Failed to parse start_command from config")
						startCmd = nil
					}
				}
				if ttlCol.Valid {
					runnerTTLSeconds = int(ttlCol.Int64)
				}
				if autoPauseCol.Valid {
					autoPause = autoPauseCol.Bool
				}
				if npPreset.Valid {
					configNetworkPolicyPreset = npPreset.String
				}
				if npJSON.Valid {
					configNetworkPolicyJSON = npJSON.String
				}
			}
		}
	}

	if req.SnapshotTag != "" {
		if workloadKey == "" {
			return nil, fmt.Errorf("snapshot tag %q requires a workload_key", req.SnapshotTag)
		}
		if s.tagRegistry == nil {
			return nil, fmt.Errorf("snapshot tag %q not found for workload %q", req.SnapshotTag, workloadKey)
		}
		v, err := s.tagRegistry.ResolveTagVersion(ctx, workloadKey, req.SnapshotTag)
		if err != nil {
			return nil, fmt.Errorf("snapshot tag %q not found for workload %q", req.SnapshotTag, workloadKey)
		}
		taggedSnapshotVersion = v
		s.logger.WithFields(logrus.Fields{
			"workload_key": workloadKey,
			"snapshot_tag": req.SnapshotTag,
			"version":      v,
		}).Info("Resolved snapshot_tag to version")
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
			for _, h := range eligible {
				if h.ID == sessionHostID {
					host = h
					s.logger.WithFields(logrus.Fields{
						"session_id": req.SessionID,
						"host_id":    sessionHostID,
					}).Info("Session sticky routing: using original host")
					if s.sessionResumeRoutingCounter != nil {
						s.sessionResumeRoutingCounter.Add(ctx, 1, metric.WithAttributes(
							fcrotel.AttrRouting.String(fcrotel.RoutingSameHost),
						))
					}
					break
				}
			}
			if host == nil {
				s.logger.WithFields(logrus.Fields{
					"session_id":      req.SessionID,
					"original_host":   sessionHostID,
					"available_hosts": len(eligible),
				}).Warn("Session sticky host not available, falling back to best-fit")
				if s.sessionResumeRoutingCounter != nil {
					s.sessionResumeRoutingCounter.Add(ctx, 1, metric.WithAttributes(
						fcrotel.AttrRouting.String(fcrotel.RoutingCrossHost),
					))
				}
			}
		}
	}

	// Fall back to workload-key cache affinity scoring
	if host == nil {
		host = s.selectBestHostForWorkloadKey(eligible, workloadKey)
	}
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

	// Connect to host agent (pooled connection)
	conn, err := s.getHostConn(host.GRPCAddress)
	if err != nil {
		return nil, err
	}

	// Create host agent client
	client := pb.NewHostAgentClient(conn)

	// Resolve the desired snapshot version for this workload_key + host
	var snapshotVersion string

	if taggedSnapshotVersion != "" {
		snapshotVersion = taggedSnapshotVersion
	}

	if snapshotVersion == "" && workloadKey != "" && s.snapshotManager != nil && s.snapshotManager.db != nil {
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
		RequestId:       req.RequestID,
		Labels:          req.Labels,
		WorkloadKey:     workloadKey,
		SessionId:       req.SessionID,
		SnapshotVersion: snapshotVersion,
		TtlSeconds:      int32(runnerTTLSeconds),
		AutoPause:       autoPause,
	}
	// Pass network policy fields via labels (proto fields 17-18 not in
	// generated wire descriptor; labels are serialized reliably).
	// Explicit request values take precedence; fall back to the config-stored policy.
	effectiveNPPreset := req.NetworkPolicyPreset
	if effectiveNPPreset == "" {
		effectiveNPPreset = configNetworkPolicyPreset
	}
	effectiveNPJSON := req.NetworkPolicyJSON
	if effectiveNPJSON == "" {
		effectiveNPJSON = configNetworkPolicyJSON
	}
	if effectiveNPPreset != "" || effectiveNPJSON != "" {
		if protoReq.Labels == nil {
			protoReq.Labels = make(map[string]string)
		}
		if effectiveNPPreset != "" {
			protoReq.Labels["_network_policy_preset"] = effectiveNPPreset
		}
		if effectiveNPJSON != "" {
			protoReq.Labels["_network_policy_json"] = effectiveNPJSON
		}
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
			Env:        startCmd.Env,
			RunAs:      startCmd.RunAs,
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
			HostAddress: host.HTTPAddress,
			Error:       resp.Error,
		}, fmt.Errorf("host agent returned error: %s", resp.Error)
	}

	// Register runner in our registry
	if resp.Runner != nil {
		if err := s.hostRegistry.AddRunner(ctx, &Runner{
			ID:               resp.Runner.Id,
			HostID:           host.ID,
			InternalIP:       resp.Runner.InternalIp,
			Status:           "busy",
			WorkloadKey:      workloadKey,
			ReservedCPU:      effectiveCPU,
			ReservedMemoryMB: tier.MemoryMB,
		}); err != nil {
			s.logger.WithError(err).Warn("Failed to register runner in control plane registry")
		}
	}

	return &AllocateRunnerResponse{
		RunnerID:    resp.Runner.GetId(),
		HostID:      host.ID,
		HostAddress: host.HTTPAddress,
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
	if err := s.hostRegistry.RemoveRunner(runnerID); err != nil {
		return err
	}

	// Roll back optimistic resource reservation made at allocate time.
	// Without this, UsedCPU/Memory accumulates until the next heartbeat
	// corrects it, causing spurious "no available hosts" under rapid
	// allocate/release cycles.
	if runner.ReservedCPU > 0 || runner.ReservedMemoryMB > 0 {
		s.hostRegistry.mu.Lock()
		if host != nil {
			host.UsedCPUMillicores -= runner.ReservedCPU
			host.UsedMemoryMB -= runner.ReservedMemoryMB
			if host.UsedCPUMillicores < 0 {
				host.UsedCPUMillicores = 0
			}
			if host.UsedMemoryMB < 0 {
				host.UsedMemoryMB = 0
			}
		}
		s.hostRegistry.mu.Unlock()
	}

	// Clean up session_snapshots row so stale entries don't accumulate.
	if s.db != nil {
		if _, dbErr := s.db.ExecContext(ctx, `DELETE FROM session_snapshots WHERE runner_id = $1`, runnerID); dbErr != nil {
			s.logger.WithError(dbErr).WithField("runner_id", runnerID).Warn("Failed to clean up session_snapshots on release")
		}
	}

	return nil
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
