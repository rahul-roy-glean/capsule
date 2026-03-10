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
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	cigithub "github.com/rahul-roy-glean/bazel-firecracker/pkg/ci/github"
	fcrotel "github.com/rahul-roy-glean/bazel-firecracker/pkg/otel"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/tiers"
)

var (
	grpcPort           = flag.Int("grpc-port", 50051, "gRPC server port")
	httpPort           = flag.Int("http-port", 8080, "HTTP server port")
	dbHost             = flag.String("db-host", "localhost", "Database host")
	dbPort             = flag.Int("db-port", 5432, "Database port")
	dbUser             = flag.String("db-user", "postgres", "Database user")
	dbPassword         = flag.String("db-password", "", "Database password")
	dbName             = flag.String("db-name", "firecracker_runner", "Database name")
	dbSSLMode          = flag.String("db-ssl-mode", "disable", "Database SSL mode")
	gcsBucket          = flag.String("gcs-bucket", "", "GCS bucket for snapshots")
	gcsPrefix          = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")
	logLevel           = flag.String("log-level", "info", "Log level")
	apiAuthToken       = flag.String("api-auth-token", "", "Bearer token required for control-plane API requests (or API_AUTH_TOKEN env var)")
	hostBootstrapToken = flag.String("host-bootstrap-token", "", "Bearer token required for host heartbeat requests (or HOST_BOOTSTRAP_TOKEN env var)")
	skipMigrations     = flag.Bool("skip-migrations", false, "Skip database migrations at startup")

	// Telemetry
	gcpProject  = flag.String("gcp-project", "", "GCP project for telemetry and snapshot builder VMs")
	gcpZone     = flag.String("gcp-zone", "us-central1-a", "GCP zone for snapshot builder VMs")
	environment = flag.String("environment", "dev", "Environment name for telemetry labels")

	// MCP server
	mcpPort      = flag.Int("mcp-port", 0, "MCP server port (0 = disabled)")
	mcpAuthToken = flag.String("mcp-auth-token", "", "Bearer token for MCP authentication (or MCP_AUTH_TOKEN env var)")
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
	if v := os.Getenv("MCP_AUTH_TOKEN"); v != "" && *mcpAuthToken == "" {
		*mcpAuthToken = v
	}
	if v := os.Getenv("API_AUTH_TOKEN"); v != "" && *apiAuthToken == "" {
		*apiAuthToken = v
	}
	if v := os.Getenv("HOST_BOOTSTRAP_TOKEN"); v != "" && *hostBootstrapToken == "" {
		*hostBootstrapToken = v
	}
	if v := os.Getenv("SKIP_MIGRATIONS"); v != "" && !*skipMigrations {
		if parsed, err := strconv.ParseBool(v); err == nil {
			*skipMigrations = parsed
		}
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
	if err := initSchema(db, *skipMigrations); err != nil {
		log.WithError(err).Fatal("Failed to initialize schema")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize OpenTelemetry
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

	otelCfg := fcrotel.ConfigFromEnv("control-plane")
	if envVal != "" {
		otelCfg.Environment = envVal
	}
	otelClient, otelErr := fcrotel.Init(ctx, otelCfg)
	if otelErr != nil {
		log.WithError(otelErr).Warn("Failed to initialize OpenTelemetry, continuing without telemetry")
		otelClient, _ = fcrotel.Init(ctx, fcrotel.Config{ServiceName: "control-plane"})
	}
	defer otelClient.Shutdown(ctx)

	// Add trace correlation to logrus
	logger.AddHook(&fcrotel.TraceCorrelationHook{})

	// Create OTel instruments
	meter := otelClient.Meter("control-plane")
	cpHostsGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPHostsTotal)
	cpRunnersGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPRunnersTotal)
	cpQueueDepthGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPQueueDepth)
	cpFleetCPUTotalGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPFleetCPUTotal)
	cpFleetCPUUsedGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPFleetCPUUsed)
	cpFleetCPUFreeGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPFleetCPUFree)
	cpFleetMemTotalGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPFleetMemTotal)
	cpFleetMemUsedGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPFleetMemUsed)
	cpFleetMemFreeGauge, _ := fcrotel.NewGauge(meter, fcrotel.CPFleetMemFree)
	cpFleetUtilGauge, _ := fcrotel.NewFloat64Gauge(meter, fcrotel.CPFleetUtilization)
	snapshotAgeGauge, _ := fcrotel.NewGauge(meter, fcrotel.SnapshotAge)
	canarySuccessCounter, _ := fcrotel.NewCounter(meter, fcrotel.E2ECanarySuccess)
	canaryFailureCounter, _ := fcrotel.NewCounter(meter, fcrotel.E2ECanaryFailure)
	sessionResumeRoutingCounter, _ := fcrotel.NewCounter(meter, fcrotel.SessionResumeRouting)

	// Create services
	hostRegistry := NewHostRegistry(db, logger)
	snapshotManager := NewSnapshotManager(ctx, db, *gcsBucket, *gcsPrefix, gcpProjectVal, gcpZoneVal, logger)
	if v := os.Getenv("BUILDER_NETWORK"); v != "" {
		snapshotManager.builderNetwork = v
	}
	if v := os.Getenv("BUILDER_IMAGE"); v != "" {
		snapshotManager.builderImage = v
	}
	if v := os.Getenv("BUILDER_SERVICE_ACCOUNT"); v != "" {
		snapshotManager.builderServiceAccount = v
	}
	configCache := NewConfigCache(db, logger)
	tagRegistry := NewSnapshotTagRegistry(db, logger)
	scheduler := NewScheduler(hostRegistry, db, snapshotManager, tagRegistry, logger)
	scheduler.SetConfigCache(configCache)
	scheduler.SetOTel(otelClient, sessionResumeRoutingCounter)
	jobQueue := NewJobQueue(db, scheduler, hostRegistry, logger)
	jobQueue.SetConfigCache(configCache)
	layeredConfigRegistry := NewLayeredConfigRegistry(db, snapshotManager, logger)
	layeredConfigRegistry.SetConfigCache(configCache)
	layeredConfigRegistry.tagRegistry = tagRegistry
	layerBuildScheduler := NewLayerBuildScheduler(db, snapshotManager, logger, 4)
	layeredConfigRegistry.SetLayerBuilder(layerBuildScheduler)

	// Wire CI webhook adapter for GitHub Actions webhook routing.
	// The webhook-only adapter does not require GitHub App credentials.
	var webhookAdapter *cigithub.Adapter
	ciSystemEnv := os.Getenv("CI_SYSTEM")
	if ciSystemEnv == "" || ciSystemEnv == "github-actions" {
		webhookAdapter = cigithub.NewWebhookAdapter(logger)
		webhookAdapter.SetWebhookDeps(cigithub.WebhookDeps{
			JobQueue:       jobQueue,
			RunnerReleaser: scheduler,
			RunnerLookup:   &jobQueueRunnerLookup{hr: hostRegistry},
			Logger:         logger,
		})
	}

	// Load existing state from DB (best-effort)
	if err := hostRegistry.LoadFromDB(ctx); err != nil {
		log.WithError(err).Warn("Failed to load host/runner state from DB")
	}

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(otelClient.TracerProvider),
			otelgrpc.WithMeterProvider(otelClient.MeterProvider),
		)),
	)

	// Register services
	controlPlaneServer := NewControlPlaneServer(scheduler, hostRegistry, snapshotManager, jobQueue, canarySuccessCounter, canaryFailureCounter, logger)
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
	httpMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("metrics served via OTel Collector"))
	})
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
	// Register webhook handler via CI adapter (replaces hardcoded CI_SYSTEM check)
	if webhookAdapter != nil {
		if h := webhookAdapter.WebhookHandler(); h != nil {
			httpMux.Handle(webhookAdapter.WebhookPath(), h)
		}
	}

	httpHandler := apiLoggingMiddleware(logger, authMiddleware(*apiAuthToken, *hostBootstrapToken, httpMux))
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *httpPort),
		Handler: httpHandler,
	}

	go func() {
		log.WithField("port", *httpPort).Info("Starting HTTP server")
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.WithError(err).Error("HTTP server error")
		}
	}()

	// Optionally start MCP server on dedicated port
	var mcpServer *http.Server
	if *mcpPort > 0 {
		deps := &mcpDeps{
			scheduler:    scheduler,
			hostRegistry: hostRegistry,
			db:           db,
			logger:       logger.WithField("component", "mcp"),
		}
		mcpHandler := newMCPHandler(deps, *mcpAuthToken)

		mcpMux := http.NewServeMux()
		mcpMux.Handle("/mcp", mcpHandler)
		mcpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})

		mcpServer = &http.Server{
			Addr:    fmt.Sprintf(":%d", *mcpPort),
			Handler: mcpMux,
		}
		go func() {
			log.WithField("port", *mcpPort).Info("Starting MCP server")
			if err := mcpServer.ListenAndServe(); err != http.ErrServerClosed {
				log.WithError(err).Error("MCP server error")
			}
		}()
	}

	// Start background workers
	go hostRegistry.HealthCheckLoop(ctx)
	go startDownscaler(ctx, db, hostRegistry, scheduler, logger)
	go jobQueue.jobRetryLoop(ctx)
	go controlPlaneServer.startTTLEnforcement(ctx)
	go layerBuildScheduler.Run(ctx)
	go controlPlaneMetricsLoop(ctx, hostRegistry, scheduler, snapshotManager,
		cpHostsGauge, cpRunnersGauge, cpQueueDepthGauge,
		cpFleetCPUTotalGauge, cpFleetCPUUsedGauge, cpFleetCPUFreeGauge,
		cpFleetMemTotalGauge, cpFleetMemUsedGauge, cpFleetMemFreeGauge,
		cpFleetUtilGauge, snapshotAgeGauge, logger)

	// Wait for shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	grpcServer.GracefulStop()
	httpServer.Shutdown(shutdownCtx)
	if mcpServer != nil {
		mcpServer.Shutdown(shutdownCtx)
	}

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

