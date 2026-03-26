package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
	fcrotel "github.com/rahul-roy-glean/capsule/pkg/otel"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
	"github.com/rahul-roy-glean/capsule/pkg/tiers"
)

const schedulerRequestTTL = 5 * time.Minute

const sessionMaxAgeLabelKey = "_session_max_age_seconds"

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
	configCache     *ConfigCache
	logger          *logrus.Entry

	sessionResumeRoutingCounter metric.Int64Counter
	allocationCounter           metric.Int64Counter
	allocationLatency           metric.Float64Histogram
	placementCounter            metric.Int64Counter
	otelClient                  *fcrotel.Client

	// connPool caches gRPC connections to host agents, keyed by address.
	// gRPC connections are multiplexed and designed to be long-lived, so
	// reusing them avoids TCP+TLS handshake latency on every RPC.
	connPool sync.Map // map[string]*grpc.ClientConn

	requestMu      sync.Mutex
	recentRequests map[string]*recentSchedulerAllocation
}

// NewScheduler creates a new scheduler
func NewScheduler(hr *HostRegistry, db *sql.DB, sm *SnapshotManager, logger *logrus.Logger) *Scheduler {
	return &Scheduler{
		hostRegistry:    hr,
		db:              db,
		snapshotManager: sm,
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

func setInternalAllocateLabels(labels map[string]string, sessionMaxAgeSeconds int) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[sessionMaxAgeLabelKey] = strconv.Itoa(sessionMaxAgeSeconds)
	return labels
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
func (s *Scheduler) SetOTel(c *fcrotel.Client, resumeRouting metric.Int64Counter, allocationCounter metric.Int64Counter, allocationLatency metric.Float64Histogram, placementCounter metric.Int64Counter) {
	s.otelClient = c
	s.sessionResumeRoutingCounter = resumeRouting
	s.allocationCounter = allocationCounter
	s.allocationLatency = allocationLatency
	s.placementCounter = placementCounter
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
	Source              string
	Labels              map[string]string
	SessionID           string
	VCPUs               int
	MemoryMB            int
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

func (s *Scheduler) recordAllocationTelemetry(ctx context.Context, duration time.Duration, workloadKey, source, result, reason string) {
	if source == "" {
		source = "unknown"
	}
	attrs := []attribute.KeyValue{
		fcrotel.AttrResult.String(result),
		fcrotel.AttrSource.String(source),
	}
	if workloadKey != "" {
		attrs = append(attrs, fcrotel.AttrWorkloadKey.String(workloadKey))
	}
	if reason != "" {
		attrs = append(attrs, fcrotel.AttrReason.String(reason))
	}
	if s.allocationCounter != nil {
		s.allocationCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
	if s.allocationLatency != nil {
		s.allocationLatency.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	}
}

func (s *Scheduler) recordPlacementTelemetry(ctx context.Context, workloadKey, source, selectionReason, cacheState string) {
	if source == "" {
		source = "unknown"
	}
	attrs := []attribute.KeyValue{
		fcrotel.AttrSource.String(source),
		fcrotel.AttrSelectionReason.String(selectionReason),
		fcrotel.AttrCacheState.String(cacheState),
	}
	if workloadKey != "" {
		attrs = append(attrs, fcrotel.AttrWorkloadKey.String(workloadKey))
	}
	if s.placementCounter != nil {
		s.placementCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func (s *Scheduler) recordResumeRoutingTelemetry(ctx context.Context, workloadKey, source, routing string) {
	if source == "" {
		source = "unknown"
	}
	attrs := []attribute.KeyValue{
		fcrotel.AttrRouting.String(routing),
		fcrotel.AttrSource.String(source),
	}
	if workloadKey != "" {
		attrs = append(attrs, fcrotel.AttrWorkloadKey.String(workloadKey))
	}
	if s.sessionResumeRoutingCounter != nil {
		s.sessionResumeRoutingCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func placementCacheState(h *Host, workloadKey, targetVersion string) string {
	if workloadKey == "" || h == nil || h.LoadedManifests == nil {
		return "miss"
	}
	version, ok := h.LoadedManifests[workloadKey]
	if !ok {
		return "miss"
	}
	if targetVersion == "" {
		return "warm"
	}
	if targetVersion != "" && version == targetVersion {
		return "exact"
	}
	return "stale"
}

// AllocateRunner allocates a runner on the best available host
func (s *Scheduler) AllocateRunner(ctx context.Context, req AllocateRunnerRequest) (_ *AllocateRunnerResponse, retErr error) {
	allocStart := time.Now()
	failureReason := ""
	var idempotentAlloc *recentSchedulerAllocation
	var allocatedResp *AllocateRunnerResponse
	defer func() {
		result := fcrotel.ResultSuccess
		if retErr != nil {
			result = fcrotel.ResultFailure
		}
		s.recordAllocationTelemetry(ctx, time.Since(allocStart), req.WorkloadKey, req.Source, result, failureReason)
	}()

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

	// Ensure every allocation has a session_id for pause/resume support.
	if req.SessionID == "" {
		req.SessionID = uuid.New().String()
	}

	s.logger.WithFields(logrus.Fields{
		"request_id":   req.RequestID,
		"workload_key": req.WorkloadKey,
		"session_id":   req.SessionID,
	}).Info("Allocating runner")

	// Derive repo slug for multi-repo support
	workloadKey := req.WorkloadKey
	var taggedSnapshotVersion string

	// Session resume: if a suspended session exists, use its original
	// workload_key so that config lookup (tier, snapshot version, auth, etc.)
	// matches the snapshot the session was paused with.
	// If the config's base image has changed (new active workload_key), detect
	// the migration and pass info to the host agent via labels.
	var sessionHostID string
	var resumeFromSessionConfig bool
	var sessionTTL sql.NullInt64
	var sessionMaxAge sql.NullInt64
	var sessionAutoPause sql.NullBool
	var sessionNPPreset sql.NullString
	var sessionNPJSON sql.NullString
	var migrateFromWorkloadKey string // set when base image migration is needed
	var migrateFromRunnerID string
	var resumeRunnerID string // runner ID for cross-host session resume
	if req.SessionID != "" && s.db != nil {
		var status string
		var sessionWorkloadKey sql.NullString
		var sessionConfigID sql.NullString
		var sessionRunnerID sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT host_id, status, workload_key, runner_ttl_seconds, session_max_age_seconds, auto_pause, network_policy_preset, network_policy, config_id, runner_id
			 FROM session_snapshots WHERE session_id = $1`,
			req.SessionID).Scan(&sessionHostID, &status, &sessionWorkloadKey, &sessionTTL, &sessionMaxAge, &sessionAutoPause, &sessionNPPreset, &sessionNPJSON, &sessionConfigID, &sessionRunnerID)
		if err == nil && status == "suspended" && sessionHostID != "" {
			resumeFromSessionConfig = true
			if sessionRunnerID.Valid && sessionRunnerID.String != "" {
				resumeRunnerID = sessionRunnerID.String
			}
			s.logger.WithFields(logrus.Fields{
				"session_id":        req.SessionID,
				"session_host_id":   sessionHostID,
				"session_runner_id": resumeRunnerID,
				"resume_from_cfg":   resumeFromSessionConfig,
			}).Info("DEBUG: session_snapshots lookup found suspended session")

			// Check for base image migration: if config_id is known, look up
			// the current active workload_key and compare.
			if sessionConfigID.Valid && sessionConfigID.String != "" && sessionWorkloadKey.Valid && sessionWorkloadKey.String != "" {
				var currentActiveWK sql.NullString
				s.db.QueryRowContext(ctx,
					`SELECT leaf_workload_key FROM config_workload_keys
					 WHERE config_id = $1 AND status = 'active' LIMIT 1`,
					sessionConfigID.String).Scan(&currentActiveWK)

				if currentActiveWK.Valid && currentActiveWK.String != "" && currentActiveWK.String != sessionWorkloadKey.String {
					// Base image migration detected: session was paused with old wk,
					// config now has a new active wk (new base image).
					migrateFromWorkloadKey = sessionWorkloadKey.String
					if sessionRunnerID.Valid {
						migrateFromRunnerID = sessionRunnerID.String
					}
					// Use the NEW workload_key for config lookup, scoring, and snapshot version
					workloadKey = currentActiveWK.String
					s.logger.WithFields(logrus.Fields{
						"session_id":    req.SessionID,
						"config_id":     sessionConfigID.String,
						"old_wk":        sessionWorkloadKey.String,
						"new_wk":        currentActiveWK.String,
						"old_runner_id": migrateFromRunnerID,
					}).Info("Base image migration detected: session will resume with new golden snapshot + old extension drives")
				} else {
					// No migration — use session's workload_key as before
					if sessionWorkloadKey.Valid && sessionWorkloadKey.String != "" {
						if workloadKey != "" && workloadKey != sessionWorkloadKey.String {
							s.logger.WithFields(logrus.Fields{
								"session_id":         req.SessionID,
								"requested_workload": workloadKey,
								"session_workload":   sessionWorkloadKey.String,
							}).Warn("Resume: overriding requested workload_key with session's original workload_key")
						}
						workloadKey = sessionWorkloadKey.String
					}
				}
			} else {
				// No config_id on session — fallback to original behavior
				if sessionWorkloadKey.Valid && sessionWorkloadKey.String != "" {
					if workloadKey != "" && workloadKey != sessionWorkloadKey.String {
						s.logger.WithFields(logrus.Fields{
							"session_id":         req.SessionID,
							"requested_workload": workloadKey,
							"session_workload":   sessionWorkloadKey.String,
						}).Warn("Resume: overriding requested workload_key with session's original workload_key")
					}
					workloadKey = sessionWorkloadKey.String
				}
			}
		}
	}

	// Look up config for fairness checks, start_command, tier, and TTL/auto_pause.
	// Uses in-memory cache first, falls back to DB on miss.
	var tierName string
	var startCmd *snapshot.StartCommand
	var runnerTTLSeconds int
	var sessionMaxAgeSeconds int
	var autoPause bool
	var configNetworkPolicyPreset string
	var configNetworkPolicyJSON string
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
			sessionMaxAgeSeconds = wc.SessionMaxAgeSeconds
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
					failureReason = "max_concurrent_runners"
					return nil, fmt.Errorf("workload_key %s at max concurrent runners (%d/%d)", workloadKey, currentCount, wc.MaxConcurrentRunners)
				}
			}
		} else if s.db != nil {
			// No cache or cache miss: fall back to DB
			var maxConcurrent int
			var startCommandJSON sql.NullString
			var tierCol sql.NullString
			var ttlCol sql.NullInt64
			var sessionMaxAgeCol sql.NullInt64
			var autoPauseCol sql.NullBool
			var npPreset sql.NullString
			var npJSON sql.NullString
			var configJSON sql.NullString

			err := s.db.QueryRowContext(ctx, `SELECT max_concurrent_runners, start_command, tier, runner_ttl_seconds, session_max_age_seconds, auto_pause, network_policy_preset, network_policy, config_json FROM layered_configs WHERE leaf_workload_key = $1 ORDER BY created_at DESC LIMIT 1`, workloadKey).Scan(&maxConcurrent, &startCommandJSON, &tierCol, &ttlCol, &sessionMaxAgeCol, &autoPauseCol, &npPreset, &npJSON, &configJSON)
			if err != nil {
				// Fallback: workload_key may be from a previous config version (draining).
				err = s.db.QueryRowContext(ctx, `
					SELECT lc.max_concurrent_runners, lc.start_command, lc.tier, lc.runner_ttl_seconds, lc.session_max_age_seconds, lc.auto_pause, lc.network_policy_preset, lc.network_policy, lc.config_json
					FROM config_workload_keys cwk
					JOIN layered_configs lc ON lc.config_id = cwk.config_id
					WHERE cwk.leaf_workload_key = $1
					ORDER BY lc.created_at DESC LIMIT 1`, workloadKey).Scan(&maxConcurrent, &startCommandJSON, &tierCol, &ttlCol, &sessionMaxAgeCol, &autoPauseCol, &npPreset, &npJSON, &configJSON)
			}
			if err != nil {
				failureReason = "workload_key_not_found"
				return nil, fmt.Errorf("workload_key %s not found", workloadKey)
			}
			if tierCol.Valid && tierCol.String != "" {
				tierName = tierCol.String
			}
			if maxConcurrent > 0 {
				var currentCount int
				_ = s.db.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM runners WHERE workload_key = $1 AND status IN ('running','busy','initializing')
				`, workloadKey).Scan(&currentCount)
				if currentCount >= maxConcurrent {
					failureReason = "max_concurrent_runners"
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
			if sessionMaxAgeCol.Valid {
				sessionMaxAgeSeconds = int(sessionMaxAgeCol.Int64)
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

	// Resolve tier (default to "m")
	if tierName == "" {
		tierName = tiers.DefaultTier
	}
	tier, _ := tiers.Lookup(tierName)

	// Session stickiness: apply TTL/network policy overrides from the
	// suspended session (the lookup and workload_key override already
	// happened above, before config resolution).
	if resumeFromSessionConfig {
		if sessionTTL.Valid {
			runnerTTLSeconds = int(sessionTTL.Int64)
		} else {
			runnerTTLSeconds = 0
		}
		if sessionMaxAge.Valid {
			sessionMaxAgeSeconds = int(sessionMaxAge.Int64)
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

	effectiveCPU := tiers.EffectiveCPUMillicores(tier)
	effectiveMemoryMB := tier.MemoryMB
	stickySelected := false
	stickyFallback := false

	// Pre-scoring version resolution: resolve per-host target versions so
	// scoring reflects the version each host will actually boot, not just
	// the fleet-wide latest. This matters during canary or per-host override
	// rollouts where different hosts have different desired versions.
	var scoringVersion string
	var hostVersionOverrides map[string]string
	if taggedSnapshotVersion != "" {
		scoringVersion = taggedSnapshotVersion
	} else if workloadKey != "" && s.snapshotManager != nil && s.snapshotManager.db != nil {
		var fleetAssigned string
		fleetAssigned, hostVersionOverrides = s.snapshotManager.GetTargetVersionsByWorkloadKey(ctx, workloadKey)
		if fleetAssigned != "" {
			scoringVersion = fleetAssigned
		}
	}
	if scoringVersion == "" && workloadKey != "" && s.snapshotManager != nil {
		scoringVersion = s.snapshotManager.GetCurrentVersionForKey(workloadKey)
	}

	// --- Step A: Snapshot under RLock ---
	// Take a read lock to capture candidate host data. This allows heartbeats
	// (which take a write lock) to proceed without blocking.
	type candidateSnapshot struct {
		host    *Host
		usedCPU int
		usedMem int
		score   float64
	}

	s.hostRegistry.mu.RLock()
	var anyAvailable bool
	var candidates []candidateSnapshot
	for _, h := range s.hostRegistry.hosts {
		if h.Status != "ready" {
			continue
		}
		if time.Since(h.LastHeartbeat) >= 60*time.Second {
			continue
		}
		usedCPU, usedMem := s.hostRegistry.effectiveUsageLocked(h)
		if h.TotalCPUMillicores <= 0 ||
			(h.TotalCPUMillicores-usedCPU) <= 0 ||
			(h.TotalMemoryMB-usedMem) <= 0 {
			continue
		}
		anyAvailable = true
		if canFitWorkloadWithUsage(h, usedCPU, usedMem, tier) {
			candidates = append(candidates, candidateSnapshot{
				host:    h,
				usedCPU: usedCPU,
				usedMem: usedMem,
			})
		}
	}
	s.hostRegistry.mu.RUnlock()

	if !anyAvailable {
		s.hostRegistry.RecordAllocFailure()
		failureReason = "no_available_hosts"
		retErr = fmt.Errorf("no available hosts")
		return nil, retErr
	}
	if len(candidates) == 0 {
		s.hostRegistry.RecordAllocFailure()
		failureReason = "insufficient_capacity"
		retErr = fmt.Errorf("no host with sufficient capacity for tier %s (need %d CPU, %d MB memory)", tierName, effectiveCPU, effectiveMemoryMB)
		return nil, retErr
	}

	// --- Step B: Score off-lock ---
	// Scoring touches no shared mutable state; the captured usage values and
	// host pointers give a consistent-enough view for ranking.
	// Per-host version overrides ensure scoring matches the version each host
	// will actually boot (important during canary rollouts).
	for i := range candidates {
		targetVersion := scoringVersion
		if hostVersionOverrides != nil {
			if override, ok := hostVersionOverrides[candidates[i].host.ID]; ok {
				targetVersion = override
			}
		}
		candidates[i].score = s.scoreHostForWorkloadKeyWithUsage(
			candidates[i].host, workloadKey, targetVersion,
			candidates[i].usedCPU, candidates[i].usedMem,
		)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Apply session stickiness: prefer the original host for cache warmth.
	// Move the sticky host to the front of the ranked list so the retry loop
	// tries it first.
	if sessionHostID != "" {
		for i, c := range candidates {
			if c.host.ID == sessionHostID {
				stickySelected = true
				if i > 0 {
					entry := candidates[i]
					copy(candidates[1:i+1], candidates[:i])
					candidates[0] = entry
				}
				break
			}
		}
		stickyFallback = !stickySelected
	}

	if stickySelected {
		s.logger.WithFields(logrus.Fields{
			"session_id": req.SessionID,
			"host_id":    sessionHostID,
		}).Info("Session sticky routing: using original host")
		s.recordResumeRoutingTelemetry(ctx, workloadKey, req.Source, fcrotel.RoutingSameHost)
	} else if stickyFallback {
		s.logger.WithFields(logrus.Fields{
			"session_id":      req.SessionID,
			"original_host":   sessionHostID,
			"available_hosts": len(candidates),
		}).Warn("Session sticky host not available, falling back to best-fit")
		s.recordResumeRoutingTelemetry(ctx, workloadKey, req.Source, fcrotel.RoutingCrossHost)
	}

	// Pre-build proto request (host-independent parts).
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
	protoReq := &pb.AllocateRunnerRequest{
		RequestId:   req.RequestID,
		Labels:      setInternalAllocateLabels(req.Labels, sessionMaxAgeSeconds),
		WorkloadKey: workloadKey,
		SessionId:   req.SessionID,
		TtlSeconds:  int32(runnerTTLSeconds),
		AutoPause:   autoPause,
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
	// Pass base image migration info via labels so the host agent can do a
	// migrated fresh boot (new golden snapshot + old session extension drives).
	if migrateFromWorkloadKey != "" {
		if protoReq.Labels == nil {
			protoReq.Labels = make(map[string]string)
		}
		protoReq.Labels["_migrate_from_workload_key"] = migrateFromWorkloadKey
		protoReq.Labels["_migrate_from_runner_id"] = migrateFromRunnerID
	}
	// Pass cross-host session resume info as proper proto fields so the host
	// agent can skip the local SessionExists() check and use GCS fallback.
	if resumeFromSessionConfig && migrateFromWorkloadKey == "" && resumeRunnerID != "" {
		protoReq.RunnerId = resumeRunnerID
		protoReq.Resume = true
		s.logger.WithFields(logrus.Fields{
			"session_id":       req.SessionID,
			"resume_runner_id": resumeRunnerID,
		}).Info("DEBUG: setting resume=true on proto request")
	} else {
		s.logger.WithFields(logrus.Fields{
			"session_id":       req.SessionID,
			"resume_from_cfg":  resumeFromSessionConfig,
			"migrate_from_wk":  migrateFromWorkloadKey,
			"resume_runner_id": resumeRunnerID,
		}).Info("DEBUG: NOT setting resume=true on proto request")
	}
	protoReq.Resources = &pb.Resources{
		Vcpus:    int32(tier.VCPUs),
		MemoryMb: int32(tier.MemoryMB),
	}
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

	// --- Step C+D: Reserve under short Lock + gRPC call with host fallback ---
	// Try up to maxHostAttempts candidates. Each attempt takes a short write
	// lock to re-validate capacity and reserve resources, then makes the gRPC
	// call. On retryable failures the reservation is released and the next
	// candidate is tried.
	const maxHostAttempts = 3
	var lastErr error
	for i := 0; i < len(candidates) && i < maxHostAttempts; i++ {
		candidate := candidates[i]
		h := candidate.host

		s.logger.WithFields(logrus.Fields{
			"host_id":       h.ID,
			"instance_name": h.InstanceName,
			"grpc_address":  h.GRPCAddress,
			"attempt":       i + 1,
		}).Debug("Attempting host allocation")

		// Reserve under short Lock with re-validation
		s.hostRegistry.mu.Lock()
		liveHost := s.hostRegistry.hosts[h.ID]
		if liveHost == nil || liveHost.Status != "ready" || time.Since(liveHost.LastHeartbeat) >= 60*time.Second {
			s.hostRegistry.mu.Unlock()
			s.logger.WithField("host", h.InstanceName).Debug("Host no longer valid, trying next")
			continue
		}
		usedCPU, usedMem := s.hostRegistry.effectiveUsageLocked(liveHost)
		if !canFitWorkloadWithUsage(liveHost, usedCPU, usedMem, tier) {
			s.hostRegistry.mu.Unlock()
			s.logger.WithField("host", h.InstanceName).Debug("Host no longer has capacity, trying next")
			continue
		}
		liveHost.PendingCPUMillicores += effectiveCPU
		liveHost.PendingMemoryMB += effectiveMemoryMB
		s.hostRegistry.mu.Unlock()

		// Resolve host-specific snapshot version for the gRPC request
		var snapshotVersion string
		if taggedSnapshotVersion != "" {
			snapshotVersion = taggedSnapshotVersion
		}
		if snapshotVersion == "" && workloadKey != "" && s.snapshotManager != nil && s.snapshotManager.db != nil {
			desired, _ := s.snapshotManager.GetDesiredVersions(ctx, h.ID)
			if v, ok := desired[workloadKey]; ok {
				snapshotVersion = v
			}
		}
		if snapshotVersion == "" && workloadKey != "" && s.snapshotManager != nil {
			snapshotVersion = s.snapshotManager.GetCurrentVersionForKey(workloadKey)
		}
		protoReq.SnapshotVersion = snapshotVersion

		// Connect to host agent (pooled connection)
		conn, err := s.getHostConn(h.GRPCAddress)
		if err != nil {
			s.hostRegistry.releasePendingReservation(h.ID, effectiveCPU, effectiveMemoryMB)
			lastErr = err
			failureReason = "host_connect_error"
			s.logger.WithError(err).WithField("host", h.InstanceName).Warn("Failed to connect to host, trying next")
			continue
		}

		// Call host agent to allocate runner
		client := pb.NewHostAgentClient(conn)
		allocStart := time.Now()
		grpcCtx, grpcCancel := context.WithTimeout(ctx, 30*time.Second)
		resp, err := client.AllocateRunner(grpcCtx, protoReq)
		grpcCancel()
		allocDuration := time.Since(allocStart)
		if allocDuration > 5*time.Second {
			s.logger.WithFields(logrus.Fields{
				"host":        h.InstanceName,
				"duration_ms": allocDuration.Milliseconds(),
				"request_id":  req.RequestID,
			}).Warn("Slow runner allocation on host agent")
		}

		if err != nil {
			s.hostRegistry.releasePendingReservation(h.ID, effectiveCPU, effectiveMemoryMB)
			// Non-retryable: context cancelled/deadline exceeded
			if ctx.Err() != nil {
				failureReason = "context_cancelled"
				retErr = ctx.Err()
				return nil, retErr
			}
			// Non-retryable: invalid argument (bad request)
			st, _ := status.FromError(err)
			if st.Code() == codes.InvalidArgument {
				s.logger.WithError(err).WithField("host", h.InstanceName).Error("gRPC AllocateRunner failed (non-retryable)")
				failureReason = "host_rpc_invalid_argument"
				retErr = fmt.Errorf("host agent AllocateRunner failed: %w", err)
				return nil, retErr
			}
			// Retryable gRPC error (transport, unavailable, timeout, etc.)
			s.logger.WithError(err).WithField("host", h.InstanceName).Warn("gRPC AllocateRunner failed, trying next host")
			failureReason = "host_rpc_error"
			lastErr = fmt.Errorf("host agent AllocateRunner failed: %w", err)
			continue
		}

		if resp.Error != "" {
			s.hostRegistry.releasePendingReservation(h.ID, effectiveCPU, effectiveMemoryMB)
			if isRetryableHostError(resp.Error) {
				s.logger.WithFields(logrus.Fields{
					"host":  h.InstanceName,
					"error": resp.Error,
				}).Warn("Host agent error, trying next host")
				failureReason = "host_rejected_retryable"
				lastErr = fmt.Errorf("host agent returned error: %s", resp.Error)
				continue
			}
			// Non-retryable host error
			failureReason = "host_rejected_non_retryable"
			retErr = fmt.Errorf("host agent returned error: %s", resp.Error)
			return &AllocateRunnerResponse{
				HostID:      h.ID,
				HostAddress: h.HTTPAddress,
				Error:       resp.Error,
			}, retErr
		}

		// Success — register runner and return
		if resp.Runner != nil {
			if err := s.hostRegistry.AddRunner(ctx, &Runner{
				ID:                   resp.Runner.Id,
				HostID:               h.ID,
				InternalIP:           resp.Runner.InternalIp,
				Status:               "busy",
				WorkloadKey:          workloadKey,
				RunnerTTLSeconds:     runnerTTLSeconds,
				SessionMaxAgeSeconds: sessionMaxAgeSeconds,
				AutoPause:            autoPause,
				NetworkPolicyPreset:  effectiveNPPreset,
				NetworkPolicyJSON:    effectiveNPJSON,
				ReservedCPU:          effectiveCPU,
				ReservedMemoryMB:     tier.MemoryMB,
			}); err != nil {
				s.logger.WithError(err).Warn("Failed to register runner in control plane registry")
			}
		}
		s.hostRegistry.releasePendingReservation(h.ID, effectiveCPU, effectiveMemoryMB)
		selectionReason := "best_fit"
		if stickySelected && h.ID == sessionHostID {
			selectionReason = "sticky_same_host"
		} else if stickyFallback {
			selectionReason = "sticky_fallback"
		}
		s.recordPlacementTelemetry(ctx, workloadKey, req.Source, selectionReason, placementCacheState(h, workloadKey, protoReq.SnapshotVersion))

		allocatedResp = &AllocateRunnerResponse{
			RunnerID:    resp.Runner.GetId(),
			HostID:      h.ID,
			HostAddress: h.HTTPAddress,
			InternalIP:  resp.Runner.GetInternalIp(),
			SessionID:   resp.GetSessionId(),
			Resumed:     resp.GetResumed(),
		}

		// After successful migration allocation, update the session snapshot
		// so future resumes don't re-migrate. runner_id is reused from the old
		// session, so only workload_key and status need updating.
		if migrateFromWorkloadKey != "" && s.db != nil {
			_, _ = s.db.ExecContext(ctx,
				`UPDATE session_snapshots SET workload_key = $1, status = 'active', host_id = $2 WHERE session_id = $3`,
				workloadKey, h.ID, req.SessionID)
		}

		return allocatedResp, nil
	}

	// All host attempts exhausted — record the failure so the downscaler's
	// demand-driven scale-up path sees the signal, not just pre-scan capacity
	// shortages.
	s.hostRegistry.RecordAllocFailure()
	failureReason = "no_suitable_host_after_attempts"
	if lastErr != nil {
		retErr = lastErr
	} else {
		retErr = fmt.Errorf("no suitable host found after %d attempts", maxHostAttempts)
	}
	return nil, retErr
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
		score := s.scoreHostForWorkloadKeyWithUsage(h, workloadKey, "", usedCPU, usedMem)
		scored = append(scored, scoredHost{host: h, score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	return scored[0].host
}

// scoreHostForWorkloadKey calculates a score for a host with workload-key cache affinity.
func (s *Scheduler) scoreHostForWorkloadKey(h *Host, workloadKey string) float64 {
	return s.scoreHostForWorkloadKeyWithUsage(h, workloadKey, "", h.UsedCPUMillicores, h.UsedMemoryMB)
}

func (s *Scheduler) scoreHostForWorkloadKeyWithUsage(h *Host, workloadKey, targetVersion string, usedCPU, usedMem int) float64 {
	score := s.scoreHostWithUsage(h, usedCPU, usedMem)

	// Version-aware cache affinity scoring:
	// Exact version match gets highest bonus, stale version still gets partial
	// credit for chunk data warmth.
	if workloadKey != "" && h.LoadedManifests != nil {
		if version, ok := h.LoadedManifests[workloadKey]; ok {
			if targetVersion != "" && version == targetVersion {
				score += 100 // Exact version match
			} else {
				score += 20 // Stale cache, partial warmth from chunk data
			}
		}
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

// isRetryableHostError returns true if the host agent error message indicates
// a transient condition that warrants trying another host.
func isRetryableHostError(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	return strings.Contains(lower, "no slots available") ||
		strings.Contains(lower, "at capacity") ||
		strings.Contains(lower, "draining")
}

// scoreHost calculates a base score for a host using resource-based metrics.
func (s *Scheduler) scoreHost(h *Host) float64 {
	return s.scoreHostWithUsage(h, h.UsedCPUMillicores, h.UsedMemoryMB)
}

func (s *Scheduler) scoreHostWithUsage(h *Host, usedCPU, usedMem int) float64 {
	var score float64

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

	// Penalize hosts with many pending (in-flight) allocations to spread
	// burst load across hosts instead of queueing on one host agent.
	if h.PendingCPUMillicores > 0 && h.TotalCPUMillicores > 0 {
		pendingFraction := float64(h.PendingCPUMillicores) / float64(h.TotalCPUMillicores)
		score -= pendingFraction * 30
	}

	return score
}

// ReleaseRunner releases a runner. The operation is idempotent: releasing a
// runner that has already been removed returns nil so callers don't see 500s
// on retries or concurrent release attempts.
func (s *Scheduler) ReleaseRunner(ctx context.Context, runnerID string, destroy bool) error {
	s.logger.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"destroy":   destroy,
	}).Info("Releasing runner")

	// Get runner's host from registry. If the runner is already gone, the
	// release is a no-op (idempotent).
	runner, err := s.hostRegistry.GetRunner(runnerID)
	if err != nil {
		s.logger.WithField("runner_id", runnerID).Debug("Runner already removed from registry, release is no-op")
		return nil
	}

	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		// Host gone — clean up the orphaned runner record and return success.
		s.logger.WithField("runner_id", runnerID).Debug("Host gone, cleaning up orphaned runner")
		_ = s.hostRegistry.RemoveRunner(runnerID)
		return nil
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
	// If the host agent says the runner is already gone, that's fine —
	// still clean up our own registry below.
	if resp.Error != "" {
		s.logger.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"error":     resp.Error,
		}).Warn("Host agent reported error during release, cleaning up registry")
	}

	// Update registry
	if err := s.hostRegistry.RemoveRunner(runnerID); err != nil {
		return err
	}

	// Clean up session_snapshots row so stale entries don't accumulate.
	// Preserve suspended sessions — they contain GCS-backed snapshot data
	// needed for cross-host resume after the original host is gone.
	if s.db != nil {
		if _, dbErr := s.db.ExecContext(ctx, `DELETE FROM session_snapshots WHERE runner_id = $1 AND status != 'suspended'`, runnerID); dbErr != nil {
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
}
