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
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	pb "github.com/rahul-roy-glean/capsule/api/proto/runner"
	fcrotel "github.com/rahul-roy-glean/capsule/pkg/otel"
	"github.com/rahul-roy-glean/capsule/pkg/runner"
	"github.com/rahul-roy-glean/capsule/pkg/snapshot"
)

var (
	grpcPort           = flag.Int("grpc-port", 50051, "gRPC server port")
	httpPort           = flag.Int("http-port", 8080, "HTTP server port (health/metrics)")
	idleTarget         = flag.Int("idle-target", 2, "Target number of idle runners")
	firecrackerBin     = flag.String("firecracker-bin", "/usr/local/bin/firecracker", "Path to firecracker binary")
	socketDir          = flag.String("socket-dir", "/var/run/firecracker", "Directory for VM sockets")
	workspaceDir       = flag.String("workspace-dir", "/mnt/data/workspaces", "Directory for workspaces")
	logDir             = flag.String("log-dir", "/var/log/firecracker", "Directory for VM logs")
	snapshotBucket     = flag.String("snapshot-bucket", "", "GCS bucket for snapshots")
	snapshotCache      = flag.String("snapshot-cache", "/mnt/data/snapshots", "Local snapshot cache path")
	quarantineDir      = flag.String("quarantine-dir", "/mnt/data/quarantine", "Directory to store quarantined runner manifests and debug metadata")
	microVMSubnet      = flag.String("microvm-subnet", "172.16.0.0/24", "Subnet for microVMs")
	extInterface       = flag.String("ext-interface", "auto", "External network interface (or \"auto\" to detect from the default route)")
	bridgeName         = flag.String("bridge-name", "fcbr0", "Bridge name for microVMs")
	environment        = flag.String("environment", "dev", "Environment name")
	controlPlane       = flag.String("control-plane", "", "Control plane address")
	hostBootstrapToken = flag.String("host-bootstrap-token", "", "Bearer token for authenticated host heartbeats")
	logLevel           = flag.String("log-level", "info", "Log level (debug, info, warn, error)")

	gcpProject = flag.String("gcp-project", "", "GCP project")

	// Chunked snapshot flags (BuildBuddy-style lazy loading)
	chunkCacheSizeGB = flag.Int("chunk-cache-size-gb", 2, "Size in GB of disk chunk LRU cache (FUSE)")
	memCacheSizeGB   = flag.Int("mem-cache-size-gb", 2, "Size in GB of memory chunk LRU cache (UFFD)")
	memBackend       = flag.String("mem-backend", "chunked", "Memory restore backend: 'chunked' (UFFD lazy loading, default) or 'file' (download full snapshot.mem at startup). Overrides the backend recorded in snapshot metadata.")
	gcsPrefix        = flag.String("gcs-prefix", "v1", "Top-level prefix for all GCS paths (e.g. 'v1'). Set to empty string to disable.")
)

type managerEndpointMetrics struct {
	requests     metric.Int64Counter
	requestDur   metric.Float64Histogram
	requestSize  metric.Float64Histogram
	responseSize metric.Float64Histogram
	inflight     metric.Int64UpDownCounter
}

type managerLifecycleMetrics struct {
	vmAllocCounter       metric.Int64Counter
	vmReadyHist          metric.Float64Histogram
	hostHeartbeatHist    metric.Float64Histogram
	hostHeartbeatCounter metric.Int64Counter
	sessionPauseHist     metric.Float64Histogram
	sessionPauseCounter  metric.Int64Counter
	sessionResumeHist    metric.Float64Histogram
	sessionResumeCounter metric.Int64Counter
}

func newManagerEndpointMetrics(meter metric.Meter) *managerEndpointMetrics {
	requests, _ := fcrotel.NewCounter(meter, fcrotel.ManagerEndpointRequests)
	requestDur, _ := fcrotel.NewHistogram(meter, fcrotel.ManagerEndpointRequestDuration)
	requestSize, _ := fcrotel.NewHistogram(meter, fcrotel.ManagerEndpointRequestSize)
	responseSize, _ := fcrotel.NewHistogram(meter, fcrotel.ManagerEndpointResponseSize)
	inflight, _ := fcrotel.NewUpDownCounter(meter, fcrotel.ManagerEndpointRequestsInFlight)
	return &managerEndpointMetrics{
		requests:     requests,
		requestDur:   requestDur,
		requestSize:  requestSize,
		responseSize: responseSize,
		inflight:     inflight,
	}
}

func instrumentManagerEndpoint(route, operation string, metrics *managerEndpointMetrics, otelClient *fcrotel.Client, next http.Handler) http.Handler {
	handler := otelhttp.WithRouteTag(route, otelhttp.NewHandler(
		next,
		operation,
		otelhttp.WithTracerProvider(otelClient.TracerProvider),
		otelhttp.WithMeterProvider(otelClient.MeterProvider),
	))
	handler = managerEndpointMetricsMiddleware(route, metrics, handler)
	return managerRouteMetadataMiddleware(route, operation, handler)
}

func managerRouteMetadataMiddleware(route, operation string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recorder, ok := w.(interface{ SetRouteMetadata(string, string) }); ok {
			recorder.SetRouteMetadata(route, operation)
		}
		next.ServeHTTP(w, r)
	})
}

func setManagerRoute(w http.ResponseWriter, route, operation string) {
	if recorder, ok := w.(interface{ SetRouteMetadata(string, string) }); ok {
		recorder.SetRouteMetadata(route, operation)
	}
}

func managerEndpointMetricsMiddleware(defaultRoute string, metrics *managerEndpointMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()
		requestSize := requestContentLength(r)
		metrics.inflight.Add(r.Context(), 1, metric.WithAttributes(
			fcrotel.AttrRoute.String(defaultRoute),
			fcrotel.AttrMethod.String(r.Method),
		))
		defer metrics.inflight.Add(r.Context(), -1, metric.WithAttributes(
			fcrotel.AttrRoute.String(defaultRoute),
			fcrotel.AttrMethod.String(r.Method),
		))

		next.ServeHTTP(rec, r)

		route := rec.route
		if route == "" {
			route = defaultRoute
		}
		attrs := []attribute.KeyValue{
			fcrotel.AttrRoute.String(route),
			fcrotel.AttrMethod.String(r.Method),
			fcrotel.AttrStatusCode.String(fmt.Sprintf("%d", rec.statusCode)),
			fcrotel.AttrStatusClass.String(httpStatusClass(rec.statusCode)),
		}
		metrics.requests.Add(r.Context(), 1, metric.WithAttributes(attrs...))
		metrics.requestDur.Record(r.Context(), time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		metrics.requestSize.Record(r.Context(), float64(requestSize), metric.WithAttributes(attrs...))
		metrics.responseSize.Record(r.Context(), float64(rec.bytesWritten), metric.WithAttributes(attrs...))
	})
}

