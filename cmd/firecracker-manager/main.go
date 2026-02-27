package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	pb "github.com/rahul-roy-glean/bazel-firecracker/api/proto/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/ci"
	cigithub "github.com/rahul-roy-glean/bazel-firecracker/pkg/ci/github"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/metrics"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/network"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/runner"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/snapshot"
	"github.com/rahul-roy-glean/bazel-firecracker/pkg/telemetry"
)

var (
	grpcPort             = flag.Int("grpc-port", 50051, "gRPC server port")
	httpPort             = flag.Int("http-port", 8080, "HTTP server port (health/metrics)")
	maxRunners           = flag.Int("max-runners", 16, "Maximum runners per host")
	idleTarget           = flag.Int("idle-target", 2, "Target number of idle runners")
	firecrackerBin       = flag.String("firecracker-bin", "/usr/local/bin/firecracker", "Path to firecracker binary")
	socketDir            = flag.String("socket-dir", "/var/run/firecracker", "Directory for VM sockets")
	workspaceDir         = flag.String("workspace-dir", "/mnt/data/workspaces", "Directory for workspaces")
	logDir               = flag.String("log-dir", "/var/log/firecracker", "Directory for VM logs")
	snapshotBucket       = flag.String("snapshot-bucket", "", "GCS bucket for snapshots")
	snapshotCache        = flag.String("snapshot-cache", "/mnt/data/snapshots", "Local snapshot cache path")
	repoCacheUpperSizeGB = flag.Int("repo-cache-upper-size-gb", 10, "Size in GB of the per-runner repo cache writable layer (upper)")
	buildbarnCertsDir    = flag.String("buildbarn-certs-dir", "", "Host directory containing Buildbarn certs to mount into microVMs (e.g. /etc/glean/ci/certs)")
	buildbarnCertsMount  = flag.String("buildbarn-certs-mount", "/etc/bazel-firecracker/certs/buildbarn", "Guest mount path for Buildbarn certs inside the microVM")
	buildbarnCertsSizeMB = flag.Int("buildbarn-certs-image-size-mb", 32, "Size in MB of the generated Buildbarn certs ext4 image")
	quarantineDir        = flag.String("quarantine-dir", "/mnt/data/quarantine", "Directory to store quarantined runner manifests and debug metadata")
	microVMSubnet        = flag.String("microvm-subnet", "172.16.0.0/24", "Subnet for microVMs")
	extInterface         = flag.String("ext-interface", "eth0", "External network interface")
	bridgeName           = flag.String("bridge-name", "fcbr0", "Bridge name for microVMs")
	environment          = flag.String("environment", "dev", "Environment name")
	controlPlane         = flag.String("control-plane", "", "Control plane address")
	logLevel             = flag.String("log-level", "info", "Log level (debug, info, warn, error)")

	// Git cache flags
	gitCacheEnabled       = flag.Bool("git-cache-enabled", false, "Enable git-cache reference cloning for faster repo setup")
	gitCacheDir           = flag.String("git-cache-dir", "/mnt/data/git-cache", "Host directory containing git mirrors")
	gitCacheImagePath     = flag.String("git-cache-image", "/mnt/data/git-cache.img", "Path to git-cache block device image")
	gitCacheMountPath     = flag.String("git-cache-mount", "/mnt/git-cache", "Mount path inside microVMs for git-cache")
	gitCacheRepos         = flag.String("git-cache-repos", "", "Comma-separated repo mappings (e.g. 'github.com/org/repo:repo-dir,github.com/org/other:other-dir')")
	gitCacheWorkspaceDir  = flag.String("git-cache-workspace", "/mnt/ephemeral/workdir", "Target directory for cloned repos inside microVMs")
	gitCachePreClonedPath = flag.String("git-cache-pre-cloned", "", "Path where repo was pre-cloned in snapshot (default: derived from repo URL)")

	// GitHub runner auto-registration flags (Option C)
	githubRunnerEnabled   = flag.Bool("github-runner-enabled", false, "Enable automatic GitHub runner registration at VM boot")
	githubRepo            = flag.String("github-repo", "", "GitHub repository for runner registration (e.g., askscio/scio)")
	githubOrg             = flag.String("github-org", "", "GitHub organization for org-level runner registration (e.g., askscio). If set, uses org-level API instead of repo-level")
	githubRunnerLabels    = flag.String("github-runner-labels", "self-hosted,firecracker,Linux,X64", "Comma-separated runner labels")
	githubRunnerEphemeral = flag.Bool("runner-ephemeral", true, "Whether runners are ephemeral (one job per VM) or persistent")
	githubAppID           = flag.String("github-app-id", "", "GitHub App ID for authentication")
	githubAppSecret       = flag.String("github-app-secret", "", "Secret Manager secret name containing GitHub App private key")
	gcpProject            = flag.String("gcp-project", "", "GCP project for Secret Manager")

	// Telemetry flags
	telemetryEnabled = flag.Bool("telemetry-enabled", true, "Enable GCP Cloud Monitoring telemetry")
	telemetryPrefix  = flag.String("telemetry-prefix", "custom.googleapis.com/firecracker", "Custom metric prefix for Cloud Monitoring")

	// Chunked snapshot flags (BuildBuddy-style lazy loading)
	useChunkedSnapshots = flag.Bool("use-chunked-snapshots", false, "Enable chunked snapshot restore with UFFD (lazy memory) and FUSE (lazy disk)")
	chunkCacheSizeGB    = flag.Int("chunk-cache-size-gb", 2, "Size in GB of disk chunk LRU cache (FUSE)")
	memCacheSizeGB      = flag.Int("mem-cache-size-gb", 2, "Size in GB of memory chunk LRU cache (UFFD)")
	memBackend          = flag.String("mem-backend", "chunked", "Memory restore backend: 'chunked' (UFFD lazy loading, default) or 'file' (download full snapshot.mem at startup). Overrides the backend recorded in snapshot metadata.")
	gcsPrefix           = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")
	enableSessionChunks = flag.Bool("enable-session-chunks", false, "Enable cloud-backed session pause/resume. Uses --snapshot-bucket for chunk storage. When enabled, PauseRunner uploads chunks to GCS and ResumeFromSession fetches lazily via UFFD+FUSE.")

	// Network namespace mode (alternative to slot-based TAPs)
	useNetNS = flag.Bool("use-netns", false, "Use network namespaces instead of slot-based TAP devices (simplifies snapshot restore)")

	// CI system adapter flag
	ciSystem = flag.String("ci-system", "github-actions", "CI system integration (github-actions, none)")

	// Runner pooling flags (VM reuse across tasks)
	poolEnabled           = flag.Bool("pool-enabled", false, "Enable runner pooling for VM reuse across tasks")
	poolMaxRunners        = flag.Int("pool-max-runners", 0, "Max pooled runners (0 = derive from resources)")
	poolMaxTotalMemoryGB  = flag.Int("pool-max-total-memory-gb", 0, "Max total memory for pooled runners in GB (0 = unlimited)")
	poolMaxRunnerMemoryGB = flag.Int("pool-max-runner-memory-gb", 2, "Max memory per pooled runner in GB")
	poolMaxRunnerDiskGB   = flag.Int("pool-max-runner-disk-gb", 16, "Max disk per pooled runner in GB")
	poolRecycleTimeout    = flag.Int("pool-recycle-timeout-secs", 30, "Timeout for recycling operations in seconds")
)

