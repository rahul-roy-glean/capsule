package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/telemetry"
)

var (
	grpcPort   = flag.Int("grpc-port", 50051, "gRPC server port")
	httpPort   = flag.Int("http-port", 8080, "HTTP server port")
	dbHost     = flag.String("db-host", "localhost", "Database host")
	dbPort     = flag.Int("db-port", 5432, "Database port")
	dbUser     = flag.String("db-user", "postgres", "Database user")
	dbPassword = flag.String("db-password", "", "Database password")
	dbName     = flag.String("db-name", "firecracker_runner", "Database name")
	dbSSLMode  = flag.String("db-ssl-mode", "disable", "Database SSL mode")
	gcsBucket  = flag.String("gcs-bucket", "", "GCS bucket for snapshots")
	logLevel   = flag.String("log-level", "info", "Log level")

	// Telemetry
	telemetryEnabled = flag.Bool("telemetry-enabled", true, "Enable GCP Cloud Monitoring telemetry")
	gcpProject       = flag.String("gcp-project", "", "GCP project for telemetry and snapshot builder VMs")
	gcpZone          = flag.String("gcp-zone", "us-central1-a", "GCP zone for snapshot builder VMs")
	environment      = flag.String("environment", "dev", "Environment name for telemetry labels")
)

func main() {
	flag.Parse()

	// Allow env vars to override defaults (useful for Kubernetes/Helm deployments).
	if v := os.Getenv("DB_HOST"); v != "" && *dbHost == "localhost" {
		*dbHost = v
	}
	if v := os.Getenv("DB_PORT"); v != "" && *dbPort == 5432 {
		if p, err := strconv.Atoi(v); err == nil {
			*dbPort = p
		}
	}
	if v := os.Getenv("DB_USER"); v != "" && *dbUser == "postgres" {
		*dbUser = v
	}
	if v := os.Getenv("DB_PASSWORD"); v != "" && *dbPassword == "" {
		*dbPassword = v
	}
	if v := os.Getenv("DB_NAME"); v != "" && *dbName == "firecracker_runner" {
		*dbName = v
	}
	if v := os.Getenv("DB_SSL_MODE"); v != "" && *dbSSLMode == "disable" {
		*dbSSLMode = v
	}
	if v := os.Getenv("GCS_BUCKET"); v != "" && *gcsBucket == "" {
		*gcsBucket = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" && *logLevel == "info" {
		*logLevel = v
	}

	// Setup logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	log := logger.WithField("component", "control-plane")
	log.Info("Starting control plane...")

	// Connect to database
	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		*dbHost, *dbPort, *dbUser, *dbPassword, *dbName, *dbSSLMode)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.WithError(err).Fatal("Failed to connect to database")
	}
	defer db.Close()

	// Test connection
	if err := db.Ping(); err != nil {
		log.WithError(err).Fatal("Failed to ping database")
	}
	log.Info("Connected to database")

	// Initialize database schema
	if err := initSchema(db); err != nil {
		log.WithError(err).Fatal("Failed to initialize schema")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize telemetry
	telemetryEnabledVal := *telemetryEnabled
	if v := os.Getenv("TELEMETRY_ENABLED"); v != "" {
		telemetryEnabledVal = strings.ToLower(v) == "true"
	}
	gcpProjectVal := *gcpProject
	if v := os.Getenv("GCP_PROJECT_ID"); v != "" {
		gcpProjectVal = v
	}
	gcpZoneVal := *gcpZone
	if v := os.Getenv("GCP_ZONE"); v != "" {
		gcpZoneVal = v
	}
	envVal := *environment
	if v := os.Getenv("ENVIRONMENT"); v != "" {
		envVal = v
	}

	var metricsClient *telemetry.Client
	if telemetryEnabledVal && gcpProjectVal != "" {
		telemetryCfg := telemetry.Config{
			Enabled:       true,
			ProjectID:     gcpProjectVal,
			MetricPrefix:  "custom.googleapis.com/firecracker",
			Component:     "control-plane",
			Environment:   envVal,
			FlushInterval: 10 * time.Second,
		}
		var telErr error
		metricsClient, telErr = telemetry.NewClient(ctx, telemetryCfg, logger)
		if telErr != nil {
			log.WithError(telErr).Warn("Failed to initialize telemetry, continuing without metrics")
		} else {
			defer metricsClient.Close()
			log.Info("GCP Cloud Monitoring telemetry initialized")
		}
	}

	// Create services
	hostRegistry := NewHostRegistry(db, logger)
	scheduler := NewScheduler(hostRegistry, db, logger)
	snapshotManager := NewSnapshotManager(ctx, db, *gcsBucket, gcpProjectVal, gcpZoneVal, logger)
	jobQueue := NewJobQueue(db, scheduler, hostRegistry, logger)
	snapshotConfigRegistry := NewSnapshotConfigRegistry(db, snapshotManager, logger)

	// Load existing state from DB (best-effort)
	if err := hostRegistry.LoadFromDB(ctx); err != nil {
		log.WithError(err).Warn("Failed to load host/runner state from DB")
	}

	// Create gRPC server
	grpcServer := grpc.NewServer()

	// Register services
	controlPlaneServer := NewControlPlaneServer(scheduler, hostRegistry, snapshotManager, jobQueue, metricsClient, logger)
	pb.RegisterControlPlaneServer(grpcServer, controlPlaneServer)

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	reflection.Register(grpcServer)

	// Start gRPC server
	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", *grpcPort))
	if err != nil {
		log.WithError(err).Fatal("Failed to listen for gRPC")
	}

	go func() {
		log.WithField("port", *grpcPort).Info("Starting gRPC server")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.WithError(err).Error("gRPC server error")
		}
	}()

	// Start HTTP server
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	httpMux.Handle("/metrics", promhttp.Handler())
	httpMux.HandleFunc("/api/v1/runners", controlPlaneServer.HandleGetRunners)
	httpMux.HandleFunc("/api/v1/runners/allocate", controlPlaneServer.HandleAllocateRunner)
	httpMux.HandleFunc("/api/v1/runners/status", controlPlaneServer.HandleRunnerStatus)
	httpMux.HandleFunc("/api/v1/runners/release", controlPlaneServer.HandleRunnerRelease)
	httpMux.HandleFunc("/api/v1/runners/pause", controlPlaneServer.HandlePauseRunner)
	httpMux.HandleFunc("/api/v1/runners/connect", controlPlaneServer.HandleConnectRunner)
	httpMux.HandleFunc("/api/v1/runners/quarantine", controlPlaneServer.HandleQuarantineRunner)
	httpMux.HandleFunc("/api/v1/runners/unquarantine", controlPlaneServer.HandleUnquarantineRunner)
	httpMux.HandleFunc("/api/v1/hosts", controlPlaneServer.HandleGetHosts)
	httpMux.HandleFunc("/api/v1/hosts/heartbeat", controlPlaneServer.HandleHostHeartbeat)
	httpMux.HandleFunc("/api/v1/snapshots", controlPlaneServer.HandleGetSnapshots)
	// Snapshot config registry endpoints
	httpMux.HandleFunc("/api/v1/snapshot-configs/", snapshotConfigRegistry.HandleSnapshotConfigs)
	httpMux.HandleFunc("/api/v1/snapshot-configs", snapshotConfigRegistry.HandleSnapshotConfigs)
	// Version/rollout endpoints (Phase 4)
	httpMux.HandleFunc("/api/v1/versions/desired", controlPlaneServer.HandleGetDesiredVersions)
	httpMux.HandleFunc("/api/v1/versions/fleet", controlPlaneServer.HandleGetFleetConvergence)
	// Canary report endpoint (Phase 6)
	httpMux.HandleFunc("/api/v1/canary/report", controlPlaneServer.HandleCanaryReport)
	// Register webhook handler conditionally based on CI system config
	ciSystemEnv := os.Getenv("CI_SYSTEM")
	if ciSystemEnv == "" || ciSystemEnv == "github-actions" {
		httpMux.HandleFunc("/webhook/github", controlPlaneServer.HandleGitHubWebhook)
	}

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *httpPort),
		Handler: httpMux,
	}

	go func() {
		log.WithField("port", *httpPort).Info("Starting HTTP server")
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.WithError(err).Error("HTTP server error")
		}
	}()

	// Start background workers
	go hostRegistry.HealthCheckLoop(ctx)
	go snapshotFreshnessLoop(ctx, snapshotManager, snapshotConfigRegistry, logger)
	go startDownscaler(ctx, db, hostRegistry, logger)
	go jobQueue.jobRetryLoop(ctx)
	if metricsClient != nil {
		go controlPlaneMetricsLoop(ctx, hostRegistry, scheduler, snapshotManager, metricsClient, logger)
	}

	// Wait for shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	grpcServer.GracefulStop()
	httpServer.Shutdown(shutdownCtx)

	log.Info("Shutdown complete")
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS hosts (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		instance_name VARCHAR(255) NOT NULL,
		zone VARCHAR(50) NOT NULL,
		status VARCHAR(20) NOT NULL DEFAULT 'starting',
		total_slots INT NOT NULL,
		used_slots INT NOT NULL DEFAULT 0,
		idle_runners INT NOT NULL DEFAULT 0,
		busy_runners INT NOT NULL DEFAULT 0,
		snapshot_version VARCHAR(50),
		snapshot_synced_at TIMESTAMP,
		last_heartbeat TIMESTAMP,
		grpc_address VARCHAR(255),
		http_address VARCHAR(255),
		created_at TIMESTAMP DEFAULT NOW(),
		UNIQUE(instance_name)
	);

	CREATE TABLE IF NOT EXISTS runners (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		host_id UUID REFERENCES hosts(id),
		status VARCHAR(20) NOT NULL DEFAULT 'initializing',
		internal_ip VARCHAR(15),
		github_runner_id VARCHAR(255),
		job_id VARCHAR(255),
		repo VARCHAR(255),
		branch VARCHAR(255),
		created_at TIMESTAMP DEFAULT NOW(),
		started_at TIMESTAMP,
		completed_at TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS snapshots (
		version VARCHAR(50) PRIMARY KEY,
		status VARCHAR(20) NOT NULL DEFAULT 'building',
		gcs_path VARCHAR(255),
		bazel_version VARCHAR(20),
		repo_commit VARCHAR(40),
		size_bytes BIGINT,
		created_at TIMESTAMP DEFAULT NOW(),
		metrics JSONB
	);

	CREATE TABLE IF NOT EXISTS jobs (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		github_workflow_run_id BIGINT,
		github_job_id BIGINT,
		repo VARCHAR(255),
		branch VARCHAR(255),
		commit_sha VARCHAR(40),
		status VARCHAR(20) NOT NULL DEFAULT 'queued',
		runner_id UUID REFERENCES runners(id),
		labels JSONB,
		queued_at TIMESTAMP DEFAULT NOW(),
		started_at TIMESTAMP,
		completed_at TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_hosts_status ON hosts(status);
	CREATE INDEX IF NOT EXISTS idx_runners_status ON runners(status);
	CREATE INDEX IF NOT EXISTS idx_runners_host_id ON runners(host_id);
	CREATE INDEX IF NOT EXISTS idx_snapshots_status ON snapshots(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	`

	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Backwards-compatible migrations (no-ops if already applied)
	migrations := []string{
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS idle_runners INT NOT NULL DEFAULT 0`,
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS busy_runners INT NOT NULL DEFAULT 0`,
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS http_address VARCHAR(255)`,
		// Phase 1: Multi-repo support
		`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS repo VARCHAR(255) DEFAULT ''`,
		`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS repo_slug VARCHAR(255) DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_repo_slug ON snapshots(repo_slug)`,
		// Repos table
		`CREATE TABLE IF NOT EXISTS repos (
			slug VARCHAR(255) PRIMARY KEY,
			url VARCHAR(512) NOT NULL,
			branch VARCHAR(255) DEFAULT 'main',
			bazel_version VARCHAR(32) DEFAULT '',
			warmup_targets VARCHAR(1024) DEFAULT '//...',
			build_schedule VARCHAR(64) DEFAULT '',
			max_concurrent_runners INT DEFAULT 0,
			current_version VARCHAR(255),
			auto_rollout BOOLEAN DEFAULT true,
			created_at TIMESTAMP DEFAULT NOW()
		)`,
		// Version assignments table
		`CREATE TABLE IF NOT EXISTS version_assignments (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			repo_slug VARCHAR(255) NOT NULL,
			host_id UUID REFERENCES hosts(id),
			version VARCHAR(255) NOT NULL,
			status VARCHAR(32) DEFAULT 'assigned',
			assigned_at TIMESTAMP DEFAULT NOW(),
			synced_at TIMESTAMP,
			UNIQUE(repo_slug, host_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_version_assignments_repo ON version_assignments(repo_slug)`,
		`CREATE INDEX IF NOT EXISTS idx_version_assignments_host ON version_assignments(host_id)`,
		// chunk_key migration: add snapshot_configs table
		`CREATE TABLE IF NOT EXISTS snapshot_configs (
			chunk_key              VARCHAR(16) PRIMARY KEY,
			display_name           VARCHAR(255),
			commands               TEXT NOT NULL DEFAULT '[]',
			build_schedule         VARCHAR(64) DEFAULT '',
			max_concurrent_runners INT DEFAULT 0,
			current_version        VARCHAR(255),
			auto_rollout           BOOLEAN DEFAULT true,
			created_at             TIMESTAMP DEFAULT NOW()
		)`,
		// Add chunk_key column to snapshots (rename from repo_slug)
		`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS chunk_key VARCHAR(16) DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_chunk_key ON snapshots(chunk_key)`,
		// GitHub App credentials for snapshot configs
		`ALTER TABLE snapshot_configs ADD COLUMN IF NOT EXISTS github_app_id VARCHAR(255) DEFAULT ''`,
		`ALTER TABLE snapshot_configs ADD COLUMN IF NOT EXISTS github_app_secret VARCHAR(255) DEFAULT ''`,
		// Add chunk_key column to version_assignments
		`ALTER TABLE version_assignments ADD COLUMN IF NOT EXISTS chunk_key VARCHAR(16) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_version_assignments_chunk ON version_assignments(chunk_key)`,
		// Session pause/resume: TTL and auto-pause config
		`ALTER TABLE snapshot_configs ADD COLUMN IF NOT EXISTS runner_ttl_seconds INT DEFAULT 0`,
		`ALTER TABLE snapshot_configs ADD COLUMN IF NOT EXISTS session_max_age_seconds INT DEFAULT 86400`,
		`ALTER TABLE snapshot_configs ADD COLUMN IF NOT EXISTS auto_pause BOOLEAN DEFAULT false`,
		// Session snapshots tracking for pause/resume
		`CREATE TABLE IF NOT EXISTS session_snapshots (
			session_id    TEXT PRIMARY KEY,
			runner_id     TEXT NOT NULL,
			chunk_key     VARCHAR(16) NOT NULL,
			host_id       UUID REFERENCES hosts(id),
			status        VARCHAR(20) NOT NULL DEFAULT 'active',
			layer_count   INT DEFAULT 0,
			total_size_bytes BIGINT DEFAULT 0,
			metadata      JSONB,
			created_at    TIMESTAMP DEFAULT NOW(),
			paused_at     TIMESTAMP,
			expires_at    TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_chunk ON session_snapshots(chunk_key)`,
		`CREATE INDEX IF NOT EXISTS idx_session_host ON session_snapshots(host_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_status ON session_snapshots(status)`,
		// runner_id lookups on session_snapshots (HandleRunnerStatus, HandleConnectRunner)
		`CREATE INDEX IF NOT EXISTS idx_session_runner ON session_snapshots(runner_id)`,
		// Expiry cleanup: scan suspended sessions past their TTL
		`CREATE INDEX IF NOT EXISTS idx_session_expires ON session_snapshots(status, expires_at) WHERE status = 'suspended'`,
		// runners: per-repo fairness check (scheduler.go: COUNT(*) WHERE repo=$1 AND status IN (...))
		`CREATE INDEX IF NOT EXISTS idx_runners_repo_status ON runners(repo, status)`,
		// jobs: queue drain (WHERE status='queued' ORDER BY queued_at)
		`CREATE INDEX IF NOT EXISTS idx_jobs_queued ON jobs(status, queued_at) WHERE status = 'queued'`,
		// jobs: completion by github_job_id
		`CREATE INDEX IF NOT EXISTS idx_jobs_github_job_id ON jobs(github_job_id)`,
		// snapshots: chunk_key + status + created_at for version lookups
		`CREATE INDEX IF NOT EXISTS idx_snapshots_chunk_status ON snapshots(chunk_key, status, created_at DESC)`,
		// version_assignments: compound for subquery in GetFleetConvergence
		`CREATE INDEX IF NOT EXISTS idx_version_assignments_chunk_host ON version_assignments(chunk_key, host_id)`,
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// ControlPlaneServer implements the ControlPlane gRPC service
type ControlPlaneServer struct {
	pb.UnimplementedControlPlaneServer
	scheduler       *Scheduler
	hostRegistry    *HostRegistry
	snapshotManager *SnapshotManager
	jobQueue        *JobQueue
	metricsClient   *telemetry.Client
	logger          *logrus.Entry
}

func NewControlPlaneServer(s *Scheduler, h *HostRegistry, sm *SnapshotManager, jq *JobQueue, mc *telemetry.Client, l *logrus.Logger) *ControlPlaneServer {
	return &ControlPlaneServer{
		scheduler:       s,
		hostRegistry:    h,
		snapshotManager: sm,
		jobQueue:        jq,
		metricsClient:   mc,
		logger:          l.WithField("service", "control-plane"),
	}
}

// RegisterHost implements the gRPC RegisterHost method
func (s *ControlPlaneServer) RegisterHost(ctx context.Context, req *pb.RegisterHostRequest) (*pb.RegisterHostResponse, error) {
	s.logger.WithFields(logrus.Fields{
		"instance_name": req.InstanceName,
		"zone":          req.Zone,
		"total_slots":   req.TotalSlots,
	}).Info("RegisterHost request")

	host, err := s.hostRegistry.RegisterHost(ctx, req.InstanceName, req.Zone, int(req.TotalSlots), "")
	if err != nil {
		return nil, err
	}

	currentSnapshot := s.snapshotManager.GetCurrentVersion()

	return &pb.RegisterHostResponse{
		HostId:          host.ID,
		SnapshotVersion: currentSnapshot,
	}, nil
}

// Heartbeat implements the gRPC Heartbeat method
func (s *ControlPlaneServer) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	if req.Status == nil {
		return &pb.HeartbeatResponse{Acknowledged: false}, nil
	}

	err := s.hostRegistry.UpdateHeartbeat(ctx, req.HostId, HostStatus{
		UsedSlots:       int(req.Status.UsedSlots),
		IdleRunners:     int(req.Status.IdleRunners),
		BusyRunners:     int(req.Status.BusyRunners),
		SnapshotVersion: req.Status.SnapshotVersion,
	})
	if err != nil {
		s.logger.WithError(err).Warn("Failed to update heartbeat")
	}

	currentSnapshot := s.snapshotManager.GetCurrentVersion()
	shouldSync := currentSnapshot != "" && currentSnapshot != req.Status.SnapshotVersion

	return &pb.HeartbeatResponse{
		Acknowledged:       true,
		SnapshotVersion:    currentSnapshot,
		ShouldSyncSnapshot: shouldSync,
	}, nil
}

// GetSnapshot implements the gRPC GetSnapshot method
func (s *ControlPlaneServer) GetSnapshot(ctx context.Context, req *pb.GetSnapshotRequest) (*pb.Snapshot, error) {
	version := req.Version
	if version == "" || version == "current" {
		version = s.snapshotManager.GetCurrentVersion()
	}

	snapshot, err := s.snapshotManager.GetSnapshot(ctx, version)
	if err != nil {
		return nil, err
	}

	return s.snapshotManager.SnapshotToProto(snapshot), nil
}

// TriggerSnapshotBuild implements the gRPC TriggerSnapshotBuild method
func (s *ControlPlaneServer) TriggerSnapshotBuild(ctx context.Context, req *pb.TriggerSnapshotBuildRequest) (*pb.TriggerSnapshotBuildResponse, error) {
	buildID, err := s.snapshotManager.TriggerBuild(ctx, req.Repo, req.Branch, req.BazelVersion)
	if err != nil {
		return &pb.TriggerSnapshotBuildResponse{
			Status: "error: " + err.Error(),
		}, nil
	}

	return &pb.TriggerSnapshotBuildResponse{
		BuildId: buildID,
		Status:  "queued",
	}, nil
}

// HTTP Handlers

func (s *ControlPlaneServer) HandleGetRunners(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	hosts := s.hostRegistry.GetAllHosts()
	var allRunners []map[string]interface{}

	for _, h := range hosts {
		// For now, return basic host runner info
		for i := 0; i < h.IdleRunners+h.BusyRunners; i++ {
			allRunners = append(allRunners, map[string]interface{}{
				"host_id":   h.ID,
				"host_name": h.InstanceName,
				"status":    "running",
			})
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"runners": allRunners,
		"count":   len(allRunners),
	})
}

// HandleAllocateRunner handles manual runner allocation requests.
// POST /api/v1/runners/allocate
// Body: {"repo": "org/repo", "branch": "main", "commit": "abc123", "labels": {"firecracker": "true"}}
// For exec mode: {"chunk_key": "generic-linux", "ci_system": "none"}
func (s *ControlPlaneServer) HandleAllocateRunner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Repo      string            `json:"repo"`
		Branch    string            `json:"branch"`
		Commit    string            `json:"commit"`
		Labels    map[string]string `json:"labels"`
		RequestID string            `json:"request_id"`
		ChunkKey  string            `json:"chunk_key"`
		CISystem  string            `json:"ci_system"`
		SessionID string            `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// In exec mode (ci_system=none), repo is optional
	if req.Repo == "" && req.CISystem != "none" {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return
	}
	if req.RequestID == "" {
		req.RequestID = fmt.Sprintf("manual-%d", time.Now().UnixNano())
	}

	// Determine chunk_key: use explicit chunk_key, or look up from repo
	chunkKey := req.ChunkKey
	if chunkKey == "" && req.Repo != "" {
		chunkKey = lookupChunkKeyForRepo(s.scheduler.db, req.Repo)
	}

	s.logger.WithFields(logrus.Fields{
		"request_id": req.RequestID,
		"repo":       req.Repo,
		"chunk_key":  chunkKey,
		"branch":     req.Branch,
		"ci_system":  req.CISystem,
	}).Info("Manual runner allocation request")

	resp, err := s.scheduler.AllocateRunner(r.Context(), AllocateRunnerRequest{
		RequestID: req.RequestID,
		Repo:      req.Repo,
		Branch:    req.Branch,
		Commit:    req.Commit,
		ChunkKey:  chunkKey,
		Labels:    req.Labels,
		CISystem:  req.CISystem,
		SessionID: req.SessionID,
	})
	if err != nil {
		s.logger.WithError(err).Error("Manual allocation failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"runner_id":    resp.RunnerID,
		"host_id":      resp.HostID,
		"host_address": resp.HostAddress,
		"internal_ip":  resp.InternalIP,
		"session_id":   resp.SessionID,
		"resumed":      resp.Resumed,
	})
}

// HandleRunnerStatus returns the status of a runner.
// GET /api/v1/runners/status?runner_id=abc-123
// Returns 200 when ready, 202 when pending, 404 when not found, 503 when unavailable.
func (s *ControlPlaneServer) HandleRunnerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runnerID := r.URL.Query().Get("runner_id")
	if runnerID == "" {
		http.Error(w, "runner_id is required", http.StatusBadRequest)
		return
	}

	runner, err := s.hostRegistry.GetRunner(runnerID)
	if err != nil {
		// Check session_snapshots for suspended sessions
		var status string
		scanErr := s.scheduler.db.QueryRowContext(r.Context(),
			`SELECT status FROM session_snapshots WHERE runner_id = $1`, runnerID).Scan(&status)
		if scanErr == nil && status == "suspended" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"runner_id": runnerID,
				"status":    "suspended",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "runner not found"})
		return
	}

	// Look up host for address
	host, err := s.hostRegistry.GetHost(runner.HostID)
	hostAddress := ""
	if err == nil {
		hostAddress = host.HTTPAddress
	}

	w.Header().Set("Content-Type", "application/json")
	switch runner.Status {
	case "initializing", "booting":
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{
			"runner_id": runnerID,
			"status":    "pending",
		})
	case "idle", "busy", "running":
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"runner_id":    runnerID,
			"status":       "ready",
			"host_address": hostAddress,
		})
	case "quarantined", "draining":
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"runner_id": runnerID,
			"status":    "unavailable",
		})
	default: // terminated or unknown
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "runner not found",
		})
	}
}

// HandleRunnerRelease explicitly releases/destroys a runner.
// POST /api/v1/runners/release
// Body: {"runner_id": "abc-123"}
func (s *ControlPlaneServer) HandleRunnerRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunnerID string `json:"runner_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RunnerID == "" {
		http.Error(w, "runner_id is required", http.StatusBadRequest)
		return
	}

	s.logger.WithField("runner_id", req.RunnerID).Info("Runner release request")

	if err := s.scheduler.ReleaseRunner(r.Context(), req.RunnerID, true); err != nil {
		s.logger.WithError(err).Error("Runner release failed")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// HandlePauseRunner pauses a runner via its host agent.
// POST /api/v1/runners/pause
// Body: {"runner_id": "abc-123"}
func (s *ControlPlaneServer) HandlePauseRunner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunnerID string `json:"runner_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.RunnerID == "" {
		http.Error(w, "runner_id is required", http.StatusBadRequest)
		return
	}

	s.logger.WithField("runner_id", req.RunnerID).Info("Pause runner request")

	runner, err := s.hostRegistry.GetRunner(req.RunnerID)
	if err != nil {
		http.Error(w, "runner not found", http.StatusNotFound)
		return
	}
	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		http.Error(w, "host not found", http.StatusInternalServerError)
		return
	}

	// Forward to host agent gRPC
	conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		http.Error(w, "failed to connect to host", http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	client := pb.NewHostAgentClient(conn)
	resp, err := client.PauseRunner(r.Context(), &pb.PauseRunnerRequest{RunnerId: req.RunnerID})
	if err != nil {
		http.Error(w, "pause failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if resp.Error != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": resp.Error})
		return
	}

	// Update session_snapshots table
	if resp.SessionId != "" {
		_, _ = s.scheduler.db.ExecContext(r.Context(), `
			INSERT INTO session_snapshots (session_id, runner_id, chunk_key, host_id, status, layer_count, paused_at)
			VALUES ($1, $2, '', $3, 'suspended', $4, NOW())
			ON CONFLICT (session_id) DO UPDATE SET
				status = 'suspended',
				layer_count = EXCLUDED.layer_count,
				paused_at = NOW()
		`, resp.SessionId, req.RunnerID, host.ID, resp.Layer+1)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":             resp.Success,
		"session_id":          resp.SessionId,
		"snapshot_size_bytes": resp.SnapshotSizeBytes,
		"layer":               resp.Layer,
	})
}

// HandleConnectRunner connects to a runner: extends TTL if running, resumes if suspended.
// POST /api/v1/runners/connect
// Body: {"runner_id": "abc-123"}
func (s *ControlPlaneServer) HandleConnectRunner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunnerID string `json:"runner_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.RunnerID == "" {
		http.Error(w, "runner_id is required", http.StatusBadRequest)
		return
	}

	runner, err := s.hostRegistry.GetRunner(req.RunnerID)
	if err != nil {
		// Check if suspended in session_snapshots
		var sessionID, hostID string
		var status string
		scanErr := s.scheduler.db.QueryRowContext(r.Context(),
			`SELECT session_id, host_id, status FROM session_snapshots WHERE runner_id = $1`,
			req.RunnerID).Scan(&sessionID, &hostID, &status)
		if scanErr != nil || status != "suspended" {
			http.Error(w, "runner not found", http.StatusNotFound)
			return
		}

		// Resume from session via host
		host, err := s.hostRegistry.GetHost(hostID)
		if err != nil {
			http.Error(w, "host not found for suspended session", http.StatusInternalServerError)
			return
		}
		conn, err := grpc.NewClient(host.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			http.Error(w, "failed to connect to host", http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		client := pb.NewHostAgentClient(conn)
		resp, err := client.ResumeRunner(r.Context(), &pb.ResumeRunnerRequest{SessionId: sessionID})
		if err != nil || resp.Error != "" {
			errMsg := "resume failed"
			if err != nil {
				errMsg = err.Error()
			} else {
				errMsg = resp.Error
			}
			http.Error(w, errMsg, http.StatusInternalServerError)
			return
		}

		// Update session status
		_, _ = s.scheduler.db.ExecContext(r.Context(),
			`UPDATE session_snapshots SET status = 'active' WHERE session_id = $1`, sessionID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "resumed",
			"runner_id": resp.Runner.GetId(),
		})
		return
	}

	// Runner exists — it's running, forward connect to extend TTL
	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		http.Error(w, "host not found", http.StatusInternalServerError)
		return
	}

	// Forward to host's HTTP connect endpoint
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":       "connected",
		"runner_id":    req.RunnerID,
		"host_address": host.HTTPAddress,
	})
}