func authMiddleware(apiToken, hostToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		var required string
		switch {
		case r.URL.Path == "/api/v1/hosts/heartbeat":
			required = hostToken
		case strings.HasPrefix(r.URL.Path, "/api/v1/"):
			required = apiToken
		default:
			next.ServeHTTP(w, r)
			return
		}

		if required == "" {
			next.ServeHTTP(w, r)
			return
		}

		if hasAuthToken(r, required) {
			next.ServeHTTP(w, r)
			return
		}

		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	})
}

func hasAuthToken(r *http.Request, expected string) bool {
	if expected == "" {
		return true
	}

	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(authz, "Bearer ")) == expected
}

func initSchema(db *sql.DB, skip bool) error {
	if !skip {
		if err := runMigrations(db); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	logger := logrus.WithField("component", "schema")

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
	scheduler            *Scheduler
	hostRegistry         *HostRegistry
	snapshotManager      *SnapshotManager
	jobQueue             *JobQueue
	canarySuccessCounter metric.Int64Counter
	canaryFailureCounter metric.Int64Counter
	logger               *logrus.Entry
}

func NewControlPlaneServer(s *Scheduler, h *HostRegistry, sm *SnapshotManager, jq *JobQueue, canarySuccess, canaryFailure metric.Int64Counter, l *logrus.Logger) *ControlPlaneServer {
	return &ControlPlaneServer{
		scheduler:            s,
		hostRegistry:         h,
		snapshotManager:      sm,
		jobQueue:             jq,
		canarySuccessCounter: canarySuccess,
		canaryFailureCounter: canaryFailure,
		logger:               l.WithField("service", "control-plane"),
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
				"host_id":      h.ID,
				"host_name":    h.InstanceName,
				"workload_key": ri.WorkloadKey,
				"status":       ri.State,
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
		var networkPolicy any
		if runner.NetworkPolicyJSON != "" {
			networkPolicy = runner.NetworkPolicyJSON
		}
		if _, dbErr := s.scheduler.db.ExecContext(r.Context(), `
			INSERT INTO session_snapshots (
				session_id, runner_id, workload_key, host_id, status, layer_count, paused_at,
				runner_ttl_seconds, auto_pause, network_policy_preset, network_policy
			)
			VALUES ($1, $2, $3, $4, 'suspended', $5, NOW(), $6, $7, $8, $9)
			ON CONFLICT (session_id) DO UPDATE SET
				status = 'suspended',
				layer_count = EXCLUDED.layer_count,
				paused_at = NOW(),
				runner_ttl_seconds = EXCLUDED.runner_ttl_seconds,
				auto_pause = EXCLUDED.auto_pause,
				network_policy_preset = EXCLUDED.network_policy_preset,
				network_policy = EXCLUDED.network_policy
		`, resp.SessionId, req.RunnerID, runner.WorkloadKey, host.ID, resp.Layer+1,
			runner.RunnerTTLSeconds, runner.AutoPause, runner.NetworkPolicyPreset, networkPolicy); dbErr != nil {
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

	// Remove the runner from the in-memory host registry so that
	// HandleRunnerStatus falls through to the session_snapshots DB lookup
	// and returns "suspended" immediately (instead of stale "ready" until
	// the next heartbeat).
	if err := s.hostRegistry.RemoveRunner(req.RunnerID); err != nil {
		s.logger.WithError(err).WithField("runner_id", req.RunnerID).Warn("Failed to remove paused runner from registry")
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
		var sessionID, hostID, workloadKey string
		var status string
		var sessionTTL sql.NullInt64
		var sessionAutoPause sql.NullBool
		var sessionNPPreset sql.NullString
		var sessionNPJSON sql.NullString
		scanErr := s.scheduler.db.QueryRowContext(r.Context(),
			`SELECT session_id, host_id, workload_key, status, runner_ttl_seconds, auto_pause, network_policy_preset, network_policy
			 FROM session_snapshots WHERE runner_id = $1`,
			req.RunnerID).Scan(&sessionID, &hostID, &workloadKey, &status, &sessionTTL, &sessionAutoPause, &sessionNPPreset, &sessionNPJSON)
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
		resumeReq := &pb.ResumeRunnerRequest{
			SessionId:           sessionID,
			WorkloadKey:         workloadKey,
			TtlSeconds:          int32(sessionTTL.Int64),
			AutoPause:           sessionAutoPause.Valid && sessionAutoPause.Bool,
			NetworkPolicyPreset: sessionNPPreset.String,
		}
		if sessionNPJSON.Valid {
			resumeReq.NetworkPolicyJson = sessionNPJSON.String
		}
		resp, err := client.ResumeRunner(r.Context(), resumeReq)
		if err != nil || resp.Error != "" {
			var errMsg string
			if err != nil {
				errMsg = err.Error()
			} else {
				errMsg = resp.Error
			}
			http.Error(w, errMsg, http.StatusInternalServerError)
			return
		}

		resumedRunnerID := resp.Runner.GetId()
		if resumedRunnerID == "" {
			http.Error(w, "resume succeeded but returned empty runner id", http.StatusInternalServerError)
			return
		}

		if err := s.hostRegistry.AddRunner(r.Context(), &Runner{
			ID:                  resumedRunnerID,
			HostID:              resumeHost.ID,
			Status:              "busy",
			InternalIP:          resp.Runner.GetInternalIp(),
			WorkloadKey:         workloadKey,
			RunnerTTLSeconds:    int(sessionTTL.Int64),
			AutoPause:           sessionAutoPause.Valid && sessionAutoPause.Bool,
			NetworkPolicyPreset: sessionNPPreset.String,
			NetworkPolicyJSON:   sessionNPJSON.String,
		}); err != nil {
			http.Error(w, "failed to register resumed runner", http.StatusInternalServerError)
			return
		}

		// Update session status and host assignment
		_, _ = s.scheduler.db.ExecContext(r.Context(),
			`UPDATE session_snapshots SET runner_id = $1, status = 'active', host_id = $2 WHERE session_id = $3`,
			resumedRunnerID, resumeHost.ID, sessionID)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"status":       "resumed",
			"runner_id":    resumedRunnerID,
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

	if report.Status == "success" {
		if s.canarySuccessCounter != nil {
			s.canarySuccessCounter.Add(r.Context(), 1)
		}
	} else {
		if s.canaryFailureCounter != nil {
			s.canaryFailureCounter.Add(r.Context(), 1)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// controlPlaneMetricsLoop periodically records control plane metrics via OpenTelemetry
func controlPlaneMetricsLoop(ctx context.Context, hr *HostRegistry, sched *Scheduler, sm *SnapshotManager,
	cpHostsGauge, cpRunnersGauge, cpQueueDepthGauge metric.Int64Gauge,
	cpFleetCPUTotalGauge, cpFleetCPUUsedGauge, cpFleetCPUFreeGauge metric.Int64Gauge,
	cpFleetMemTotalGauge, cpFleetMemUsedGauge, cpFleetMemFreeGauge metric.Int64Gauge,
	cpFleetUtilGauge metric.Float64Gauge, snapshotAgeGauge metric.Int64Gauge,
	logger *logrus.Logger) {

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
				cpHostsGauge.Record(ctx, count, metric.WithAttributes(
					fcrotel.AttrStatus.String(status),
				))
			}

			// Record runner totals
			cpRunnersGauge.Record(ctx, totalRunners, metric.WithAttributes(
				fcrotel.AttrStatus.String("total"),
			))
			cpRunnersGauge.Record(ctx, totalIdle, metric.WithAttributes(
				fcrotel.AttrStatus.String("idle"),
			))
			cpRunnersGauge.Record(ctx, totalBusy, metric.WithAttributes(
				fcrotel.AttrStatus.String("busy"),
			))

			// Record fleet resource metrics
			cpFleetCPUTotalGauge.Record(ctx, fleetCPUTotal)
			cpFleetCPUUsedGauge.Record(ctx, fleetCPUUsed)
			cpFleetCPUFreeGauge.Record(ctx, fleetCPUTotal-fleetCPUUsed)
			cpFleetMemTotalGauge.Record(ctx, fleetMemTotal)
			cpFleetMemUsedGauge.Record(ctx, fleetMemUsed)
			cpFleetMemFreeGauge.Record(ctx, fleetMemTotal-fleetMemUsed)

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
			cpFleetUtilGauge.Record(ctx, utilization)

			// Record queue depth
			cpQueueDepthGauge.Record(ctx, int64(queueDepth))

			// Record snapshot age
			currentVersion := sm.GetCurrentVersion()
			if currentVersion != "" {
				if snapshot, err := sm.GetSnapshot(ctx, currentVersion); err == nil && snapshot != nil {
					age := time.Since(snapshot.CreatedAt)
					snapshotAgeGauge.Record(ctx, int64(age.Seconds()))
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