func main() {
	flag.Parse()

	// Setup logger
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	log := logger.WithField("component", "firecracker-manager")
	log.Info("Starting firecracker-manager")

	// Wait for workspace directory to be on a real mount (not root fs).
	// The startup script mounts /mnt/data before starting the manager, but
	// if the service auto-starts before the startup script runs, /mnt/data
	// won't be mounted and we'd create files on the root fs that get hidden
	// when the data disk is later mounted over /mnt/data.
	waitForDataMount(log, *workspaceDir)

	// Get instance metadata
	hostID, instanceName, zone := getInstanceMetadata()
	log.WithFields(logrus.Fields{
		"host_id":       hostID,
		"instance_name": instanceName,
		"zone":          zone,
	}).Info("Instance metadata loaded")

	// Get snapshot bucket from metadata if not provided
	if *snapshotBucket == "" {
		*snapshotBucket = getMetadataAttribute("snapshot-bucket")
	}
	if *snapshotBucket == "" {
		log.Fatal("Snapshot bucket not configured")
	}

	// Get control plane address from instance metadata if not provided.
	if *controlPlane == "" {
		*controlPlane = getMetadataAttribute("control-plane")
	}

	// Parse git-cache repo mappings
	gitCacheRepoMappings := make(map[string]string)
	if *gitCacheRepos != "" {
		for _, mapping := range strings.Split(*gitCacheRepos, ",") {
			parts := strings.SplitN(strings.TrimSpace(mapping), ":", 2)
			if len(parts) == 2 {
				gitCacheRepoMappings[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	// Get git-cache enabled from metadata if not set via flag
	gitCacheEnabledVal := *gitCacheEnabled
	if !gitCacheEnabledVal {
		if v := getMetadataAttribute("git-cache-enabled"); v == "true" {
			gitCacheEnabledVal = true
		}
	}

	// Get GitHub runner config from metadata if not set via flags
	githubRunnerEnabledVal := *githubRunnerEnabled
	if !githubRunnerEnabledVal {
		if v := getMetadataAttribute("github-runner-enabled"); v == "true" {
			githubRunnerEnabledVal = true
		}
	}
	githubRepoVal := *githubRepo
	if githubRepoVal == "" {
		githubRepoVal = getMetadataAttribute("github-repo")
	}
	githubOrgVal := *githubOrg
	if githubOrgVal == "" {
		githubOrgVal = getMetadataAttribute("github-org")
	}
	githubAppIDVal := *githubAppID
	if githubAppIDVal == "" {
		githubAppIDVal = getMetadataAttribute("github-app-id")
	}
	githubAppSecretVal := *githubAppSecret
	if githubAppSecretVal == "" {
		githubAppSecretVal = getMetadataAttribute("github-app-secret")
	}
	gcpProjectVal := *gcpProject
	if gcpProjectVal == "" {
		gcpProjectVal = getMetadataAttribute("gcp-project")
		if gcpProjectVal == "" {
			// Try to get from project metadata
			gcpProjectVal = getProjectMetadata()
		}
	}

	// Parse GitHub runner labels
	var githubRunnerLabelsVal []string
	labelsStr := *githubRunnerLabels
	if labelsStr == "" {
		labelsStr = getMetadataAttribute("github-runner-labels")
	}
	if labelsStr != "" {
		for _, label := range strings.Split(labelsStr, ",") {
			if l := strings.TrimSpace(label); l != "" {
				githubRunnerLabelsVal = append(githubRunnerLabelsVal, l)
			}
		}
	}

	// Parse runner ephemeral setting (defaults to true)
	runnerEphemeralVal := *githubRunnerEphemeral
	if ephemeralStr := getMetadataAttribute("runner-ephemeral"); ephemeralStr != "" {
		runnerEphemeralVal = strings.ToLower(ephemeralStr) == "true"
	}

	// Create runner manager config
	cfg := runner.HostConfig{
		HostID:            hostID,
		InstanceName:      instanceName,
		Zone:              zone,
		MaxRunners:        *maxRunners,
		IdleTarget:        *idleTarget,
		FirecrackerBin:    *firecrackerBin,
		SocketDir:         *socketDir,
		WorkspaceDir:      *workspaceDir,
		LogDir:            *logDir,
		SnapshotBucket:    *snapshotBucket,
		SnapshotCachePath: *snapshotCache,
		QuarantineDir:     *quarantineDir,
		MicroVMSubnet:     *microVMSubnet,
		ExternalInterface: *extInterface,
		BridgeName:        *bridgeName,
		Environment:       *environment,
		ControlPlaneAddr:  *controlPlane,
		GCPProject:        gcpProjectVal,
		// Runner pooling configuration
		PoolEnabled:            *poolEnabled,
		PoolMaxRunners:         *poolMaxRunners,
		PoolMaxTotalMemoryGB:   *poolMaxTotalMemoryGB,
		PoolMaxRunnerMemoryGB:  *poolMaxRunnerMemoryGB,
		PoolMaxRunnerDiskGB:    *poolMaxRunnerDiskGB,
		PoolRecycleTimeoutSecs: *poolRecycleTimeout,
		// Bazel-specific settings
		Bazel: runner.BazelConfig{
			RepoCacheUpperSizeGB:      *repoCacheUpperSizeGB,
			BuildbarnCertsDir:         *buildbarnCertsDir,
			BuildbarnCertsMountPath:   *buildbarnCertsMount,
			BuildbarnCertsImageSizeMB: *buildbarnCertsSizeMB,
			GitCacheEnabled:           gitCacheEnabledVal,
			GitCacheDir:               *gitCacheDir,
			GitCacheImagePath:         *gitCacheImagePath,
			GitCacheMountPath:         *gitCacheMountPath,
			GitCacheRepoMappings:      gitCacheRepoMappings,
			GitCacheWorkspaceDir:      *gitCacheWorkspaceDir,
			GitCachePreClonedPath:     *gitCachePreClonedPath,
		},
		// CI system settings
		CI: runner.CIConfig{
			GitHubRunnerEnabled:   githubRunnerEnabledVal,
			GitHubRepo:            githubRepoVal,
			GitHubOrg:             githubOrgVal,
			GitHubRunnerLabels:    githubRunnerLabelsVal,
			GitHubRunnerEphemeral: runnerEphemeralVal,
			GitHubAppID:           githubAppIDVal,
			GitHubAppSecret:       githubAppSecretVal,
		},
	}

	// Enable cloud-backed session chunks using the snapshot bucket.
	if *enableSessionChunks {
		cfg.SessionChunkBucket = *snapshotBucket
	}
	cfg.GCSPrefix = *gcsPrefix

	// Detect host resources for bin-packing scheduler
	cfg.TotalCPUMillicores, cfg.TotalMemoryMB = detectHostResources(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Construct CI adapter
	var ciAdapter ci.Adapter
	ciSystemVal := *ciSystem
	if ciSystemVal == "" {
		ciSystemVal = getMetadataAttribute("ci-system")
		if ciSystemVal == "" {
			ciSystemVal = "github-actions" // default for backwards compat
		}
	}

	switch ciSystemVal {
	case "github-actions":
		if githubAppIDVal != "" && githubAppSecretVal != "" {
			adapter, err := cigithub.NewAdapter(ctx, cigithub.Config{
				AppID:      githubAppIDVal,
				AppSecret:  githubAppSecretVal,
				GCPProject: gcpProjectVal,
				Repo:       githubRepoVal,
				Org:        githubOrgVal,
				Labels:     githubRunnerLabelsVal,
				Ephemeral:  runnerEphemeralVal,
			}, logger)
			if err != nil {
				log.WithError(err).Warn("Failed to create GitHub CI adapter, falling back to no-op")
				ciAdapter = ci.NewNoopAdapter()
			} else {
				ciAdapter = adapter
				log.Info("GitHub Actions CI adapter initialized")
			}
		} else {
			log.Info("GitHub App not configured, using no-op CI adapter")
			ciAdapter = ci.NewNoopAdapter()
		}
	default:
		log.WithField("ci_system", ciSystemVal).Info("Using no-op CI adapter")
		ciAdapter = ci.NewNoopAdapter()
	}

	// Create runner manager (optionally with chunked snapshot support)
	var mgr *runner.Manager
	var chunkedMgr *runner.ChunkedManager

	if *useChunkedSnapshots {
		log.WithFields(logrus.Fields{
			"chunked_snapshots": *useChunkedSnapshots,
			"use_netns":         *useNetNS,
			"disk_cache_gb":     *chunkCacheSizeGB,
			"mem_cache_gb":      *memCacheSizeGB,
		}).Info("Creating chunked manager with BuildBuddy-style optimizations")

		chunkedCfg := runner.ChunkedManagerConfig{
			HostConfig:          cfg,
			CIAdapter:           ciAdapter,
			UseChunkedSnapshots: *useChunkedSnapshots,
			UseNetNS:            *useNetNS,
			ChunkCacheSizeBytes: int64(*chunkCacheSizeGB) * 1024 * 1024 * 1024,
			MemCacheSizeBytes:   int64(*memCacheSizeGB) * 1024 * 1024 * 1024,
			MemBackend:          *memBackend,
			GCSPrefix:           *gcsPrefix,
		}

		var err error
		chunkedMgr, err = runner.NewChunkedManager(ctx, chunkedCfg, logger)
		if err != nil {
			log.WithError(err).Fatal("Failed to create chunked runner manager")
		}
		defer chunkedMgr.Close()
		mgr = chunkedMgr.Manager // Use embedded manager for compatibility

		// In chunked mode, no downloads happen at startup. The kernel,
		// snapshot.mem (file mode), and manifest are all fetched on demand
		// when the first SyncManifest (heartbeat) or AllocateRunner arrives.
		log.Info("Chunked mode: deferring all downloads until first manifest sync")
	} else {
		var err error
		mgr, err = runner.NewManager(ctx, cfg, ciAdapter, logger)
		if err != nil {
			log.WithError(err).Fatal("Failed to create runner manager")
		}
		defer mgr.Close()

		// Wire --use-netns to the base manager for per-VM namespace isolation.
		// When --use-netns is set without --use-chunked-snapshots, the base
		// manager creates per-VM namespaces instead of using the shared bridge.
		if *useNetNS {
			netnsNet, err := network.NewNetNSNetwork(network.NetNSConfig{
				BridgeName:    *bridgeName,
				Subnet:        *microVMSubnet,
				ExternalIface: *extInterface,
				Logger:        logger,
			})
			if err != nil {
				log.WithError(err).Fatal("Failed to create netns network for base manager")
			}
			if err := netnsNet.Setup(); err != nil {
				log.WithError(err).Fatal("Failed to setup netns network for base manager")
			}
			mgr.SetNetNSNetwork(netnsNet)
			log.Info("Network namespace mode enabled for base manager")
		}
	}

	// Reconcile orphaned resources from previous incarnation
	go mgr.ReconcileOrphans(ctx)

	// Initialize telemetry
	telemetryCfg := telemetry.Config{
		Enabled:      *telemetryEnabled,
		ProjectID:    gcpProjectVal,
		MetricPrefix: *telemetryPrefix,
		Component:    "firecracker-manager",
		Environment:  *environment,
		InstanceID:   hostID,
		InstanceName: instanceName,
		Zone:         zone,
	}
	// Override from metadata if set
	if v := getMetadataAttribute("telemetry-enabled"); v != "" {
		telemetryCfg.Enabled = strings.ToLower(v) == "true"
	}

	var metricsClient *telemetry.Client
	if telemetryCfg.Enabled && telemetryCfg.ProjectID != "" {
		var telErr error
		metricsClient, telErr = telemetry.NewClient(ctx, telemetryCfg, logger)
		if telErr != nil {
			log.WithError(telErr).Warn("Failed to initialize telemetry, continuing without metrics")
		} else {
			defer metricsClient.Close()
			log.Info("GCP Cloud Monitoring telemetry initialized")
		}
	}

	// Register Prometheus metrics (legacy, for Prometheus scraping if still needed)
	metrics.RegisterHostMetrics()

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(loggingInterceptor(logger)),
	)

	// Register services
	var hostAgentServer *HostAgentServer
	if chunkedMgr != nil {
		hostAgentServer = NewHostAgentServerWithChunked(mgr, chunkedMgr, logger)
	} else {
		hostAgentServer = NewHostAgentServer(mgr, logger)
	}
	pb.RegisterHostAgentServer(grpcServer, hostAgentServer)
	if metricsClient != nil {
		hostAgentServer.SetMetricsClient(metricsClient)
	}

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// Enable reflection for debugging
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

	// Start HTTP server for health and metrics
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", healthHandler(mgr))
	httpMux.HandleFunc("/ready", readyHandler(mgr))
	httpMux.Handle("/metrics", promhttp.Handler())
	httpMux.HandleFunc("/api/v1/runners/quarantine", drainingGuard(mgr, quarantineRunnerHandler(mgr, logger)))
	httpMux.HandleFunc("/api/v1/runners/unquarantine", drainingGuard(mgr, unquarantineRunnerHandler(mgr, logger)))
	httpMux.HandleFunc("/snapshot/sync", snapshotSyncHandler(mgr, logger))
	httpMux.HandleFunc("/api/v1/gc", gcHandler(mgr, logger))
	httpMux.HandleFunc("/api/v1/pool/flush", poolFlushHandler(mgr, logger))
	httpMux.HandleFunc("/api/v1/pool/stats", poolStatsHandler(mgr, logger))
	httpMux.HandleFunc("/api/v1/runners/", drainingGuard(mgr, runnerProxyHandler(mgr, logger)))

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

	// Start autoscaler loop
	go autoscaleLoop(ctx, mgr, chunkedMgr, *idleTarget, logger, metricsClient)

	// Start heartbeat loop if control plane is configured
	if *controlPlane != "" {
		go heartbeatLoop(ctx, mgr, chunkedMgr, *controlPlane, instanceName, zone, *grpcPort, *httpPort, logger, metricsClient)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info("Shutting down...")

	// Mark host as draining so local middleware rejects new requests.
	mgr.SetDraining(true)

	// Send a drain heartbeat to the control plane so it stops allocating to this host.
	if *controlPlane != "" {
		sendDrainHeartbeat(mgr, chunkedMgr, *controlPlane, instanceName, zone, *grpcPort, *httpPort, logger)
	}

	// Pause all session-bound runners so their state is saved to GCS and they
	// can be resumed on another host.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 25*time.Second)
	paused, pauseErr := mgr.PauseSessionRunners(drainCtx)
	drainCancel()
	if pauseErr != nil {
		log.WithError(pauseErr).WithField("paused", paused).Warn("Some session runners failed to pause during drain")
	} else if paused > 0 {
		log.WithField("paused", paused).Info("Paused session runners during drain")
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpcServer.GracefulStop()
	httpServer.Shutdown(shutdownCtx)

	log.Info("Shutdown complete")
}

// drainingGuard wraps an HTTP handler and returns 503 with Retry-After if the
// host is draining. Health and readiness endpoints are NOT wrapped so GCE
// health checks continue to work.
func drainingGuard(mgr *runner.Manager, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mgr.IsDraining() {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "host is draining, retry via control plane",
			})
			return
		}
		next(w, r)
	}
}

func healthHandler(mgr *runner.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}

func readyHandler(mgr *runner.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := mgr.GetStatus()
		if status.SnapshotVersion == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("No snapshot loaded"))
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Ready: %d runners, snapshot: %s",
			status.ActiveRunners, status.SnapshotVersion)
	}
}

func snapshotSyncHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	log := logger.WithField("handler", "snapshot-sync")
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Get version from header or query param
		version := r.Header.Get("X-Snapshot-Version")
		if version == "" {
			version = r.URL.Query().Get("version")
		}
		// version is empty if not specified; SyncFromGCS will resolve via
		// current-pointer.json.

		log.WithField("version", version).Info("Snapshot sync requested")

		// Sync snapshot in background to avoid blocking the HTTP request
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			if err := mgr.SyncSnapshot(ctx, version); err != nil {
				log.WithError(err).Error("Failed to sync snapshot")
			} else {
				log.WithField("version", version).Info("Snapshot sync completed")
			}
		}()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "syncing",
			"version": version,
		})
	}
}

func autoscaleLoop(ctx context.Context, mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, idleTarget int, logger *logrus.Logger, metricsClient *telemetry.Client) {
	log := logger.WithField("component", "autoscaler")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := mgr.GetStatus()

			// Maintain idle target.
			// In chunked mode, skip pre-allocation: each workload key needs a different
			// snapshot, so generic warm VMs would load the wrong data and be
			// useless when an actual job arrives. The control plane drives
			// allocation on-demand via gRPC AllocateRunner with the correct
			// WorkloadKey. In single-snapshot (non-chunked) mode, warm pool is fine
			// because there is only one snapshot.
			if chunkedMgr != nil {
				// Chunked mode: no local pre-allocation; control plane drives it.
			} else if mgr.DiskUsage() > 0.85 {
				log.Warn("Disk usage exceeds 85%, skipping runner allocation")
			} else if !mgr.IsDraining() && status.IdleRunners < idleTarget && mgr.CanAddRunner(0, 0) {
				log.Debug("Adding runner to maintain idle pool")
				allocTimer := telemetry.NewStopwatch()
				_, err := mgr.AllocateRunner(ctx, runner.AllocateRequest{})
				if err != nil {
					log.WithError(err).Warn("Failed to allocate idle runner")
					if metricsClient != nil {
						metricsClient.IncrementCounter(ctx, telemetry.MetricVMAllocations, telemetry.Labels{
							telemetry.LabelResult: telemetry.ResultFailure,
							telemetry.LabelReason: "idle_pool",
						})
					}
				} else {
					if metricsClient != nil {
						metricsClient.RecordDuration(ctx, telemetry.MetricVMBootDuration, allocTimer.Elapsed(), telemetry.Labels{
							telemetry.LabelReason: "idle_pool",
						})
						metricsClient.IncrementCounter(ctx, telemetry.MetricVMAllocations, telemetry.Labels{
							telemetry.LabelResult: telemetry.ResultSuccess,
							telemetry.LabelReason: "idle_pool",
						})
					}
				}
			}

			// Update Prometheus metrics
			metrics.UpdateHostMetrics(
				status.TotalCPUMillicores,
				status.UsedCPUMillicores,
				status.TotalMemoryMB,
				status.UsedMemoryMB,
				status.IdleRunners,
				status.BusyRunners,
			)

			// Record GCP Cloud Monitoring metrics
			if metricsClient != nil {
				metricsClient.RecordHostMetrics(ctx, telemetry.HostMetrics{
					TotalCPUMillicores: status.TotalCPUMillicores,
					UsedCPUMillicores:  status.UsedCPUMillicores,
					TotalMemoryMB:      status.TotalMemoryMB,
					UsedMemoryMB:       status.UsedMemoryMB,
					IdleRunners:        status.IdleRunners,
					BusyRunners:        status.BusyRunners,
				})

				// Record chunked snapshot metrics
				if chunkedMgr != nil {
					cs := chunkedMgr.GetChunkedStats()
					metricsClient.RecordChunkedMetrics(ctx, telemetry.ChunkedMetrics{
						DiskCacheSize:    cs.DiskCacheSize,
						DiskCacheMaxSize: cs.DiskCacheMaxSize,
						DiskCacheItems:   cs.DiskCacheItems,
						MemCacheSize:     cs.MemCacheSize,
						MemCacheMaxSize:  cs.MemCacheMaxSize,
						MemCacheItems:    cs.MemCacheItems,
						PageFaults:       cs.TotalPageFaults,
						CacheHits:        cs.TotalCacheHits,
						ChunkFetches:     cs.TotalChunkFetches,
						DiskReads:        cs.TotalDiskReads,
						DiskWrites:       cs.TotalDiskWrites,
						DirtyChunks:      cs.TotalDirtyChunks,
					})
				}

				// Record runner pool metrics
				if pool := mgr.GetPool(); pool != nil {
					ps := pool.Stats()
					metricsClient.RecordPoolMetrics(ctx, telemetry.PoolMetrics{
						PooledRunners:   ps.PooledRunners,
						PoolHits:        ps.PoolHits,
						PoolMisses:      ps.PoolMisses,
						Evictions:       ps.Evictions,
						RecycleFailures: ps.RecycleFailures,
						MemoryUsedBytes: ps.MemoryUsageBytes,
						MemoryMaxBytes:  ps.MaxMemoryBytes,
					})
				}
			}
		}
	}
}