func requestContentLength(r *http.Request) int64 {
	if r.ContentLength < 0 {
		return 0
	}
	return r.ContentLength
}

func httpStatusClass(code int) string {
	return fmt.Sprintf("%dxx", code/100)
}

func recordSessionPauseMetrics(ctx context.Context, metrics managerLifecycleMetrics, duration time.Duration, result, source string) {
	attrs := []attribute.KeyValue{
		fcrotel.AttrResult.String(result),
		fcrotel.AttrSource.String(source),
	}
	if result == fcrotel.ResultSuccess && metrics.sessionPauseHist != nil {
		metrics.sessionPauseHist.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	}
	if metrics.sessionPauseCounter != nil {
		metrics.sessionPauseCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func sessionResumeRouting(mgr *runner.Manager, sessionID string) string {
	meta, _ := mgr.GetSessionMetadata(sessionID)
	if meta != nil && meta.GCSManifestPath != "" {
		return fcrotel.RoutingGCS
	}
	return fcrotel.RoutingLocal
}

func recordSessionResumeMetrics(ctx context.Context, mgr *runner.Manager, metrics managerLifecycleMetrics, sessionID string, duration time.Duration, result, source string) {
	attrs := []attribute.KeyValue{
		fcrotel.AttrResult.String(result),
		fcrotel.AttrSource.String(source),
	}
	if result == fcrotel.ResultSuccess {
		attrs = append(attrs, fcrotel.AttrRouting.String(sessionResumeRouting(mgr, sessionID)))
		if metrics.sessionResumeHist != nil {
			metrics.sessionResumeHist.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
		}
	}
	if metrics.sessionResumeCounter != nil {
		metrics.sessionResumeCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func recordAllocationMetrics(ctx context.Context, metrics managerLifecycleMetrics, duration time.Duration, result, source string) {
	attrs := metric.WithAttributes(
		fcrotel.AttrResult.String(result),
		fcrotel.AttrSource.String(source),
	)
	if metrics.vmAllocCounter != nil {
		metrics.vmAllocCounter.Add(ctx, 1, attrs)
	}
	if result == fcrotel.ResultSuccess && metrics.vmReadyHist != nil {
		metrics.vmReadyHist.Record(ctx, duration.Seconds(), attrs)
	}
}

func recordHeartbeatMetrics(ctx context.Context, metrics managerLifecycleMetrics, result, source string, statusCode int, reason string) {
	if metrics.hostHeartbeatCounter == nil {
		return
	}
	attrs := []attribute.KeyValue{
		fcrotel.AttrResult.String(result),
		fcrotel.AttrSource.String(source),
	}
	if statusCode > 0 {
		attrs = append(attrs, fcrotel.AttrStatusCode.String(fmt.Sprintf("%d", statusCode)))
	}
	if reason != "" {
		attrs = append(attrs, fcrotel.AttrReason.String(reason))
	}
	metrics.hostHeartbeatCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// resumeGates prevents thundering-herd on concurrent auto-resume for the same runner.
var resumeGates sync.Map // runnerID -> *singleflight.Group

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

	log := logger.WithField("component", "capsule-manager")
	log.Info("Starting capsule-manager")

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
	if *hostBootstrapToken == "" {
		if v := os.Getenv("HOST_BOOTSTRAP_TOKEN"); v != "" {
			*hostBootstrapToken = v
		} else {
			*hostBootstrapToken = getMetadataAttribute("host-bootstrap-token")
		}
	}

	// Get GCP project
	gcpProjectVal := *gcpProject
	if gcpProjectVal == "" {
		gcpProjectVal = getMetadataAttribute("gcp-project")
		if gcpProjectVal == "" {
			// Try to get from project metadata
			gcpProjectVal = getProjectMetadata()
		}
	}

	// Create runner manager config
	cfg := runner.HostConfig{
		HostID:            hostID,
		InstanceName:      instanceName,
		Zone:              zone,
		IdleTarget:        *idleTarget,
		FirecrackerBin:    *firecrackerBin,
		SocketDir:         *socketDir,
		WorkspaceDir:      *workspaceDir,
		LogDir:            *logDir,
		SnapshotBucket:    *snapshotBucket,
		SnapshotCachePath: *snapshotCache,
		QuarantineDir:     *quarantineDir,
		Environment:       *environment,
		ControlPlaneAddr:  *controlPlane,
		GCPProject:        gcpProjectVal,
	}

	// Enable cloud-backed session chunks using the snapshot bucket.
	cfg.SessionChunkBucket = *snapshotBucket
	cfg.GCSPrefix = *gcsPrefix

	// Detect host resources for bin-packing scheduler
	cfg.TotalCPUMillicores, cfg.TotalMemoryMB = detectHostResources(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create runner manager with chunked snapshot support
	log.WithFields(logrus.Fields{
		"disk_cache_gb": *chunkCacheSizeGB,
		"mem_cache_gb":  *memCacheSizeGB,
	}).Info("Creating chunked manager with BuildBuddy-style optimizations")

	chunkedCfg := runner.ChunkedManagerConfig{
		HostConfig:          cfg,
		UseChunkedSnapshots: true,
		MicroVMSubnet:       *microVMSubnet,
		ExternalInterface:   *extInterface,
		BridgeName:          *bridgeName,
		ChunkCacheSizeBytes: int64(*chunkCacheSizeGB) * 1024 * 1024 * 1024,
		MemCacheSizeBytes:   int64(*memCacheSizeGB) * 1024 * 1024 * 1024,
		MemBackend:          *memBackend,
		GCSPrefix:           *gcsPrefix,
	}

	chunkedMgr, err := runner.NewChunkedManager(ctx, chunkedCfg, logger)
	if err != nil {
		log.WithError(err).Fatal("Failed to create chunked runner manager")
	}
	defer chunkedMgr.Close()
	mgr := chunkedMgr.Manager // Use embedded manager for compatibility

	// In chunked mode, no downloads happen at startup. The kernel,
	// snapshot.mem (file mode), and manifest are all fetched on demand
	// when the first SyncManifest (heartbeat) or AllocateRunner arrives.
	log.Info("Chunked mode: deferring all downloads until first manifest sync")

	// Reconcile orphaned resources from previous incarnation
	go mgr.ReconcileOrphans(ctx)

	// Initialize OpenTelemetry
	otelCfg := fcrotel.ConfigFromEnv("capsule-manager")
	otelClient, otelErr := fcrotel.Init(ctx, otelCfg)
	if otelErr != nil {
		log.WithError(otelErr).Warn("Failed to initialize OpenTelemetry, continuing without telemetry")
		otelClient, _ = fcrotel.Init(ctx, fcrotel.Config{ServiceName: "capsule-manager"})
	}
	defer otelClient.Shutdown(ctx)

	logger.AddHook(&fcrotel.TraceCorrelationHook{})

	// Create OTel instruments
	meter := otelClient.Meter("capsule-manager")

	// Host metrics
	hostCPUTotalGauge, _ := fcrotel.NewGauge(meter, fcrotel.HostCPUTotal)
	hostCPUUsedGauge, _ := fcrotel.NewGauge(meter, fcrotel.HostCPUUsed)
	hostMemTotalGauge, _ := fcrotel.NewGauge(meter, fcrotel.HostMemTotal)
	hostMemUsedGauge, _ := fcrotel.NewGauge(meter, fcrotel.HostMemUsed)
	hostRunnersIdleGauge, _ := fcrotel.NewUpDownCounter(meter, fcrotel.HostRunnersIdle)
	hostRunnersBusyGauge, _ := fcrotel.NewUpDownCounter(meter, fcrotel.HostRunnersBusy)

	// VM metrics
	vmAllocCounter, _ := fcrotel.NewCounter(meter, fcrotel.VMAllocations)
	vmBootHist, _ := fcrotel.NewHistogram(meter, fcrotel.VMBootDuration)
	vmReadyHist, _ := fcrotel.NewHistogram(meter, fcrotel.VMReadyDuration)

	// Chunked metrics (gauges for absolute values reported each iteration)
	diskCacheSizeGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedDiskCacheSize)
	diskCacheMaxGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedDiskCacheMax)
	diskCacheItemsGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedDiskCacheItems)
	memCacheSizeGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedMemCacheSize)
	memCacheMaxGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedMemCacheMax)
	memCacheItemsGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedMemCacheItems)
	dirtyChunksGauge, _ := fcrotel.NewGauge(meter, fcrotel.ChunkedDirtyChunks)
	cacheHitRatioGauge, _ := fcrotel.NewFloat64Gauge(meter, fcrotel.ChunkedCacheHitRatio)
	// Cumulative counters reported as absolute values from GetChunkedStats - use gauges
	pageFaultsGauge, _ := meter.Int64Gauge(string(fcrotel.ChunkedPageFaults))
	cacheHitsGauge, _ := meter.Int64Gauge(string(fcrotel.ChunkedCacheHits))
	cacheMissesGauge, _ := meter.Int64Gauge(string(fcrotel.ChunkedCacheMisses))
	chunkFetchesGauge, _ := meter.Int64Gauge(string(fcrotel.ChunkedChunkFetches))
	diskReadsGauge, _ := meter.Int64Gauge(string(fcrotel.ChunkedDiskReads))
	diskWritesGauge, _ := meter.Int64Gauge(string(fcrotel.ChunkedDiskWrites))

	// Heartbeat
	hbLatencyHist, _ := fcrotel.NewHistogram(meter, fcrotel.HostHeartbeatLatency)
	hbTotalCounter, _ := fcrotel.NewCounter(meter, fcrotel.HostHeartbeatTotal)

	// Session metrics for server.go
	sessionPauseHist, _ := fcrotel.NewHistogram(meter, fcrotel.SessionPauseDuration)
	sessionPauseCounter, _ := fcrotel.NewCounter(meter, fcrotel.SessionPauseTotal)
	sessionResumeHist, _ := fcrotel.NewHistogram(meter, fcrotel.SessionResumeDuration)
	sessionResumeCounter, _ := fcrotel.NewCounter(meter, fcrotel.SessionResumeTotal)
	endpointMetrics := newManagerEndpointMetrics(meter)
	lifecycleMetrics := managerLifecycleMetrics{
		vmAllocCounter:       vmAllocCounter,
		vmReadyHist:          vmReadyHist,
		hostHeartbeatHist:    hbLatencyHist,
		hostHeartbeatCounter: hbTotalCounter,
		sessionPauseHist:     sessionPauseHist,
		sessionPauseCounter:  sessionPauseCounter,
		sessionResumeHist:    sessionResumeHist,
		sessionResumeCounter: sessionResumeCounter,
	}

	// Create gRPC server
	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(otelClient.TracerProvider),
			otelgrpc.WithMeterProvider(otelClient.MeterProvider),
		)),
		grpc.UnaryInterceptor(loggingInterceptor(logger)),
	)

	// Register services
	hostAgentServer := NewHostAgentServer(mgr, chunkedMgr, logger)
	pb.RegisterHostAgentServer(grpcServer, hostAgentServer)
	hostAgentServer.SetOTelInstruments(lifecycleMetrics)

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
	instrumentHTTPHandler := func(route, operation string, handler http.Handler) http.Handler {
		return instrumentManagerEndpoint(route, operation, endpointMetrics, otelClient, handler)
	}
	httpMux.Handle("/health", instrumentHTTPHandler("/health", "capsule_manager.health", healthHandler(mgr)))
	httpMux.Handle("/ready", instrumentHTTPHandler("/ready", "capsule_manager.ready", readyHandler(mgr)))
	httpMux.Handle("/api/v1/runners/quarantine", instrumentHTTPHandler("/api/v1/runners/quarantine", "capsule_manager.quarantine_runner", drainingGuard(mgr, quarantineRunnerHandler(mgr, logger))))
	httpMux.Handle("/api/v1/runners/unquarantine", instrumentHTTPHandler("/api/v1/runners/unquarantine", "capsule_manager.unquarantine_runner", drainingGuard(mgr, unquarantineRunnerHandler(mgr, logger))))
	httpMux.Handle("/api/v1/runners/network-policy", instrumentHTTPHandler("/api/v1/runners/network-policy", "capsule_manager.network_policy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getNetworkPolicyHandler(mgr, logger)(w, r)
		case http.MethodPost:
			updateNetworkPolicyHandler(mgr, logger)(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})))
	httpMux.Handle("/api/v1/gc", instrumentHTTPHandler("/api/v1/gc", "capsule_manager.gc", gcHandler(mgr, logger)))
	httpMux.Handle("/api/v1/runners/", instrumentHTTPHandler("/api/v1/runners/*", "capsule_manager.runners", drainingGuard(mgr, runnerProxyHandler(mgr, logger, lifecycleMetrics))))

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", *httpPort),
		Handler: managerAPILoggingMiddleware(logger, httpMux),
	}

	go func() {
		log.WithField("port", *httpPort).Info("Starting HTTP server")
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.WithError(err).Error("HTTP server error")
		}
	}()

	// Start autoscaler loop
	go autoscaleLoop(ctx, mgr, chunkedMgr, *idleTarget, logger, autoscaleInstruments{
		hostAttrs:       metric.WithAttributes(fcrotel.AttrHostID.String(instanceName)),
		vmAllocCounter:  vmAllocCounter,
		vmBootHist:      vmBootHist,
		hostCPUTotal:    hostCPUTotalGauge,
		hostCPUUsed:     hostCPUUsedGauge,
		hostMemTotal:    hostMemTotalGauge,
		hostMemUsed:     hostMemUsedGauge,
		hostRunnersIdle: hostRunnersIdleGauge,
		hostRunnersBusy: hostRunnersBusyGauge,
		diskCacheSize:   diskCacheSizeGauge,
		diskCacheMax:    diskCacheMaxGauge,
		diskCacheItems:  diskCacheItemsGauge,
		memCacheSize:    memCacheSizeGauge,
		memCacheMax:     memCacheMaxGauge,
		memCacheItems:   memCacheItemsGauge,
		pageFaults:      pageFaultsGauge,
		cacheHits:       cacheHitsGauge,
		cacheMisses:     cacheMissesGauge,
		chunkFetches:    chunkFetchesGauge,
		diskReads:       diskReadsGauge,
		diskWrites:      diskWritesGauge,
		dirtyChunks:     dirtyChunksGauge,
		cacheHitRatio:   cacheHitRatioGauge,
	})

	// Start heartbeat loop if control plane is configured
	if *controlPlane != "" {
		go heartbeatLoop(ctx, mgr, chunkedMgr, *controlPlane, *hostBootstrapToken, instanceName, zone, *grpcPort, *httpPort, logger, lifecycleMetrics)
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
		sendDrainHeartbeat(mgr, chunkedMgr, *controlPlane, *hostBootstrapToken, instanceName, zone, *grpcPort, *httpPort, logger, lifecycleMetrics)
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
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Ready: %d runners", status.ActiveRunners)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
	route        string
	operation    string
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytesWritten += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) SetRouteMetadata(route, operation string) {
	r.route = route
	r.operation = operation
	if recorder, ok := r.ResponseWriter.(interface{ SetRouteMetadata(string, string) }); ok {
		recorder.SetRouteMetadata(route, operation)
	}
}

