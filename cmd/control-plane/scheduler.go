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

const schedulerRequestTTL = 5 * time.Minute

type recentSchedulerAllocation struct {
	resp      *AllocateRunnerResponse
	allocTime time.Time
	err       error
	waitCh    chan struct{}
	inFlight  bool
}

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

	requestMu      sync.Mutex
	recentRequests map[string]*recentSchedulerAllocation
}

// NewScheduler creates a new scheduler
func NewScheduler(hr *HostRegistry, db *sql.DB, sm *SnapshotManager, tr *SnapshotTagRegistry, logger *logrus.Logger) *Scheduler {
	return &Scheduler{
		hostRegistry:    hr,
		db:              db,
		snapshotManager: sm,
		tagRegistry:     tr,
		logger:          logger.WithField("component", "scheduler"),
		recentRequests:  make(map[string]*recentSchedulerAllocation),
	}
}

func (s *Scheduler) beginIdempotentAllocation(reqID string) (*AllocateRunnerResponse, *recentSchedulerAllocation, bool) {
	if reqID == "" {
		return nil, nil, true
	}

	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	if alloc, ok := s.recentRequests[reqID]; ok {
		if alloc.inFlight {
			return nil, alloc, false
		}
		if time.Since(alloc.allocTime) <= schedulerRequestTTL && s.cachedAllocationStillValid(alloc) {
			return alloc.resp, nil, false
		}
		delete(s.recentRequests, reqID)
	}

	alloc := &recentSchedulerAllocation{
		allocTime: time.Now(),
		waitCh:    make(chan struct{}),
		inFlight:  true,
	}
	s.recentRequests[reqID] = alloc
	return nil, alloc, true
}