type hostHeartbeatRequest struct {
	InstanceName       string                       `json:"instance_name"`
	Zone               string                       `json:"zone"`
	GRPCAddress        string                       `json:"grpc_address"`
	HTTPAddress        string                       `json:"http_address"`
	IdleRunners        int                          `json:"idle_runners"`
	BusyRunners        int                          `json:"busy_runners"`
	SnapshotVersion    string                       `json:"snapshot_version"`
	Draining           bool                         `json:"draining"`
	DiskUsage          float64                      `json:"disk_usage"`
	TotalCPUMillicores int                          `json:"total_cpu_millicores"`
	UsedCPUMillicores  int                          `json:"used_cpu_millicores"`
	TotalMemoryMB      int                          `json:"total_memory_mb"`
	UsedMemoryMB       int                          `json:"used_memory_mb"`
	LoadedManifests    map[string]string            `json:"loaded_manifests,omitempty"`
	Runners            []runner.RunnerHeartbeatInfo `json:"runners,omitempty"`
}

type hostHeartbeatResponse struct {
	Acknowledged       bool              `json:"acknowledged"`
	ShouldDrain        bool              `json:"should_drain"`
	ShouldSyncSnapshot bool              `json:"should_sync_snapshot,omitempty"`
	SnapshotVersion    string            `json:"snapshot_version,omitempty"`
	DesiredVersions    map[string]string `json:"desired_versions,omitempty"`
	SyncVersions       map[string]string `json:"sync_versions,omitempty"`
	Error              string            `json:"error,omitempty"`
}