func managerAPILoggingMiddleware(logger *logrus.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == "/ready" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		duration := time.Since(start)

		fields := logrus.Fields{
			"method":         r.Method,
			"path":           r.URL.Path,
			"status":         rec.statusCode,
			"status_class":   httpStatusClass(rec.statusCode),
			"duration_ms":    duration.Milliseconds(),
			"remote_addr":    r.RemoteAddr,
			"response_bytes": rec.bytesWritten,
		}
		if rec.route != "" {
			fields["route"] = rec.route
		}
		if rec.operation != "" {
			fields["operation"] = rec.operation
		}
		if r.ContentLength >= 0 {
			fields["request_bytes"] = r.ContentLength
		}

		entry := logger.WithFields(fields)
		if rec.statusCode >= 500 {
			entry.Error("Manager HTTP request completed with server error")
		} else if rec.statusCode >= 400 {
			entry.Warn("Manager HTTP request completed with client error")
		} else {
			entry.Info("Manager HTTP request completed")
		}
	})
}

// autoscaleInstruments holds OTel instruments used by the autoscale loop.
type autoscaleInstruments struct {
	hostAttrs       metric.MeasurementOption // host_id attribute for all recordings
	vmAllocCounter  metric.Int64Counter
	vmBootHist      metric.Float64Histogram
	hostCPUTotal    metric.Int64Gauge
	hostCPUUsed     metric.Int64Gauge
	hostMemTotal    metric.Int64Gauge
	hostMemUsed     metric.Int64Gauge
	hostRunnersIdle metric.Int64UpDownCounter
	hostRunnersBusy metric.Int64UpDownCounter
	diskCacheSize   metric.Int64Gauge
	diskCacheMax    metric.Int64Gauge
	diskCacheItems  metric.Int64Gauge
	memCacheSize    metric.Int64Gauge
	memCacheMax     metric.Int64Gauge
	memCacheItems   metric.Int64Gauge
	pageFaults      metric.Int64Gauge
	cacheHits       metric.Int64Gauge
	cacheMisses     metric.Int64Gauge
	chunkFetches    metric.Int64Gauge
	diskReads       metric.Int64Gauge
	diskWrites      metric.Int64Gauge
	dirtyChunks     metric.Int64Gauge
	cacheHitRatio   metric.Float64Gauge
}