func (s *ControlPlaneServer) HandleGetHosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	hosts := s.hostRegistry.GetAllHosts()
	var hostList []map[string]interface{}

	for _, h := range hosts {
		hostList = append(hostList, map[string]interface{}{
			"id":               h.ID,
			"instance_name":    h.InstanceName,
			"zone":             h.Zone,
			"status":           h.Status,
			"total_slots":      h.TotalSlots,
			"used_slots":       h.UsedSlots,
			"idle_runners":     h.IdleRunners,
			"busy_runners":     h.BusyRunners,
			"snapshot_version": h.SnapshotVersion,
			"last_heartbeat":   h.LastHeartbeat,
			"grpc_address":     h.GRPCAddress,
		})
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"hosts": hostList,
		"count": len(hostList),
	})
}

func (s *ControlPlaneServer) HandleGetSnapshots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	snapshots, err := s.snapshotManager.ListSnapshots(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"snapshots":       snapshots,
		"count":           len(snapshots),
		"current_version": s.snapshotManager.GetCurrentVersion(),
	})
}

func (s *ControlPlaneServer) HandleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	handler := NewGitHubWebhookHandler(s.scheduler, s.hostRegistry, s.jobQueue, s.logger.Logger)
	handler.HandleWebhook(w, r)
}

// HandleGetDesiredVersions returns the desired snapshot versions for a host.
// GET /api/v1/versions/desired?instance_name={name}
func (s *ControlPlaneServer) HandleGetDesiredVersions(w http.ResponseWriter, r *http.Request) {
	instanceName := r.URL.Query().Get("instance_name")
	if instanceName == "" {
		http.Error(w, "instance_name is required", http.StatusBadRequest)
		return
	}

	host, ok := s.hostRegistry.GetHostByInstanceName(instanceName)
	if !ok {
		http.Error(w, "host not found", http.StatusNotFound)
		return
	}

	versions, err := s.snapshotManager.GetDesiredVersions(r.Context(), host.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"host_id":          host.ID,
		"instance_name":    instanceName,
		"desired_versions": versions,
	})
}