func heartbeatLoop(ctx context.Context, mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, controlPlane, instanceName, zone string, grpcPort, httpPort int, logger *logrus.Logger, metricsClient *telemetry.Client) {
	log := logger.WithField("component", "heartbeat")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	controlPlane = strings.TrimSpace(controlPlane)
	if !strings.HasPrefix(controlPlane, "http://") && !strings.HasPrefix(controlPlane, "https://") {
		controlPlane = "http://" + controlPlane
	}
	heartbeatURL := strings.TrimRight(controlPlane, "/") + "/api/v1/hosts/heartbeat"

	internalIP := getMetadataValue("instance/network-interfaces/0/ip")
	if internalIP == "" {
		internalIP = getLocalIPFallback()
	}

	client := &http.Client{Timeout: 5 * time.Second}
	wasDraining := mgr.IsDraining()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbTimer := telemetry.NewStopwatch()
			status := mgr.GetStatus()
			reqBody := hostHeartbeatRequest{
				InstanceName:       instanceName,
				Zone:               zone,
				GRPCAddress:        fmt.Sprintf("%s:%d", internalIP, grpcPort),
				HTTPAddress:        fmt.Sprintf("%s:%d", internalIP, httpPort),
				IdleRunners:        status.IdleRunners,
				BusyRunners:        status.BusyRunners,
				SnapshotVersion:    status.SnapshotVersion,
				Draining:           status.Draining,
				DiskUsage:          mgr.DiskUsage(),
				TotalCPUMillicores: status.TotalCPUMillicores,
				UsedCPUMillicores:  status.UsedCPUMillicores,
				TotalMemoryMB:      status.TotalMemoryMB,
				UsedMemoryMB:       status.UsedMemoryMB,
			}
			if chunkedMgr != nil {
				reqBody.LoadedManifests = chunkedMgr.GetLoadedManifests()
			}
			reqBody.Runners = mgr.GetRunnerHeartbeatInfo()

			b, _ := json.Marshal(reqBody)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, heartbeatURL, bytes.NewReader(b))
			if err != nil {
				log.WithError(err).Warn("Failed to create heartbeat request")
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				log.WithError(err).Warn("Heartbeat request failed")
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Record heartbeat latency
			if metricsClient != nil {
				metricsClient.RecordDuration(ctx, telemetry.MetricHostHeartbeatLatency, hbTimer.Elapsed(), nil)
			}

			if resp.StatusCode >= 400 {
				log.WithFields(logrus.Fields{
					"status": resp.StatusCode,
					"body":   string(body),
				}).Warn("Heartbeat rejected by control plane")
				continue
			}

			var hbResp hostHeartbeatResponse
			if err := json.Unmarshal(body, &hbResp); err != nil {
				log.WithError(err).WithField("body", string(body)).Warn("Failed to parse heartbeat response")
				continue
			}
			if hbResp.Error != "" {
				log.WithField("error", hbResp.Error).Warn("Control plane heartbeat error")
				continue
			}

			changed := mgr.SetDraining(hbResp.ShouldDrain)
			if changed {
				if hbResp.ShouldDrain {
					wasDraining = true

					// Remove labels from GitHub runners to prevent new jobs being scheduled
					labelsRemoved, err := mgr.RemoveRunnerLabels(ctx)
					if err != nil {
						log.WithError(err).WithField("labels_removed", labelsRemoved).Warn("Failed to remove some runner labels")
					} else if labelsRemoved > 0 {
						log.WithField("labels_removed", labelsRemoved).Info("Removed labels from GitHub runners")
					}

					// Drain idle runners (terminate them)
					drained, err := mgr.DrainIdleRunners(ctx)
					if err != nil {
						log.WithError(err).WithField("drained_idle_runners", drained).Warn("Failed to drain idle runners")
					} else {
						log.WithField("drained_idle_runners", drained).Info("Host entered draining mode")
					}

					// Pause session-bound runners so they can resume on another host
					paused, pauseErr := mgr.PauseSessionRunners(ctx)
					if pauseErr != nil {
						log.WithError(pauseErr).WithField("paused", paused).Warn("Failed to pause some session runners during drain")
					} else if paused > 0 {
						log.WithField("paused", paused).Info("Paused session runners during drain")
					}
				} else {
					wasDraining = false
					log.Info("Host exited draining mode")
				}
			} else if hbResp.ShouldDrain && !wasDraining {
				wasDraining = true
				_, _ = mgr.RemoveRunnerLabels(ctx)
				_, _ = mgr.DrainIdleRunners(ctx)
				_, _ = mgr.PauseSessionRunners(ctx)
			}

			// Handle snapshot sync directive from control plane
			if hbResp.ShouldSyncSnapshot && hbResp.SnapshotVersion != "" {
				log.WithField("snapshot_version", hbResp.SnapshotVersion).Info("Control plane requested snapshot sync")
				go func(version string) {
					syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Minute)
					defer syncCancel()
					if err := mgr.SyncSnapshot(syncCtx, version); err != nil {
						log.WithError(err).WithField("snapshot_version", version).Error("Failed to sync snapshot")
					} else {
						log.WithField("snapshot_version", version).Info("Snapshot sync completed")
					}
				}(hbResp.SnapshotVersion)
			}

			// Handle per-workload-key manifest sync directives
			if len(hbResp.SyncVersions) > 0 && chunkedMgr != nil {
				for workloadKey, version := range hbResp.SyncVersions {
					go func(key, ver string) {
						syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancel()
						if err := chunkedMgr.SyncManifest(syncCtx, key, ver); err != nil {
							log.WithError(err).WithFields(logrus.Fields{
								"workload_key": key,
								"version":      ver,
							}).Warn("Failed to sync manifest for workload key")
						}
					}(workloadKey, version)
				}
			}
		}
	}
}