func autoscaleLoop(ctx context.Context, mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, idleTarget int, logger *logrus.Logger, instruments autoscaleInstruments) {
	_ = idleTarget // reserved for future use
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// Track previous values of UpDownCounters to compute deltas
	var prevIdle, prevBusy int

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := mgr.GetStatus()

			// Chunked mode: no local pre-allocation; control plane drives it.
			// Each workload key needs a different snapshot, so generic warm VMs
			// would load the wrong data and be useless when an actual job arrives.

			// Record host metrics
			ha := instruments.hostAttrs
			instruments.hostCPUTotal.Record(ctx, int64(status.TotalCPUMillicores), ha)
			instruments.hostCPUUsed.Record(ctx, int64(status.UsedCPUMillicores), ha)
			instruments.hostMemTotal.Record(ctx, int64(status.TotalMemoryMB), ha)
			instruments.hostMemUsed.Record(ctx, int64(status.UsedMemoryMB), ha)

			// UpDownCounters need delta from previous value
			idleDelta := int64(status.IdleRunners - prevIdle)
			busyDelta := int64(status.BusyRunners - prevBusy)
			instruments.hostRunnersIdle.Add(ctx, idleDelta, ha)
			instruments.hostRunnersBusy.Add(ctx, busyDelta, ha)
			prevIdle = status.IdleRunners
			prevBusy = status.BusyRunners

			// Record chunked snapshot metrics
			cs := chunkedMgr.GetChunkedStats()
			instruments.diskCacheSize.Record(ctx, cs.DiskCacheSize, ha)
			instruments.diskCacheMax.Record(ctx, cs.DiskCacheMaxSize, ha)
			instruments.diskCacheItems.Record(ctx, int64(cs.DiskCacheItems), ha)
			instruments.memCacheSize.Record(ctx, cs.MemCacheSize, ha)
			instruments.memCacheMax.Record(ctx, cs.MemCacheMaxSize, ha)
			instruments.memCacheItems.Record(ctx, int64(cs.MemCacheItems), ha)
			instruments.pageFaults.Record(ctx, int64(cs.TotalPageFaults), ha)
			instruments.cacheHits.Record(ctx, int64(cs.MemCacheHits), ha)
			instruments.cacheMisses.Record(ctx, int64(cs.MemCacheMisses), ha)
			instruments.chunkFetches.Record(ctx, int64(cs.TotalChunkFetches), ha)
			instruments.diskReads.Record(ctx, int64(cs.TotalDiskReads), ha)
			instruments.diskWrites.Record(ctx, int64(cs.TotalDiskWrites), ha)
			instruments.dirtyChunks.Record(ctx, int64(cs.TotalDirtyChunks), ha)
			// Compute cache hit ratio from the ChunkStore LRU stats, which
			// persist across handler lifetimes (handler-level CacheHits is
			// always 0 since page-level caching was removed).
			if totalLookups := cs.MemCacheHits + cs.MemCacheMisses; totalLookups > 0 {
				instruments.cacheHitRatio.Record(ctx, float64(cs.MemCacheHits)/float64(totalLookups), ha)
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
	Acknowledged bool              `json:"acknowledged"`
	ShouldDrain  bool              `json:"should_drain"`
	SyncVersions map[string]string `json:"sync_versions,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func heartbeatLoop(ctx context.Context, mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, controlPlane, hostBootstrapToken, instanceName, zone string, grpcPort, httpPort int, logger *logrus.Logger, lifecycleMetrics managerLifecycleMetrics) {
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
			hbStart := time.Now()
			status := mgr.GetStatus()
			reqBody := hostHeartbeatRequest{
				InstanceName:       instanceName,
				Zone:               zone,
				GRPCAddress:        fmt.Sprintf("%s:%d", internalIP, grpcPort),
				HTTPAddress:        fmt.Sprintf("%s:%d", internalIP, httpPort),
				IdleRunners:        status.IdleRunners,
				BusyRunners:        status.BusyRunners,
				Draining:           status.Draining,
				DiskUsage:          mgr.DiskUsage(),
				TotalCPUMillicores: status.TotalCPUMillicores,
				UsedCPUMillicores:  status.UsedCPUMillicores,
				TotalMemoryMB:      status.TotalMemoryMB,
				UsedMemoryMB:       status.UsedMemoryMB,
			}
			reqBody.LoadedManifests = chunkedMgr.GetLoadedManifests()
			reqBody.Runners = mgr.GetRunnerHeartbeatInfo()

			b, _ := json.Marshal(reqBody)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, heartbeatURL, bytes.NewReader(b))
			if err != nil {
				log.WithError(err).Warn("Failed to create heartbeat request")
				recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultError, "periodic", 0, "request_create")
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			if hostBootstrapToken != "" {
				req.Header.Set("Authorization", "Bearer "+hostBootstrapToken)
			}
			resp, err := client.Do(req)
			if err != nil {
				log.WithError(err).Warn("Heartbeat request failed")
				recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultFailure, "periodic", 0, "request_send")
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Record heartbeat latency after a response is received.
			if lifecycleMetrics.hostHeartbeatHist != nil {
				lifecycleMetrics.hostHeartbeatHist.Record(ctx, time.Since(hbStart).Seconds())
			}

			if resp.StatusCode >= 400 {
				log.WithFields(logrus.Fields{
					"status": resp.StatusCode,
					"body":   string(body),
				}).Warn("Heartbeat rejected by control plane")
				recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultFailure, "periodic", resp.StatusCode, "http_rejected")
				continue
			}

			var hbResp hostHeartbeatResponse
			if err := json.Unmarshal(body, &hbResp); err != nil {
				log.WithError(err).WithField("body", string(body)).Warn("Failed to parse heartbeat response")
				recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultError, "periodic", resp.StatusCode, "response_parse")
				continue
			}
			if hbResp.Error != "" {
				log.WithField("error", hbResp.Error).Warn("Control plane heartbeat error")
				recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultError, "periodic", resp.StatusCode, "control_plane_error")
				continue
			}
			recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultSuccess, "periodic", resp.StatusCode, "")

			changed := mgr.SetDraining(hbResp.ShouldDrain)
			if changed {
				if hbResp.ShouldDrain {
					wasDraining = true

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
				_, _ = mgr.DrainIdleRunners(ctx)
				_, _ = mgr.PauseSessionRunners(ctx)
			}

			// Handle per-workload-key manifest sync directives
			if len(hbResp.SyncVersions) > 0 {
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
func sendDrainHeartbeat(mgr *runner.Manager, chunkedMgr *runner.ChunkedManager, controlPlane, hostBootstrapToken, instanceName, zone string, grpcPort, httpPort int, logger *logrus.Logger, lifecycleMetrics managerLifecycleMetrics) {
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
		Draining:           true,
		DiskUsage:          mgr.DiskUsage(),
		TotalCPUMillicores: status.TotalCPUMillicores,
		UsedCPUMillicores:  status.UsedCPUMillicores,
		TotalMemoryMB:      status.TotalMemoryMB,
		UsedMemoryMB:       status.UsedMemoryMB,
	}
	reqBody.LoadedManifests = chunkedMgr.GetLoadedManifests()
	reqBody.Runners = mgr.GetRunnerHeartbeatInfo()

	b, _ := json.Marshal(reqBody)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, heartbeatURL, bytes.NewReader(b))
	if err != nil {
		log.WithError(err).Warn("Failed to create drain heartbeat request")
		recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultError, "drain", 0, "request_create")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if hostBootstrapToken != "" {
		req.Header.Set("Authorization", "Bearer "+hostBootstrapToken)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.WithError(err).Warn("Failed to send drain heartbeat")
		recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultFailure, "drain", 0, "request_send")
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultFailure, "drain", resp.StatusCode, "http_rejected")
	} else {
		recordHeartbeatMetrics(ctx, lifecycleMetrics, fcrotel.ResultSuccess, "drain", resp.StatusCode, "")
	}
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

// runnerProxyHandler reverse-proxies HTTP requests to a specific microVM's service.
//
// URL pattern: /api/v1/runners/{runnerID}/proxy/{path...}
//
// The handler looks up the runner by ID, gets its InternalIP (which is the
// host-reachable veth IP in netns mode), and proxies the request to the user's
// service port (or the capsule-thaw-agent health port if no service is configured).
//
// This allows external clients to reach services running inside microVMs
// (e.g., claude_sandbox_service) without knowing about network namespaces,
// veth IPs, or DNAT. The client just needs the host address and runner ID.
func runnerProxyHandler(mgr *runner.Manager, logger *logrus.Logger, lifecycleMetrics managerLifecycleMetrics) http.HandlerFunc {
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
			setManagerRoute(w, "/api/v1/runners/:id/token/gcp", "capsule_manager.runner_gcp_token")
			gcpTokenHandler(w, r, logger)
			return
		}

		// Handle /api/v1/runners/{id}/exec (execute command in VM)
		if execParts := strings.SplitN(suffix, "/exec", 2); len(execParts) == 2 && execParts[1] == "" {
			runnerID := execParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			setManagerRoute(w, "/api/v1/runners/:id/exec", "capsule_manager.runner_exec")
			handleExecCommand(w, r, mgr, log, runnerID, lifecycleMetrics)
			return
		}

		// Handle /api/v1/runners/{id}/pty (interactive terminal via WebSocket)
		if ptyParts := strings.SplitN(suffix, "/pty", 2); len(ptyParts) == 2 && (ptyParts[1] == "" || ptyParts[1][0] == '?') {
			runnerID := strings.TrimSuffix(ptyParts[0], "/")
			setManagerRoute(w, "/api/v1/runners/:id/pty", "capsule_manager.runner_pty")
			handlePTYProxy(w, r, mgr, log, runnerID, lifecycleMetrics)
			return
		}

		// Handle /api/v1/runners/{id}/service-logs (proxy to capsule-thaw-agent's service-logs)
		if slParts := strings.SplitN(suffix, "/service-logs", 2); len(slParts) == 2 {
			runnerID := strings.TrimSuffix(slParts[0], "/")
			setManagerRoute(w, "/api/v1/runners/:id/service-logs", "capsule_manager.runner_service_logs")
			handleServiceLogs(w, r, mgr, log, runnerID, slParts[1])
			return
		}

		// Handle /api/v1/runners/{id}/pause (pause runner and create session snapshot)
		if pauseParts := strings.SplitN(suffix, "/pause", 2); len(pauseParts) == 2 && pauseParts[1] == "" {
			runnerID := pauseParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			setManagerRoute(w, "/api/v1/runners/:id/pause", "capsule_manager.runner_pause")
			handlePauseRunner(w, r, mgr, log, runnerID, lifecycleMetrics)
			return
		}

		// Handle /api/v1/runners/{id}/checkpoint (non-destructive checkpoint, VM keeps running)
		if cpParts := strings.SplitN(suffix, "/checkpoint", 2); len(cpParts) == 2 && cpParts[1] == "" {
			runnerID := cpParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			setManagerRoute(w, "/api/v1/runners/:id/checkpoint", "capsule_manager.runner_checkpoint")
			handleCheckpointRunner(w, r, mgr, log, runnerID)
			return
		}

		// Handle /api/v1/runners/{id}/fork (checkpoint + fork session lineage)
		if forkParts := strings.SplitN(suffix, "/fork", 2); len(forkParts) == 2 && forkParts[1] == "" {
			runnerID := forkParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			setManagerRoute(w, "/api/v1/runners/:id/fork", "capsule_manager.runner_fork")
			handleForkRunner(w, r, mgr, log, runnerID)
			return
		}

		// Handle /api/v1/runners/{id}/connect (extend TTL or resume)
		if connectParts := strings.SplitN(suffix, "/connect", 2); len(connectParts) == 2 && connectParts[1] == "" {
			runnerID := connectParts[0]
			runnerID = strings.TrimSuffix(runnerID, "/")
			setManagerRoute(w, "/api/v1/runners/:id/connect", "capsule_manager.runner_connect")
			handleConnectRunner(w, r, mgr, log, runnerID, lifecycleMetrics)
			return
		}

		// Handle /api/v1/runners/{id}/files/{op} (file operations in VM)
		if filesParts := strings.SplitN(suffix, "/files/", 2); len(filesParts) == 2 {
			runnerID := strings.TrimSuffix(filesParts[0], "/")
			fileOp := filesParts[1]
			setManagerRoute(w, "/api/v1/runners/:id/files/"+fileOp, "capsule_manager.runner_files")
			handleFileOp(w, r, mgr, log, runnerID, fileOp, lifecycleMetrics)
			return
		}

		// Split into runnerID and the rest
		parts := strings.SplitN(suffix, "/proxy/", 2)
		if len(parts) != 2 {
			http.Error(w, "Invalid URL: expected /api/v1/runners/{id}/proxy/{path}", http.StatusBadRequest)
			return
		}
		setManagerRoute(w, "/api/v1/runners/:id/proxy/*", "capsule_manager.runner_proxy")

		runnerID := parts[0]
		proxyPath := "/" + parts[1]

		// Look up runner
		rn, err := mgr.GetRunner(runnerID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Runner not found: %s", runnerID), http.StatusNotFound)
			return
		}

		// Auto-resume suspended runners on proxy traffic
		if rn.State == runner.StateSuspended {
			resumed, resumeErr := autoResumeIfSuspended(r.Context(), mgr, log, runnerID, rn, lifecycleMetrics)
			if resumeErr != nil {
				log.WithError(resumeErr).WithField("runner_id", runnerID).Warn("Auto-resume failed for proxy")
				http.Error(w, "auto-resume failed: "+resumeErr.Error(), http.StatusServiceUnavailable)
				return
			}
			rn = resumed
		}

		if rn.InternalIP == nil {
			http.Error(w, "Runner has no internal IP", http.StatusServiceUnavailable)
			return
		}

		// Build target URL - use user's service port if configured, otherwise capsule-thaw-agent health port
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

// handleExecCommand proxies a POST /exec request to a runner's capsule-thaw-agent,
// streaming the ndjson response back to the client line-by-line.
func handleExecCommand(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string, lifecycleMetrics managerLifecycleMetrics) {
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

	// Auto-resume if suspended
	if rn.State == runner.StateSuspended {
		resumed, err := autoResumeIfSuspended(r.Context(), mgr, log, runnerID, rn, lifecycleMetrics)
		if err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Warn("Auto-resume failed for exec")
			http.Error(w, "auto-resume failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		rn = resumed
	}

	// Build target URL to capsule-thaw-agent's /exec on debug port
	targetURL := fmt.Sprintf("http://%s:%d/exec", rn.InternalIP.String(), snapshot.ThawAgentDebugPort)

	log.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"target":    targetURL,
	}).Debug("Proxying exec request to capsule-thaw-agent")

	// Track active execs for TTL enforcement
	if err := mgr.TryAcquireExec(runnerID); err != nil {
		http.Error(w, "runner unavailable: "+err.Error(), http.StatusConflict)
		return
	}
	defer mgr.ReleaseExec(runnerID)

	// Forward the request to capsule-thaw-agent
	upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", targetURL, r.Body)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))

	client := &http.Client{Timeout: 0} // no client-side timeout, capsule-thaw-agent handles it
	resp, err := client.Do(upstreamReq)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Warn("Failed to reach capsule-thaw-agent for exec")
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

// handlePTYProxy upgrades the client connection to WebSocket, dials the
// capsule-thaw-agent's /pty endpoint inside the VM, and pumps frames bidirectionally.
func handlePTYProxy(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string, lifecycleMetrics managerLifecycleMetrics) {
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

	// Auto-resume if suspended
	if rn.State == runner.StateSuspended {
		resumed, err := autoResumeIfSuspended(r.Context(), mgr, log, runnerID, rn, lifecycleMetrics)
		if err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Warn("Auto-resume failed for pty")
			http.Error(w, "auto-resume failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		rn = resumed
	}

	// Track active execs for TTL enforcement
	if err := mgr.TryAcquireExec(runnerID); err != nil {
		http.Error(w, "runner unavailable: "+err.Error(), http.StatusConflict)
		return
	}
	defer mgr.ReleaseExec(runnerID)

	// Build backend WebSocket URL (capsule-thaw-agent debug port)
	backendURL := fmt.Sprintf("ws://%s:%d/pty?%s", rn.InternalIP.String(), snapshot.ThawAgentDebugPort, r.URL.RawQuery)
	log.WithFields(logrus.Fields{
		"runner_id":   runnerID,
		"backend_url": backendURL,
	}).Debug("Proxying PTY WebSocket to capsule-thaw-agent")

	// Upgrade client-side to WebSocket
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.WithError(err).Warn("Client WebSocket upgrade failed")
		return
	}
	defer clientConn.Close()

	// Dial backend WebSocket to capsule-thaw-agent
	dialer := websocket.Dialer{}
	backendConn, _, err := dialer.Dial(backendURL, nil)
	if err != nil {
		log.WithError(err).Warn("Backend WebSocket dial failed")
		clientConn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "backend connect failed"))
		return
	}
	defer backendConn.Close()

	// Bidirectional frame pump
	done := make(chan struct{}, 2)

	// Client -> Backend
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := clientConn.ReadMessage()
			if err != nil {
				return
			}
			if err := backendConn.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	// Backend -> Client
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := backendConn.ReadMessage()
			if err != nil {
				return
			}
			if err := clientConn.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	// Wait for either side to disconnect
	<-done
}

// handleFileOp proxies a /files/{op} request to a runner's capsule-thaw-agent.
// For download/upload ops, it uses streaming (raw bytes, no timeout).
// For metadata ops (read/write/list/stat/remove/mkdir), it uses JSON with a 30s timeout.
func handleFileOp(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID, fileOp string, lifecycleMetrics managerLifecycleMetrics) {
	// download is GET, everything else is POST
	isDownload := fileOp == "download"
	isUpload := fileOp == "upload"
	isStreaming := isDownload || isUpload

	if isDownload {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	} else if r.Method != http.MethodPost {
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
	if rn.State == runner.StateQuarantined || rn.State == runner.StateTerminated {
		http.Error(w, fmt.Sprintf("runner is %s", rn.State), http.StatusConflict)
		return
	}

	// Auto-resume if suspended
	if rn.State == runner.StateSuspended {
		resumed, err := autoResumeIfSuspended(r.Context(), mgr, log, runnerID, rn, lifecycleMetrics)
		if err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Warn("Auto-resume failed for file op")
			http.Error(w, "auto-resume failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		rn = resumed
	}

	targetURL := fmt.Sprintf("http://%s:%d/files/%s", rn.InternalIP.String(), snapshot.ThawAgentDebugPort, fileOp)
	// Forward query string for download/upload (path, mode, perm params).
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	log.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"file_op":   fileOp,
		"target":    targetURL,
	}).Debug("Proxying file op request to capsule-thaw-agent")

	if err := mgr.TryAcquireExec(runnerID); err != nil {
		http.Error(w, "runner unavailable: "+err.Error(), http.StatusConflict)
		return
	}
	defer mgr.ReleaseExec(runnerID)

	method := r.Method
	var body io.Reader
	if !isDownload {
		body = r.Body
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), method, targetURL, body)
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	if isUpload {
		// Forward the original content type for uploads (may be application/octet-stream).
		if ct := r.Header.Get("Content-Type"); ct != "" {
			upstreamReq.Header.Set("Content-Type", ct)
		}
	} else if !isDownload {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}

	// Streaming ops get no timeout (large files). Metadata ops keep 30s.
	timeout := 30 * time.Second
	if isStreaming {
		timeout = 0
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Warn("Failed to reach capsule-thaw-agent for file op")
		http.Error(w, "bad gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy all response headers for streaming ops (Content-Type, Content-Length,
	// Content-Range, Last-Modified, etc. from http.ServeFile).
	if isStreaming {
		for key, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(key, v)
			}
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// autoResumeIfSuspended checks if a runner is suspended and auto-resumes it.
// Returns the updated runner or an error. If the runner is not suspended, it
// returns the original runner unchanged.
func autoResumeIfSuspended(ctx context.Context, mgr *runner.Manager, log *logrus.Entry, runnerID string, rn *runner.Runner, lifecycleMetrics managerLifecycleMetrics) (*runner.Runner, error) {
	if rn.State != runner.StateSuspended || rn.SessionID == "" {
		return rn, nil
	}

	log.WithFields(logrus.Fields{
		"runner_id":  runnerID,
		"session_id": rn.SessionID,
	}).Info("Auto-resuming suspended runner")

	// Use singleflight to prevent thundering-herd on concurrent requests
	val, _ := resumeGates.LoadOrStore(runnerID, &singleflight.Group{})
	group := val.(*singleflight.Group)

	result, err, _ := group.Do(runnerID, func() (interface{}, error) {
		start := time.Now()
		resumed, err := mgr.ResumeFromSession(ctx, rn.SessionID, rn.WorkloadKey)
		if err != nil {
			recordSessionResumeMetrics(ctx, mgr, lifecycleMetrics, rn.SessionID, time.Since(start), fcrotel.ResultFailure, "auto_resume")
			return nil, fmt.Errorf("auto-resume failed: %w", err)
		}

		// Wait for capsule-thaw-agent exec readiness — /alive responds before /exec
		// is fully functional after snapshot restore, so probe with a real exec.
		if err := waitForThawAgentExec(resumed.InternalIP, 30*time.Second); err != nil {
			recordSessionResumeMetrics(ctx, mgr, lifecycleMetrics, rn.SessionID, time.Since(start), fcrotel.ResultFailure, "auto_resume")
			return nil, fmt.Errorf("capsule-thaw-agent not ready after resume: %w", err)
		}
		recordSessionResumeMetrics(ctx, mgr, lifecycleMetrics, rn.SessionID, time.Since(start), fcrotel.ResultSuccess, "auto_resume")

		return resumed, nil
	})

	// Clean up the gate after resume completes
	resumeGates.Delete(runnerID)

	if err != nil {
		return nil, err
	}
	return result.(*runner.Runner), nil
}

// waitForThawAgentExec polls the capsule-thaw-agent by sending a trivial exec command
// until it responds successfully. This is more reliable than checking /alive
// after snapshot restore, because /alive can respond before the exec handler
// is fully functional.
func waitForThawAgentExec(ip net.IP, timeout time.Duration) error {
	execURL := fmt.Sprintf("http://%s:%d/exec", ip.String(), snapshot.ThawAgentDebugPort)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	body := []byte(`{"command":["echo","ready"],"timeout_seconds":3}`)

	for time.Now().Before(deadline) {
		resp, err := client.Post(execURL, "application/json", bytes.NewReader(body))
		if err == nil {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(respBody), "ready") {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("capsule-thaw-agent at %s not ready after %s", ip.String(), timeout)
}

// handleServiceLogs proxies GET /runners/{id}/service-logs to the capsule-thaw-agent's
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

	// Build target URL to capsule-thaw-agent's /service-logs on debug port
	targetURL := fmt.Sprintf("http://%s:%d/service-logs", rn.InternalIP.String(), snapshot.ThawAgentDebugPort)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	log.WithFields(logrus.Fields{
		"runner_id": runnerID,
		"target":    targetURL,
	}).Debug("Proxying service-logs request to capsule-thaw-agent")

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
func handlePauseRunner(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string, lifecycleMetrics managerLifecycleMetrics) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	result, err := mgr.PauseRunner(r.Context(), runnerID)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Error("Failed to pause runner")
		recordSessionPauseMetrics(r.Context(), lifecycleMetrics, time.Since(start), fcrotel.ResultFailure, "http")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	recordSessionPauseMetrics(r.Context(), lifecycleMetrics, time.Since(start), fcrotel.ResultSuccess, "http")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"session_id":          result.SessionID,
		"layer":               result.Layer,
		"snapshot_size_bytes": result.SnapshotSizeBytes,
	})
}

// handleCheckpointRunner handles POST /api/v1/runners/{id}/checkpoint
// Creates a non-destructive snapshot without stopping the VM.
func handleCheckpointRunner(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	result, err := mgr.CheckpointRunner(r.Context(), runnerID)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Error("Failed to checkpoint runner")
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
		"running":             result.Running,
	})
}

// handleForkRunner handles POST /api/v1/runners/{id}/fork.
// It checkpoints the live runner and materializes a forkable session lineage.
func handleForkRunner(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		RunnerID  string `json:"runner_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" || req.RunnerID == "" {
		http.Error(w, "session_id and runner_id are required", http.StatusBadRequest)
		return
	}

	meta, err := mgr.ForkRunnerSession(r.Context(), runnerID, req.SessionID, req.RunnerID)
	if err != nil {
		log.WithError(err).WithField("runner_id", runnerID).Error("Failed to fork runner session")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"session_id":        meta.SessionID,
		"runner_id":         meta.RunnerID,
		"source_session_id": meta.ParentSessionID,
		"layer":             meta.Layers,
	})
}

// handleConnectRunner handles POST /api/v1/runners/{id}/connect
// If running: extends TTL (200). If suspended: resumes (201).
func handleConnectRunner(w http.ResponseWriter, r *http.Request, mgr *runner.Manager, log *logrus.Entry, runnerID string, lifecycleMetrics managerLifecycleMetrics) {
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
		start := time.Now()
		resumed, err := mgr.ResumeFromSession(r.Context(), rn.SessionID, rn.WorkloadKey)
		if err != nil {
			log.WithError(err).WithField("runner_id", runnerID).Error("Failed to resume runner")
			recordSessionResumeMetrics(r.Context(), mgr, lifecycleMetrics, rn.SessionID, time.Since(start), fcrotel.ResultFailure, "connect_http")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		recordSessionResumeMetrics(r.Context(), mgr, lifecycleMetrics, rn.SessionID, time.Since(start), fcrotel.ResultSuccess, "connect_http")
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