func (s *Scheduler) waitForIdempotentAllocation(ctx context.Context, reqID string, alloc *recentSchedulerAllocation) (*AllocateRunnerResponse, error) {
	if reqID == "" || alloc == nil {
		return nil, nil
	}

	select {
	case <-alloc.waitCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	if alloc.resp != nil && s.cachedAllocationStillValid(alloc) {
		return alloc.resp, nil
	}
	if alloc.err != nil {
		return nil, alloc.err
	}
	return nil, fmt.Errorf("allocation for request %q completed without a runner", reqID)
}

func (s *Scheduler) finishIdempotentAllocation(reqID string, alloc *recentSchedulerAllocation, resp *AllocateRunnerResponse, err error) {
	if reqID == "" || alloc == nil {
		return
	}

	s.requestMu.Lock()
	if err == nil && resp != nil {
		alloc.resp = resp
		alloc.allocTime = time.Now()
	} else {
		alloc.err = err
		delete(s.recentRequests, reqID)
	}
	alloc.inFlight = false
	waitCh := alloc.waitCh
	s.requestMu.Unlock()

	if waitCh != nil {
		close(waitCh)
	}
}

func (s *Scheduler) cachedAllocationStillValid(alloc *recentSchedulerAllocation) bool {
	if alloc == nil || alloc.resp == nil || alloc.resp.RunnerID == "" {
		return false
	}
	if s.hostRegistry == nil {
		return true
	}
	_, err := s.hostRegistry.GetRunner(alloc.resp.RunnerID)
	return err == nil
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
func (s *Scheduler) AllocateRunner(ctx context.Context, req AllocateRunnerRequest) (_ *AllocateRunnerResponse, retErr error) {
	var idempotentAlloc *recentSchedulerAllocation
	var allocatedResp *AllocateRunnerResponse

	if existing, alloc, leader := s.beginIdempotentAllocation(req.RequestID); existing != nil {
		return existing, nil
	} else if !leader {
		return s.waitForIdempotentAllocation(ctx, req.RequestID, alloc)
	} else {
		idempotentAlloc = alloc
		defer func() {
			s.finishIdempotentAllocation(req.RequestID, idempotentAlloc, allocatedResp, retErr)
		}()
	}

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
	var resumeFromSessionConfig bool
	var authConfigJSON string
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
			authConfigJSON = wc.AuthConfigJSON
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
			var configJSON sql.NullString

			err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_runners, start_command, tier, runner_ttl_seconds, auto_pause, network_policy_preset, network_policy, config_json FROM layered_configs WHERE leaf_workload_key = $1 ORDER BY created_at DESC LIMIT 1`, workloadKey).Scan(&maxConcurrent, &startCommandJSON, &tierCol, &ttlCol, &autoPauseCol, &npPreset, &npJSON, &configJSON)
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
				if configJSON.Valid {
					authConfigJSON = extractAuthConfigJSON(configJSON.String)
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

	// Session stickiness: if this is a session resume, prefer the host where
	// the session was paused. This keeps the LRU chunk cache warm and avoids
	// cold GCS fetches on resume. When resuming a suspended session, also use
	// the TTL/network policy persisted on that session instead of re-reading
	// mutable layered-config defaults by workload key.
	var host *Host
	var sessionHostID string
	var sessionRestoreMeta sql.NullString
	if req.SessionID != "" && s.db != nil {
		var status string
		var sessionTTL sql.NullInt64
		var sessionAutoPause sql.NullBool
		var sessionNPPreset sql.NullString
		var sessionNPJSON sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT host_id, status, runner_ttl_seconds, auto_pause, network_policy_preset, network_policy, restore_metadata
			 FROM session_snapshots WHERE session_id = $1`,
			req.SessionID).Scan(&sessionHostID, &status, &sessionTTL, &sessionAutoPause, &sessionNPPreset, &sessionNPJSON, &sessionRestoreMeta)
		if err == nil && status == "suspended" && sessionHostID != "" {
			resumeFromSessionConfig = true
			if sessionTTL.Valid {
				runnerTTLSeconds = int(sessionTTL.Int64)
			} else {
				runnerTTLSeconds = 0
			}
			autoPause = sessionAutoPause.Valid && sessionAutoPause.Bool
			configNetworkPolicyPreset = ""
			if sessionNPPreset.Valid {
				configNetworkPolicyPreset = sessionNPPreset.String
			}
			configNetworkPolicyJSON = ""
			if sessionNPJSON.Valid {
				configNetworkPolicyJSON = sessionNPJSON.String
			}
		}
	}

	effectiveCPU := tiers.EffectiveCPUMillicores(tier)
	effectiveMemoryMB := tier.MemoryMB
	stickySelected := false
	stickyFallback := false

	s.hostRegistry.mu.Lock()
	var available []*Host
	for _, h := range s.hostRegistry.hosts {
		usedCPU, usedMem := s.hostRegistry.effectiveUsageLocked(h)
		if h.Status != "ready" {
			continue
		}
		if time.Since(h.LastHeartbeat) >= 60*time.Second {
			continue
		}
		if h.TotalCPUMillicores > 0 &&
			(h.TotalCPUMillicores-usedCPU) > 0 &&
			(h.TotalMemoryMB-usedMem) > 0 {
			available = append(available, h)
		}
	}
	if len(available) == 0 {
		s.hostRegistry.mu.Unlock()
		retErr = fmt.Errorf("no available hosts")
		return nil, retErr
	}

	candidateHosts := available
	var eligible []*Host
	for _, h := range available {
		usedCPU, usedMem := s.hostRegistry.effectiveUsageLocked(h)
		if canFitWorkloadWithUsage(h, usedCPU, usedMem, tier) {
			eligible = append(eligible, h)
		}
	}
	if len(eligible) > 0 {
		candidateHosts = eligible
	}

	if sessionHostID != "" {
		for _, h := range candidateHosts {
			if h.ID == sessionHostID {
				host = h
				stickySelected = true
				break
			}
		}
		stickyFallback = host == nil
		if stickyFallback && !sessionRestoreMetadataSupportsCrossHost(sessionRestoreMeta.String) {
			s.hostRegistry.mu.Unlock()
			retErr = fmt.Errorf("original session host unavailable and session is not cross-host resumable yet")
			return nil, retErr
		}
	}

	if host == nil {
		var bestScore float64
		for _, h := range candidateHosts {
			usedCPU, usedMem := s.hostRegistry.effectiveUsageLocked(h)
			score := s.scoreHostForWorkloadKeyWithUsage(h, workloadKey, usedCPU, usedMem)
			if host == nil || score > bestScore {
				host = h
				bestScore = score
			}
		}
	}
	if host == nil {
		s.hostRegistry.mu.Unlock()
		retErr = fmt.Errorf("no suitable host found")
		return nil, retErr
	}

	host.PendingCPUMillicores += effectiveCPU
	host.PendingMemoryMB += effectiveMemoryMB
	s.hostRegistry.mu.Unlock()

	if stickySelected {
		s.logger.WithFields(logrus.Fields{
			"session_id": req.SessionID,
			"host_id":    sessionHostID,
		}).Info("Session sticky routing: using original host")
		if s.sessionResumeRoutingCounter != nil {
			s.sessionResumeRoutingCounter.Add(ctx, 1, metric.WithAttributes(
				fcrotel.AttrRouting.String(fcrotel.RoutingSameHost),
			))
		}
	} else if stickyFallback {
		s.logger.WithFields(logrus.Fields{
			"session_id":      req.SessionID,
			"original_host":   sessionHostID,
			"available_hosts": len(candidateHosts),
		}).Warn("Session sticky host not available, falling back to best-fit")
		if s.sessionResumeRoutingCounter != nil {
			s.sessionResumeRoutingCounter.Add(ctx, 1, metric.WithAttributes(
				fcrotel.AttrRouting.String(fcrotel.RoutingCrossHost),
			))
		}
	}

	s.logger.WithFields(logrus.Fields{
		"host_id":       host.ID,
		"instance_name": host.InstanceName,
		"grpc_address":  host.GRPCAddress,
	}).Debug("Selected host")

	// Connect to host agent (pooled connection)
	conn, err := s.getHostConn(host.GRPCAddress)
	if err != nil {
		s.hostRegistry.releasePendingReservation(host.ID, effectiveCPU, effectiveMemoryMB)
		retErr = err
		return nil, retErr
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
	// Fresh allocations may apply request-level overrides, but resumed sessions
	// must keep the policy persisted with that session.
	effectiveNPPreset := configNetworkPolicyPreset
	effectiveNPJSON := configNetworkPolicyJSON
	if !resumeFromSessionConfig {
		if req.NetworkPolicyPreset != "" {
			effectiveNPPreset = req.NetworkPolicyPreset
		}
		if req.NetworkPolicyJSON != "" {
			effectiveNPJSON = req.NetworkPolicyJSON
		}
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
	// Pass auth config via labels so the host agent can start the auth proxy.
	s.logger.WithFields(logrus.Fields{
		"workload_key":    workloadKey,
		"auth_config_len": len(authConfigJSON),
		"has_auth_config": authConfigJSON != "",
	}).Info("DEBUG: auth config for allocation")
	if authConfigJSON != "" {
		if protoReq.Labels == nil {
			protoReq.Labels = make(map[string]string)
		}
		protoReq.Labels["_auth_config_json"] = authConfigJSON
	}
	if sessionRestoreMeta.Valid && sessionRestoreMeta.String != "" {
		if protoReq.Labels == nil {
			protoReq.Labels = make(map[string]string)
		}
		protoReq.Labels["_session_metadata_json"] = sessionRestoreMeta.String
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
		s.hostRegistry.releasePendingReservation(host.ID, effectiveCPU, effectiveMemoryMB)
		s.logger.WithError(err).WithField("host", host.InstanceName).Error("gRPC AllocateRunner failed")
		retErr = fmt.Errorf("host agent AllocateRunner failed: %w", err)
		return nil, retErr
	}

	if resp.Error != "" {
		s.hostRegistry.releasePendingReservation(host.ID, effectiveCPU, effectiveMemoryMB)
		retErr = fmt.Errorf("host agent returned error: %s", resp.Error)
		return &AllocateRunnerResponse{
			HostID:      host.ID,
			HostAddress: host.HTTPAddress,
			Error:       resp.Error,
		}, retErr
	}

	// Register runner in our registry
	if resp.Runner != nil {
		if err := s.hostRegistry.AddRunner(ctx, &Runner{
			ID:                  resp.Runner.Id,
			HostID:              host.ID,
			InternalIP:          resp.Runner.InternalIp,
			Status:              "busy",
			WorkloadKey:         workloadKey,
			RunnerTTLSeconds:    runnerTTLSeconds,
			AutoPause:           autoPause,
			NetworkPolicyPreset: effectiveNPPreset,
			NetworkPolicyJSON:   effectiveNPJSON,
			ReservedCPU:         effectiveCPU,
			ReservedMemoryMB:    tier.MemoryMB,
		}); err != nil {
			s.logger.WithError(err).Warn("Failed to register runner in control plane registry")
		}
	}
	s.hostRegistry.releasePendingReservation(host.ID, effectiveCPU, effectiveMemoryMB)

	allocatedResp = &AllocateRunnerResponse{
		RunnerID:    resp.Runner.GetId(),
		HostID:      host.ID,
		HostAddress: host.HTTPAddress,
		InternalIP:  resp.Runner.GetInternalIp(),
		SessionID:   resp.GetSessionId(),
		Resumed:     resp.GetResumed(),
	}
	return allocatedResp, nil
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
		usedCPU, usedMem := h.UsedCPUMillicores, h.UsedMemoryMB
		if s.hostRegistry != nil {
			s.hostRegistry.mu.RLock()
			usedCPU, usedMem = s.hostRegistry.effectiveUsageLocked(h)
			s.hostRegistry.mu.RUnlock()
		}
		score := s.scoreHostForWorkloadKeyWithUsage(h, workloadKey, usedCPU, usedMem)
		scored = append(scored, scoredHost{host: h, score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	return scored[0].host
}

// scoreHostForWorkloadKey calculates a score for a host with workload-key cache affinity.
func (s *Scheduler) scoreHostForWorkloadKey(h *Host, workloadKey string) float64 {
	return s.scoreHostForWorkloadKeyWithUsage(h, workloadKey, h.UsedCPUMillicores, h.UsedMemoryMB)
}

func (s *Scheduler) scoreHostForWorkloadKeyWithUsage(h *Host, workloadKey string, usedCPU, usedMem int) float64 {
	score := s.scoreHostWithUsage(h, usedCPU, usedMem)

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

func canFitWorkloadWithUsage(h *Host, usedCPU, usedMem int, t tiers.Tier) bool {
	if h.TotalCPUMillicores == 0 {
		return false
	}
	effectiveCPU := tiers.EffectiveCPUMillicores(t)
	return (h.TotalCPUMillicores-usedCPU) >= effectiveCPU &&
		(h.TotalMemoryMB-usedMem) >= t.MemoryMB
}

// scoreHost calculates a base score for a host using resource-based metrics.
func (s *Scheduler) scoreHost(h *Host) float64 {
	return s.scoreHostWithUsage(h, h.UsedCPUMillicores, h.UsedMemoryMB)
}

func (s *Scheduler) scoreHostWithUsage(h *Host, usedCPU, usedMem int) float64 {
	var score float64

	// Prefer hosts with idle runners (cache warmth)
	score += float64(h.IdleRunners) * 10

	if h.TotalCPUMillicores > 0 {
		cpuFree := float64(h.TotalCPUMillicores-usedCPU) / float64(h.TotalCPUMillicores)
		memFree := float64(h.TotalMemoryMB-usedMem) / float64(h.TotalMemoryMB)
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