// sendDrainHeartbeat sends a single heartbeat with Draining=true so the control
// plane marks this host as draining before the process exits.
func sendDrainHeartbeat(mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, controlPlane, instanceName, zone string, grpcPort, httpPort int, logger *logrus.Logger) {
	log := logger.WithField("component", "drain-heartbeat")

	cp := strings.TrimSpace(controlPlane)
	if !strings.HasPrefix(cp, "http://") && !strings.HasPrefix(cp, "https://") {
		cp = "http://" + cp
	}
	heartbeatURL := strings.TrimRight(cp, "/") + "/api/v1/hosts/heartbeat"

	internalIP := getMetadataValue("instance/network-interfaces/0/ip")
	if internalIP == "" {
		internalIP = getLocalIPFallback()
	}

	status := mgr.GetStatus()
	reqBody := hostHeartbeatRequest{
		InstanceName:       instanceName,
		Zone:               zone,
		GRPCAddress:        fmt.Sprintf("%s:%d", internalIP, grpcPort),
		HTTPAddress:        fmt.Sprintf("%s:%d", internalIP, httpPort),
		IdleRunners:        status.IdleRunners,
		BusyRunners:        status.BusyRunners,
		SnapshotVersion:    status.SnapshotVersion,
		Draining:           true,
		DiskUsage:          mgr.DiskUsage(),
		TotalCPUMillicores: status.TotalCPUMillicores,
		UsedCPUMillicores:  status.UsedCPUMillicores,
		TotalMemoryMB:      status.TotalMemoryMB,
		UsedMemoryMB:       status.UsedMemoryMB,
	}
	if chunkedMgr != nil {
		reqBody.LoadedManifests = chunkedMgr.GetLoadedManifests()
	}
	reqBody.Runners = mgr.GetRunnerHeartbeatInfo()

	b, _ := json.Marshal(reqBody)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, heartbeatURL, bytes.NewReader(b))
	if err != nil {
		log.WithError(err).Warn("Failed to create drain heartbeat request")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.WithError(err).Warn("Failed to send drain heartbeat")
		return
	}
	resp.Body.Close()
	log.WithField("status", resp.StatusCode).Info("Drain heartbeat sent to control plane")
}

func loggingInterceptor(logger *logrus.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		duration := time.Since(start)

		logger.WithFields(logrus.Fields{
			"method":   info.FullMethod,
			"duration": duration,
			"error":    err,
		}).Debug("gRPC request")

		return resp, err
	}
}

func getInstanceMetadata() (hostID, instanceName, zone string) {
	// Try to get from GCP metadata service
	hostID = getMetadataValue("instance/id")
	instanceName = getMetadataValue("instance/name")
	zone = getMetadataValue("instance/zone")
	if zone != "" {
		zone = path.Base(zone)
	}

	if hostID == "" {
		hostID = os.Getenv("HOST_ID")
		if hostID == "" {
			hostID = fmt.Sprintf("host-%d", time.Now().Unix())
		}
	}

	if instanceName == "" {
		instanceName = os.Getenv("INSTANCE_NAME")
		if instanceName == "" {
			hostname, _ := os.Hostname()
			instanceName = hostname
		}
	}

	if zone == "" {
		zone = os.Getenv("ZONE")
		if zone == "" {
			zone = "unknown"
		}
	}

	return
}

