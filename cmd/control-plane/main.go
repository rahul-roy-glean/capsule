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
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
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
	gcsPrefix  = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")
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
	snapshotManager := NewSnapshotManager(ctx, db, *gcsBucket, *gcsPrefix, gcpProjectVal, gcpZoneVal, logger)
	configCache := NewConfigCache(db, logger)
	tagRegistry := NewSnapshotTagRegistry(db, logger)
	scheduler := NewScheduler(hostRegistry, db, snapshotManager, tagRegistry, logger)
	scheduler.SetConfigCache(configCache)
	if metricsClient != nil {
		scheduler.SetMetricsClient(metricsClient)
	}
	jobQueue := NewJobQueue(db, scheduler, hostRegistry, logger)
	jobQueue.SetConfigCache(configCache)
	layeredConfigRegistry := NewLayeredConfigRegistry(db, snapshotManager, logger)
	layeredConfigRegistry.SetConfigCache(configCache)
	layerBuildScheduler := NewLayerBuildScheduler(db, snapshotManager, logger, 4)
	layeredConfigRegistry.SetLayerBuilder(layerBuildScheduler)

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
	// Layered config registry endpoints
	httpMux.HandleFunc("/api/v1/layered-configs/", layeredConfigRegistry.HandleLayeredConfigs)
	httpMux.HandleFunc("/api/v1/layered-configs", layeredConfigRegistry.HandleLayeredConfigs)
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
		Handler: apiLoggingMiddleware(logger, httpMux),
	}

	go func() {
		log.WithField("port", *httpPort).Info("Starting HTTP server")
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.WithError(err).Error("HTTP server error")
		}
	}()

	// Start background workers
	go hostRegistry.HealthCheckLoop(ctx)
	go startDownscaler(ctx, db, hostRegistry, scheduler, logger)
	go jobQueue.jobRetryLoop(ctx)
	go controlPlaneServer.startTTLEnforcement(ctx)
	go layerBuildScheduler.Run(ctx)
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

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// apiLoggingMiddleware logs every API request on completion with method, path,
// status code, duration, and optional identifiers from query parameters.
func apiLoggingMiddleware(logger *logrus.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip /health and /metrics — too noisy
		if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		fields := logrus.Fields{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      rec.statusCode,
			"duration_ms": duration.Milliseconds(),
			"remote_addr": r.RemoteAddr,
		}

		// Extract identifiers from query string (GET endpoints)
		if v := r.URL.Query().Get("runner_id"); v != "" {
			fields["runner_id"] = v
		}
		if v := r.URL.Query().Get("workload_key"); v != "" {
			fields["workload_key"] = v
		}

		entry := logger.WithFields(fields)
		if rec.statusCode >= 500 {
			entry.Error("API request completed with server error")
		} else if rec.statusCode >= 400 {
			entry.Warn("API request completed with client error")
		} else {
			entry.Info("API request completed")
		}
	})
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS hosts (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		instance_name VARCHAR(255) NOT NULL,
		zone VARCHAR(50) NOT NULL,
		status VARCHAR(20) NOT NULL DEFAULT 'starting',
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
		// Rename chunk_key -> workload_key in existing tables (idempotent: no-op if column doesn't exist or target already exists)
		`DO $$ BEGIN ALTER TABLE snapshots RENAME COLUMN chunk_key TO workload_key; EXCEPTION WHEN undefined_column THEN NULL; WHEN duplicate_column THEN NULL; WHEN others THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE version_assignments RENAME COLUMN chunk_key TO workload_key; EXCEPTION WHEN undefined_column THEN NULL; WHEN duplicate_column THEN NULL; WHEN others THEN NULL; END $$`,
		`DO $$ BEGIN ALTER TABLE session_snapshots RENAME COLUMN chunk_key TO workload_key; EXCEPTION WHEN undefined_column THEN NULL; WHEN duplicate_column THEN NULL; WHEN others THEN NULL; END $$`,
		// Add workload_key column to snapshots (for fresh installs without chunk_key)
		`ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS workload_key VARCHAR(16) DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_workload_key ON snapshots(workload_key)`,
		// Add workload_key column to version_assignments
		`ALTER TABLE version_assignments ADD COLUMN IF NOT EXISTS workload_key VARCHAR(16) NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_version_assignments_workload ON version_assignments(workload_key)`,
		// Session snapshots tracking for pause/resume
		`CREATE TABLE IF NOT EXISTS session_snapshots (
			session_id    TEXT PRIMARY KEY,
			runner_id     TEXT NOT NULL,
			workload_key     VARCHAR(16) NOT NULL,
			host_id       UUID REFERENCES hosts(id),
			status        VARCHAR(20) NOT NULL DEFAULT 'active',
			layer_count   INT DEFAULT 0,
			total_size_bytes BIGINT DEFAULT 0,
			metadata      JSONB,
			created_at    TIMESTAMP DEFAULT NOW(),
			paused_at     TIMESTAMP,
			expires_at    TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_workload ON session_snapshots(workload_key)`,
		`CREATE INDEX IF NOT EXISTS idx_session_host ON session_snapshots(host_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_status ON session_snapshots(status)`,
		// runner_id lookups on session_snapshots (HandleRunnerStatus, HandleConnectRunner)
		`CREATE INDEX IF NOT EXISTS idx_session_runner ON session_snapshots(runner_id)`,
		// Expiry cleanup: scan suspended sessions past their TTL
		`CREATE INDEX IF NOT EXISTS idx_session_expires ON session_snapshots(status, expires_at) WHERE status = 'suspended'`,
		// Drop legacy repo index and columns (workload_key replaced repo-based routing)
		`DROP INDEX IF EXISTS idx_runners_repo_status`,
		// jobs: queue drain (WHERE status='queued' ORDER BY queued_at)
		`CREATE INDEX IF NOT EXISTS idx_jobs_queued ON jobs(status, queued_at) WHERE status = 'queued'`,
		// jobs: completion by github_job_id
		`CREATE INDEX IF NOT EXISTS idx_jobs_github_job_id ON jobs(github_job_id)`,
		// snapshots: workload_key + status + created_at for version lookups
		`CREATE INDEX IF NOT EXISTS idx_snapshots_workload_status ON snapshots(workload_key, status, created_at DESC)`,
		// version_assignments: compound for subquery in GetFleetConvergence
		`CREATE INDEX IF NOT EXISTS idx_version_assignments_workload_host ON version_assignments(workload_key, host_id)`,
		// Add workload_key column to runners
		`ALTER TABLE runners ADD COLUMN IF NOT EXISTS workload_key VARCHAR(16) DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_runners_workload_key ON runners(workload_key)`,
		// Host resource tracking for bin-packing scheduler
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS total_cpu_millicores INT DEFAULT 0`,
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS used_cpu_millicores INT DEFAULT 0`,
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS total_memory_mb INT DEFAULT 0`,
		`ALTER TABLE hosts ADD COLUMN IF NOT EXISTS used_memory_mb INT DEFAULT 0`,
		// Drop host_summary view that depends on total_slots/used_slots before dropping the columns
		`DROP VIEW IF EXISTS host_summary`,
		`ALTER TABLE hosts DROP COLUMN IF EXISTS total_slots`,
		`ALTER TABLE hosts DROP COLUMN IF EXISTS used_slots`,
		// Recreate host_summary view without slot columns
		`CREATE OR REPLACE VIEW host_summary AS
		 SELECT
		     COUNT(*) as total_hosts,
		     COUNT(*) FILTER (WHERE status = 'ready') as ready_hosts,
		     COUNT(*) FILTER (WHERE status = 'draining') as draining_hosts,
		     COUNT(*) FILTER (WHERE status = 'unhealthy') as unhealthy_hosts,
		     SUM(idle_runners) as idle_runners,
		     SUM(busy_runners) as busy_runners
		 FROM hosts
		 WHERE last_heartbeat > NOW() - INTERVAL '2 minutes'`,
		// Drop unused repo/branch columns from runners
		`ALTER TABLE runners DROP COLUMN IF EXISTS repo`,
		`ALTER TABLE runners DROP COLUMN IF EXISTS branch`,
		// Layered snapshot pipeline tables
		`CREATE TABLE IF NOT EXISTS snapshot_layers (
			layer_hash           VARCHAR(64) PRIMARY KEY,
			parent_layer_hash    VARCHAR(64) REFERENCES snapshot_layers(layer_hash),
			config_name          VARCHAR(255) NOT NULL,
			depth                INT NOT NULL DEFAULT 0,
			init_commands        JSONB NOT NULL DEFAULT '[]',
			refresh_commands     JSONB DEFAULT '[]',
			drives               JSONB DEFAULT '[]',
			refresh_interval     VARCHAR(64) DEFAULT '',
			current_version      VARCHAR(255),
			status               VARCHAR(32) DEFAULT 'pending',
			created_at           TIMESTAMP DEFAULT NOW(),
			updated_at           TIMESTAMP DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_layers_parent ON snapshot_layers(parent_layer_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_layers_status ON snapshot_layers(status)`,
		`CREATE TABLE IF NOT EXISTS snapshot_builds (
			build_id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			layer_hash        VARCHAR(64) NOT NULL REFERENCES snapshot_layers(layer_hash),
			version           VARCHAR(255) NOT NULL,
			status            VARCHAR(32) DEFAULT 'queued',
			build_type        VARCHAR(16) DEFAULT 'init',
			instance_name     VARCHAR(255),
			parent_version    VARCHAR(255),
			started_at        TIMESTAMP,
			completed_at      TIMESTAMP,
			failure_reason    TEXT,
			retry_count       INT DEFAULT 0,
			max_retries       INT DEFAULT 3,
			created_at        TIMESTAMP DEFAULT NOW(),
			UNIQUE(layer_hash, version)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_builds_status ON snapshot_builds(status, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_builds_layer ON snapshot_builds(layer_hash)`,
		`CREATE TABLE IF NOT EXISTS layered_configs (
			config_id              VARCHAR(64) PRIMARY KEY,
			display_name           VARCHAR(255) NOT NULL,
			config_json            TEXT NOT NULL,
			leaf_layer_hash        VARCHAR(64),
			leaf_workload_key      VARCHAR(16),
			tier                   VARCHAR(8) DEFAULT 'm',
			ci_system              VARCHAR(64) DEFAULT '',
			github_app_id          VARCHAR(255) DEFAULT '',
			github_app_secret      VARCHAR(255) DEFAULT '',
			start_command          TEXT DEFAULT '',
			runner_ttl_seconds     INT DEFAULT 0,
			session_max_age_seconds INT DEFAULT 86400,
			auto_pause             BOOLEAN DEFAULT false,
			auto_rollout           BOOLEAN DEFAULT true,
			max_concurrent_runners INT DEFAULT 0,
			build_schedule         VARCHAR(64) DEFAULT '',
			created_at             TIMESTAMP DEFAULT NOW(),
			updated_at             TIMESTAMP DEFAULT NOW()
		)`,
		// Indexes for layered_configs lookups
		`CREATE INDEX IF NOT EXISTS idx_layered_configs_leaf_wk ON layered_configs(leaf_workload_key)`,
		`CREATE INDEX IF NOT EXISTS idx_layered_configs_leaf_hash ON layered_configs(leaf_layer_hash)`,
		// Network policy columns on layered_configs
		`ALTER TABLE layered_configs ADD COLUMN IF NOT EXISTS network_policy JSONB DEFAULT NULL`,
		`ALTER TABLE layered_configs ADD COLUMN IF NOT EXISTS network_policy_preset VARCHAR(64) DEFAULT ''`,
		// Mapping table: repo → workload_key for CI webhook routing.
		// This is a CI integration concern, not part of the core config model.
		`CREATE TABLE IF NOT EXISTS repo_workload_mappings (
			repo          VARCHAR(512) PRIMARY KEY,
			workload_key  VARCHAR(16) NOT NULL,
			source        VARCHAR(32) DEFAULT 'auto',
			created_at    TIMESTAMP DEFAULT NOW()
		)`,
		// Reattach build support: track old layer hash/version for drive reuse
		`ALTER TABLE snapshot_builds ADD COLUMN IF NOT EXISTS old_layer_hash VARCHAR(64)`,
		`ALTER TABLE snapshot_builds ADD COLUMN IF NOT EXISTS old_layer_version VARCHAR(255)`,
		// config_id links a build to the owning layered_configs row for tier/credential lookups
		`ALTER TABLE snapshot_builds ADD COLUMN IF NOT EXISTS config_id VARCHAR(64)`,
		// All-chain drives: union of drives across all layers in a config
		`ALTER TABLE snapshot_layers ADD COLUMN IF NOT EXISTS all_chain_drives JSONB DEFAULT '[]'`,
		// Re-activate layers that are referenced by a config but were incorrectly deactivated.
		`UPDATE snapshot_layers SET status='active' WHERE status='inactive'
			AND layer_hash IN (
				WITH RECURSIVE config_layers AS (
					SELECT sl.layer_hash, sl.parent_layer_hash
					FROM snapshot_layers sl
					JOIN layered_configs lc ON lc.leaf_layer_hash = sl.layer_hash
					UNION ALL
					SELECT sl.layer_hash, sl.parent_layer_hash
					FROM snapshot_layers sl
					JOIN config_layers cl ON cl.parent_layer_hash = sl.layer_hash
				)
				SELECT layer_hash FROM config_layers
			)`,
		// Deactivate orphaned layers not referenced by any config and cancel their active builds.
		// Walk the parent chain from each config's leaf_layer_hash to find all referenced layers.
		`UPDATE snapshot_layers SET status='inactive' WHERE status IN ('active', 'pending')
			AND layer_hash NOT IN (
				WITH RECURSIVE config_layers AS (
					SELECT sl.layer_hash, sl.parent_layer_hash
					FROM snapshot_layers sl
					JOIN layered_configs lc ON lc.leaf_layer_hash = sl.layer_hash
					UNION ALL
					SELECT sl.layer_hash, sl.parent_layer_hash
					FROM snapshot_layers sl
					JOIN config_layers cl ON cl.parent_layer_hash = sl.layer_hash
				)
				SELECT layer_hash FROM config_layers
			)`,
		`UPDATE snapshot_builds SET status='cancelled' WHERE status IN ('queued','waiting_parent','running')
			AND layer_hash IN (SELECT layer_hash FROM snapshot_layers WHERE status='inactive')`,
		// Snapshot tags for template versioning (WS6)
		`CREATE TABLE IF NOT EXISTS snapshot_tags (
			tag           VARCHAR(64) NOT NULL,
			workload_key  VARCHAR(16) NOT NULL,
			version       VARCHAR(255) NOT NULL,
			description   TEXT DEFAULT '',
			created_at    TIMESTAMP DEFAULT NOW(),
			PRIMARY KEY (tag, workload_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshot_tags_workload ON snapshot_tags(workload_key)`,
		// Denormalize config_id into snapshot_builds for efficient query joins
		`ALTER TABLE snapshot_builds ADD COLUMN IF NOT EXISTS config_id VARCHAR(64) DEFAULT ''`,
		// Prevent duplicate active builds per layer
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_builds_one_active_per_layer
			ON snapshot_builds (layer_hash) WHERE status IN ('queued', 'waiting_parent', 'running')`,
	}
	logger := logrus.WithField("component", "migrations")
	for i, stmt := range migrations {
		result, err := db.Exec(stmt)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			logger.WithFields(logrus.Fields{
				"migration": i,
				"rows":      n,
			}).Info("Migration applied")
		}
	}

	// Log layer statuses for debugging
	rows, err := db.Query(`SELECT layer_hash, config_name, status FROM snapshot_layers ORDER BY config_name`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var hash, name, status string
			rows.Scan(&hash, &name, &status)
			logger.WithFields(logrus.Fields{
				"layer_hash": hash[:16],
				"name":       name,
				"status":     status,
			}).Info("Layer status at startup")
		}
	}

	// Log active builds
	rows2, err := db.Query(`SELECT build_id, layer_hash, status, build_type FROM snapshot_builds WHERE status IN ('queued','waiting_parent','running') ORDER BY created_at`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var bid, hash, status, btype string
			rows2.Scan(&bid, &hash, &status, &btype)
			logger.WithFields(logrus.Fields{
				"build_id":   bid,
				"layer_hash": hash[:16],
				"status":     status,
				"build_type": btype,
			}).Info("Active build at startup")
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
	}).Info("RegisterHost request")

	host, err := s.hostRegistry.RegisterHost(ctx, req.InstanceName, req.Zone, "")
	if err != nil {
		return nil, err
	}

	currentSnapshot := s.snapshotManager.GetCurrentVersion()

	return &pb.RegisterHostResponse{
		HostId:          host.ID,
		SnapshotVersion: currentSnapshot,
	}, nil
}

// Heartbeat implements the gRPC Heartbeat method.
// The HTTP heartbeat endpoint (UpsertHeartbeat) is the primary path;
// this gRPC method is kept for proto interface compatibility.
func (s *ControlPlaneServer) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	if req.Status == nil {
		return &pb.HeartbeatResponse{Acknowledged: false}, nil
	}
	return &pb.HeartbeatResponse{
		Acknowledged: true,
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
		s.hostRegistry.mu.RLock()
		runnerInfos := h.RunnerInfos
		s.hostRegistry.mu.RUnlock()

		for _, ri := range runnerInfos {
			allRunners = append(allRunners, map[string]interface{}{
				"runner_id":    ri.RunnerID,
				"host_id":     h.ID,
				"host_name":   h.InstanceName,
				"workload_key": ri.WorkloadKey,
				"status":      ri.State,
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
// Body: {"workload_key": "abc123", "labels": {"firecracker": "true"}}
// ci_system is resolved from the snapshot config registered for the workload_key.
func (s *ControlPlaneServer) HandleAllocateRunner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RequestID           string            `json:"request_id"`
		WorkloadKey         string            `json:"workload_key"`
		Labels              map[string]string `json:"labels"`
		SessionID           string            `json:"session_id"`
		SnapshotTag         string            `json:"snapshot_tag"`
		NetworkPolicyPreset string            `json:"network_policy_preset"`
		NetworkPolicyJSON   string            `json:"network_policy_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.WorkloadKey == "" {
		http.Error(w, "workload_key is required", http.StatusBadRequest)
		return
	}
	if req.RequestID == "" {
		req.RequestID = fmt.Sprintf("manual-%d", time.Now().UnixNano())
	}

	s.logger.WithFields(logrus.Fields{
		"request_id":   req.RequestID,
		"workload_key": req.WorkloadKey,
	}).Info("Manual runner allocation request")

	resp, err := s.scheduler.AllocateRunner(r.Context(), AllocateRunnerRequest{
		RequestID:           req.RequestID,
		WorkloadKey:         req.WorkloadKey,
		Labels:              req.Labels,
		SessionID:           req.SessionID,
		SnapshotTag:         req.SnapshotTag,
		NetworkPolicyPreset: req.NetworkPolicyPreset,
		NetworkPolicyJSON:   req.NetworkPolicyJSON,
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

	// If host is draining/terminating, session runners are already being
	// proactively paused by the host agent — return 503 so the client retries
	// via /connect which will resume on a new host.
	if host.Status == "draining" || host.Status == "terminating" {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "host is draining"})
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
		if _, dbErr := s.scheduler.db.ExecContext(r.Context(), `
			INSERT INTO session_snapshots (session_id, runner_id, workload_key, host_id, status, layer_count, paused_at)
			VALUES ($1, $2, $3, $4, 'suspended', $5, NOW())
			ON CONFLICT (session_id) DO UPDATE SET
				status = 'suspended',
				layer_count = EXCLUDED.layer_count,
				paused_at = NOW()
		`, resp.SessionId, req.RunnerID, runner.WorkloadKey, host.ID, resp.Layer+1); dbErr != nil {
			s.logger.WithError(dbErr).WithField("session_id", resp.SessionId).Error("Failed to update session_snapshots table")
		}
	}

	// Roll back optimistic resource reservation — a paused runner no longer
	// consumes host resources. Without this, UsedCPU/Memory accumulates
	// until the next heartbeat.
	if runner.ReservedCPU > 0 || runner.ReservedMemoryMB > 0 {
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

		// Pick the host to resume on. Prefer the original host unless it's
		// draining/terminating or unreachable — in that case, fall back to
		// another available host using cache-affinity scheduling.
		var resumeHost *Host
		origHost, origErr := s.hostRegistry.GetHost(hostID)
		if origErr == nil && origHost.Status != "draining" && origHost.Status != "terminating" {
			resumeHost = origHost
		} else {
			// Look up workload_key for affinity scheduling
			var workloadKey string
			_ = s.scheduler.db.QueryRowContext(r.Context(),
				`SELECT workload_key FROM session_snapshots WHERE session_id = $1`, sessionID).Scan(&workloadKey)
			resumeHost = s.scheduler.selectBestHostForWorkloadKey(s.hostRegistry.GetAvailableHosts(), workloadKey)
		}
		if resumeHost == nil {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "no available host for session resume"})
			return
		}

		conn, err := grpc.NewClient(resumeHost.GRPCAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

		// Update session status and host assignment
		_, _ = s.scheduler.db.ExecContext(r.Context(),
			`UPDATE session_snapshots SET status = 'active', host_id = $1 WHERE session_id = $2`,
			resumeHost.ID, sessionID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"status":       "resumed",
			"runner_id":    resp.Runner.GetId(),
			"host_address": resumeHost.HTTPAddress,
		})
		return
	}

	// Runner exists — it's running, forward connect to extend TTL
	host, err := s.hostRegistry.GetHost(runner.HostID)
	if err != nil {
		http.Error(w, "host not found", http.StatusInternalServerError)
		return
	}

	// If the host is draining, the runner will soon be gone — tell the client to retry.
	if host.Status == "draining" || host.Status == "terminating" {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "host is draining, retry shortly"})
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
			"id":                   h.ID,
			"instance_name":        h.InstanceName,
			"zone":                 h.Zone,
			"status":               h.Status,
			"idle_runners":         h.IdleRunners,
			"busy_runners":         h.BusyRunners,
			"snapshot_version":     h.SnapshotVersion,
			"last_heartbeat":       h.LastHeartbeat,
			"grpc_address":         h.GRPCAddress,
			"total_cpu_millicores": h.TotalCPUMillicores,
			"used_cpu_millicores":  h.UsedCPUMillicores,
			"total_memory_mb":      h.TotalMemoryMB,
			"used_memory_mb":       h.UsedMemoryMB,
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
// GET /api/v1/versions/fleet?workload_key={key}
func (s *ControlPlaneServer) HandleGetFleetConvergence(w http.ResponseWriter, r *http.Request) {
	workloadKey := r.URL.Query().Get("workload_key")
	if workloadKey == "" {
		http.Error(w, "workload_key is required", http.StatusBadRequest)
		return
	}

	statuses, err := s.snapshotManager.GetFleetConvergence(r.Context(), workloadKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workload_key": workloadKey,
		"hosts":        statuses,
		"count":        len(statuses),
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
			fleetCPUTotal := int64(0)
			fleetCPUUsed := int64(0)
			fleetMemTotal := int64(0)
			fleetMemUsed := int64(0)
			activeHosts := int64(0)

			for _, h := range hosts {
				statusCounts[h.Status]++
				totalRunners += int64(h.IdleRunners + h.BusyRunners)
				totalIdle += int64(h.IdleRunners)
				totalBusy += int64(h.BusyRunners)
				if h.Status == "ready" {
					fleetCPUTotal += int64(h.TotalCPUMillicores)
					fleetCPUUsed += int64(h.UsedCPUMillicores)
					fleetMemTotal += int64(h.TotalMemoryMB)
					fleetMemUsed += int64(h.UsedMemoryMB)
					activeHosts++
				}
			}

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

			// Record fleet resource metrics
			mc.RecordInt(ctx, telemetry.MetricCPFleetCPUTotal, fleetCPUTotal, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetCPUUsed, fleetCPUUsed, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetCPUFree, fleetCPUTotal-fleetCPUUsed, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetMemTotal, fleetMemTotal, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetMemUsed, fleetMemUsed, nil)
			mc.RecordInt(ctx, telemetry.MetricCPFleetMemFree, fleetMemTotal-fleetMemUsed, nil)

			// Record slot-based fleet utilization for autoscaler
			defaultTier, _ := tiers.Lookup(tiers.DefaultTier)
			defaultCPU := tiers.EffectiveCPUMillicores(defaultTier)
			defaultMem := defaultTier.MemoryMB

			totalSlots := 0
			usedSlots := 0
			for _, h := range hosts {
				if h.Status != "ready" || h.TotalCPUMillicores == 0 {
					continue
				}
				hostTotal := min(h.TotalCPUMillicores/defaultCPU, h.TotalMemoryMB/defaultMem)
				freeCPU := max((h.TotalCPUMillicores-h.UsedCPUMillicores)/defaultCPU, 0)
				freeMem := max((h.TotalMemoryMB-h.UsedMemoryMB)/defaultMem, 0)
				hostFree := min(freeCPU, freeMem)
				totalSlots += hostTotal
				usedSlots += hostTotal - hostFree
			}

			queueDepth := sched.GetQueueDepth()
			var utilization float64
			if totalSlots > 0 {
				utilization = float64(usedSlots+queueDepth) / float64(totalSlots)
			} else if queueDepth > 0 {
				utilization = 10.0 // no capacity but demand exists — force scale-up
			}
			mc.RecordFloat(ctx, telemetry.MetricCPFleetUtilization, utilization, nil)

			// Record queue depth
			mc.RecordInt(ctx, telemetry.MetricCPQueueDepth, int64(queueDepth), nil)

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