// HandleGetFleetConvergence returns the fleet convergence state.
// GET /api/v1/versions/fleet?chunk_key={key}
func (s *ControlPlaneServer) HandleGetFleetConvergence(w http.ResponseWriter, r *http.Request) {
	chunkKey := r.URL.Query().Get("chunk_key")
	if chunkKey == "" {
		http.Error(w, "chunk_key is required", http.StatusBadRequest)
		return
	}

	statuses, err := s.snapshotManager.GetFleetConvergence(r.Context(), chunkKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"chunk_key": chunkKey,
		"hosts":     statuses,
		"count":     len(statuses),
	})
}

// HandleCanaryReport receives E2E canary health check results.
// POST /api/v1/canary/report
func (s *ControlPlaneServer) HandleCanaryReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var report struct {
		Status    string `json:"status"`
		Runner    string `json:"runner"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	s.logger.WithFields(logrus.Fields{
		"status": report.Status,
		"runner": report.Runner,
	}).Info("Received canary report")

	if s.metricsClient != nil {
		if report.Status == "success" {
			s.metricsClient.IncrementCounter(r.Context(), telemetry.MetricE2ECanarySuccess, nil)
		} else {
			s.metricsClient.IncrementCounter(r.Context(), telemetry.MetricE2ECanaryFailure, nil)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// controlPlaneMetricsLoop periodically records control plane metrics to GCP Cloud Monitoring
func controlPlaneMetricsLoop(ctx context.Context, hr *HostRegistry, sched *Scheduler, sm *SnapshotManager, mc *telemetry.Client, logger *logrus.Logger) {
	log := logger.WithField("component", "metrics-loop")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hosts := hr.GetAllHosts()

			// Aggregate host counts by status
			statusCounts := map[string]int64{}
			totalRunners := int64(0)
			totalIdle := int64(0)
			totalBusy := int64(0)
			fleetSlotsTotal := int64(0)
			fleetSlotsUsed := int64(0)
			activeHosts := int64(0)

			for _, h := range hosts {
				statusCounts[h.Status]++
				totalRunners += int64(h.IdleRunners + h.BusyRunners)
				totalIdle += int64(h.IdleRunners)
				totalBusy += int64(h.BusyRunners)
				if h.Status == "ready" {
					fleetSlotsTotal += int64(h.TotalSlots)
					fleetSlotsUsed += int64(h.UsedSlots)
					activeHosts++
				}
			}
			fleetSlotsFree := fleetSlotsTotal - fleetSlotsUsed

			// Record host counts by status
			for status, count := range statusCounts {
				mc.RecordInt(ctx, telemetry.MetricCPHostsTotal, count, telemetry.Labels{
					telemetry.LabelStatus: status,
				})
			}

			// Record runner totals
			mc.RecordInt(ctx, telemetry.MetricCPRunnersTotal, totalRunners, telemetry.Labels{
				telemetry.LabelStatus: "total",
			})
			mc.RecordInt(ctx, telemetry.MetricCPRunnersTotal, totalIdle, telemetry.Labels{
				telemetry.LabelStatus: "idle",
			})
			mc.RecordInt(ctx, telemetry.MetricCPRunnersTotal, totalBusy, telemetry.Labels{
				telemetry.LabelStatus: "busy",
			})

			// Record fleet slot metrics — primary autoscaler signal.
			mc.RecordInt(ctx, telemetry.MetricCPFleetSlotsTotal, fleetSlotsTotal, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetSlotsUsed, fleetSlotsUsed, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetSlotsFree, fleetSlotsFree, nil)
			// free_slots_per_host: GCP autoscaler scales out when this drops below target.
			// Use 0 when there are no active hosts (signals immediate scale-out need).
			freeSlotsPer := int64(0)
			if activeHosts > 0 {
				freeSlotsPer = fleetSlotsFree / activeHosts
			}
			mc.RecordInt(ctx, telemetry.MetricCPFleetFreeSlotsPer, freeSlotsPer, nil)

			// Record queue depth
			mc.RecordInt(ctx, telemetry.MetricCPQueueDepth, int64(sched.GetQueueDepth()), nil)

			// Record snapshot age
			currentVersion := sm.GetCurrentVersion()
			if currentVersion != "" {
				if snapshot, err := sm.GetSnapshot(ctx, currentVersion); err == nil && snapshot != nil {
					age := time.Since(snapshot.CreatedAt)
					mc.RecordFloat(ctx, telemetry.MetricSnapshotAge, age.Seconds(), nil)
				}
			}

			log.WithFields(logrus.Fields{
				"hosts_total":   len(hosts),
				"runners_total": totalRunners,
				"runners_idle":  totalIdle,
				"runners_busy":  totalBusy,
			}).Debug("Metrics recorded")
		}
	}
}