func getMetadataAttribute(attr string) string {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://metadata.google.internal/computeMetadata/v1/instance/attributes/%s", attr)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

func getProjectMetadata() string {
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://metadata.google.internal/computeMetadata/v1/project/project-id"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

func getMetadataValue(path string) string {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://metadata.google.internal/computeMetadata/v1/%s", strings.TrimPrefix(path, "/"))
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}

func getLocalIPFallback() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if (iface.Flags&net.FlagUp) == 0 || (iface.Flags&net.FlagLoopback) != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// detectHostResources detects the total CPU and memory available on this host.
// It reads from /proc/cpuinfo and /proc/meminfo on Linux. Returns (cpuMillicores, memoryMB).
func detectHostResources(log *logrus.Entry) (int, int) {
	var cpuMillicores, memoryMB int

	// Detect CPU count from /proc/cpuinfo
	cpuData, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		cpuCount := 0
		for _, line := range strings.Split(string(cpuData), "\n") {
			if strings.HasPrefix(line, "processor") {
				cpuCount++
			}
		}
		if cpuCount > 0 {
			cpuMillicores = cpuCount * 1000
		}
	}

	// Detect total memory from /proc/meminfo
	memData, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		for _, line := range strings.Split(string(memData), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := fmt.Sscanf(fields[1], "%d", &memoryMB); err == nil && kb == 1 {
						memoryMB = memoryMB / 1024 // kB -> MB
					}
				}
				break
			}
		}
	}

	// Fallback: try GCE machine-type metadata
	if cpuMillicores == 0 || memoryMB == 0 {
		log.Debug("Could not detect resources from /proc, will report 0 (slot-based fallback)")
	} else {
		log.WithFields(logrus.Fields{
			"cpu_millicores": cpuMillicores,
			"memory_mb":      memoryMB,
		}).Info("Detected host resources")
	}

	return cpuMillicores, memoryMB
}

func gcpTokenHandler(w http.ResponseWriter, r *http.Request, logger *logrus.Logger) {
	// Fetch a fresh GCP access token from the metadata server
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET",
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
		nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to fetch GCP token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func gcHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "GC not yet implemented"})
	}
}

func poolFlushHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		pool := mgr.GetPool()
		if pool == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "pool_disabled"})
			return
		}
		olderThan := r.URL.Query().Get("older_than")
		ctx := r.Context()
		evicted := pool.FlushOlderThan(ctx, olderThan)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"evicted": evicted,
		})
	}
}

func poolStatsHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool := mgr.GetPool()
		if pool == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "pool_disabled"})
			return
		}
		stats := pool.Stats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	}
}

// runnerProxyHandler reverse-proxies HTTP requests to a specific microVM's service.
//
// URL pattern: /api/v1/runners/{runnerID}/proxy/{path...}
//
// The handler looks up the runner by ID, gets its InternalIP (which is the
// host-reachable veth IP in netns mode), and proxies the request to the user's
// service port (or the thaw-agent health port if no service is configured).
//
// This allows external clients to reach services running inside microVMs
// (e.g., claude_sandbox_service) without knowing about network namespaces,
// veth IPs, or DNAT. The client just needs the host address and runner ID.
func runnerProxyHandler(mgr *runner.Manager, logger *logrus.Logger) http.HandlerFunc {
	log := logger.WithField("handler", "runner-proxy")
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse URL: /api/v1/runners/{runnerID}/proxy/{path...}
		// Strip the prefix to get {runnerID}/proxy/{path...}
		suffix := strings.TrimPrefix(r.URL.Path, "/api/v1/runners/")

		// Don't proxy quarantine/unquarantine endpoints (they're registered separately)
		if strings.HasPrefix(suffix, "quarantine") || strings.HasPrefix(suffix, "unquarantine") {
			http.NotFound(w, r)
			return
		}

		// Handle /api/v1/runners/{id}/token/gcp (GCP token refresh for long jobs)
		if tokenParts := strings.SplitN(suffix, "/token/gcp", 2); len(tokenParts) == 2 && tokenParts[1] == "" {
			gcpTokenHandler(w, r, logger)
			return
		}

		// Handle /api/v1/runners/{id}/exec (execute command in VM)
		if execParts := strings.SplitN(suffix, "/exec", 2); len(execParts) == 2 && execParts[1] == "" {
			runnerID := execParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			handleExecCommand(w, r, mgr, log, runnerID)
			return
		}

		// Handle /api/v1/runners/{id}/service-logs (proxy to thaw-agent's service-logs)
		if slParts := strings.SplitN(suffix, "/service-logs", 2); len(slParts) == 2 {
			runnerID := strings.TrimSuffix(slParts[0], "/")
			handleServiceLogs(w, r, mgr, log, runnerID, slParts[1])
			return
		}

		// Handle /api/v1/runners/{id}/pause (pause runner and create session snapshot)
		if pauseParts := strings.SplitN(suffix, "/pause", 2); len(pauseParts) == 2 && pauseParts[1] == "" {
			runnerID := pauseParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			handlePauseRunner(w, r, mgr, log, runnerID)
			return
		}

		// Handle /api/v1/runners/{id}/connect (extend TTL or resume)
		if connectParts := strings.SplitN(suffix, "/connect", 2); len(connectParts) == 2 && connectParts[1] == "" {
			runnerID := connectParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			handleConnectRunner(w, r, mgr, log, runnerID)
			return
		}

		// Split into runnerID and the rest
		parts := strings.SplitN(suffix, "/proxy/", 2)
		if len(parts) != 2 {
			http.Error(w, "Invalid URL: expected /api/v1/runners/{id}/proxy/{path}", http.StatusBadRequest)
			return
		}

		runnerID := parts[0]
		proxyPath := "/" + parts[1]

		// Look up runner
		rn, err := mgr.GetRunner(runnerID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Runner not found: %s", runnerID), http.StatusNotFound)
			return
		}

		if rn.InternalIP == nil {
			http.Error(w, "Runner has no internal IP", http.StatusServiceUnavailable)
			return
		}

		// Build target URL - use user's service port if configured, otherwise thaw-agent health port
		servicePort := snapshot.ThawAgentHealthPort
		if rn.ServicePort > 0 {
			servicePort = rn.ServicePort
		}
		target, err := url.Parse(fmt.Sprintf("http://%s:%d", rn.InternalIP.String(), servicePort))
		if err != nil {
			http.Error(w, "Invalid target URL", http.StatusInternalServerError)
			return
		}

		log.WithFields(logrus.Fields{
			"runner_id": runnerID,
			"target":    target.String(),
			"path":      proxyPath,
			"method":    r.Method,
		}).Debug("Proxying request to microVM")

		// Create reverse proxy
		proxy := httputil.NewSingleHostReverseProxy(target)

		// Rewrite the request path to the proxied path
		r.URL.Path = proxyPath
		r.Host = target.Host

		// For streaming responses (SSE), disable buffering
		proxy.FlushInterval = -1 // Flush immediately

		proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
			log.WithError(err).WithField("runner_id", runnerID).Warn("Proxy error")
			rw.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(rw, "Proxy error: %v", err)
		}

		proxy.ServeHTTP(w, r)
	}
}

// handleExecCommand proxies a POST /exec request to a runner's thaw-agent,
// streaming the ndjson response back to the client line-by-line.
func handleExecCommand(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Look up runner
	rn, err := mgr.GetRunner(runnerID)
	if err != nil {
		http.Error(w, fmt.Sprintf("runner not found: %s", runnerID), http.StatusNotFound)
		return
	}
	if rn.InternalIP == nil {
		http.Error(w, "runner has no internal IP", http.StatusServiceUnavailable)
		return
	}
	if rn.State == runner.StateQuarantined || rn.State == runner.StateTerminated {
		http.Error(w, fmt.Sprintf("runner is %s", rn.State), http.StatusConflict)
		return
	}

	// Build target URL to thaw-agent's /exec on debug port
	targetURL := fmt.Sprintf("http://%s:%d/exec", rn.InternalIP.String(), snapshot.ThawAgentDebugPort)

	log.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"target":    targetURL,
	}).Debug("Proxying exec request to thaw-agent")

	// Track active execs for TTL enforcement
	mgr.IncrementActiveExecs(runnerID)
	defer mgr.DecrementActiveExecs(runnerID)

	// Forward the request to thaw-agent
	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, r.Body)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))

	client := &http.Client{Timeout: 0} // no client-side timeout, thaw-agent handles it
	resp, err := client.Do(upstreamReq)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Warn("Failed to reach thaw-agent for exec")
		http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy headers and set streaming headers
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	// Stream response body line-by-line
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		w.Write(scanner.Bytes())
		w.Write([]byte("\n"))
		flusher.Flush()
	}
}

// handleServiceLogs proxies GET /runners/{id}/service-logs to the thaw-agent's
// /service-logs endpoint on the debug port inside the VM.
func handleServiceLogs(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID, querySuffix string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rn, err := mgr.GetRunner(runnerID)
	if err != nil {
		http.Error(w, fmt.Sprintf("runner not found: %s", runnerID), http.StatusNotFound)
		return
	}
	if rn.InternalIP == nil {
		http.Error(w, "runner has no internal IP", http.StatusServiceUnavailable)
		return
	}

	// Build target URL to thaw-agent's /service-logs on debug port
	targetURL := fmt.Sprintf("http://%s:%d/service-logs", rn.InternalIP.String(), snapshot.ThawAgentDebugPort)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	log.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"target":    targetURL,
	}).Debug("Proxying service-logs request to thaw-agent")

	upstreamReq, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	client := &http.Client{Timeout: 0} // no timeout for streaming
	resp, err := client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy headers and status
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream response body
	if flusher, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flusher.Flush()
			}
			if readErr != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}

// waitForDataMount blocks until the parent mount point of workspaceDir (e.g.
// /mnt/data) has a real filesystem mounted on it. This prevents the manager
// from creating files on the root filesystem that get hidden when the data
// disk is later mounted over /mnt/data by the startup script.
func waitForDataMount(log *logrus.Entry, workspaceDir string) {
	// Walk up from workspaceDir to find the /mnt/data mount point.
	// workspaceDir is typically /mnt/data/workspaces.
	mountPoint := filepath.Dir(workspaceDir) // e.g. /mnt/data

	const (
		pollInterval = 2 * time.Second
		maxWait      = 120 * time.Second
	)

	start := time.Now()
	for {
		if isMounted(mountPoint) {
			log.WithFields(logrus.Fields{
				"mount_point": mountPoint,
				"waited_ms":   time.Since(start).Milliseconds(),
			}).Info("Data mount ready")
			return
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			log.WithFields(logrus.Fields{
				"mount_point": mountPoint,
				"waited":      elapsed.String(),
			}).Fatal("Data mount not ready after timeout — startup script may have failed")
		}

		log.WithField("mount_point", mountPoint).Warn("Data mount not ready, waiting...")
		time.Sleep(pollInterval)
	}
}

// isMounted checks whether the given path is a mount point by scanning /proc/mounts.
func isMounted(target string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == target {
			return true
		}
	}
	return false
}

// handlePauseRunner handles POST /api/v1/runners/{id}/pause
func handlePauseRunner(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	result, err := mgr.PauseRunner(r.Context(), runnerID)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Error("Failed to pause runner")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"session_id":          result.SessionID,
		"layer":               result.Layer,
		"snapshot_size_bytes": result.SnapshotSizeBytes,
	})
}

// handleConnectRunner handles POST /api/v1/runners/{id}/connect
// If running: extends TTL (200). If suspended: resumes (201).
func handleConnectRunner(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rn, err := mgr.GetRunner(runnerID)
	if err != nil {
		http.Error(w, fmt.Sprintf("runner not found: %s", runnerID), http.StatusNotFound)
		return
	}

	switch rn.State {
	case runner.StateIdle, runner.StateBusy:
		// Runner is active — reset TTL timer
		mgr.ResetTTL(runnerID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "connected",
			"runner_id": runnerID,
		})

	case runner.StateSuspended:
		// Runner is suspended — resume from session
		if rn.SessionID == "" {
			http.Error(w, "runner has no session_id", http.StatusBadRequest)
			return
		}
		resumed, err := mgr.ResumeFromSession(r.Context(), rn.SessionID, rn.WorkloadKey)
		if err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Error("Failed to resume runner")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "resumed",
			"runner_id": resumed.ID,
		})

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error":  fmt.Sprintf("runner is in state %s, cannot connect", rn.State),
			"status": string(rn.State),
		})
	}
}
