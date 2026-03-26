package otel

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Typed metric name aliases.
type CounterName string
type HistogramName string
type GaugeName string
type UpDownCounterName string
type Float64GaugeName string

// Histogram names (Float64Histogram, unit "s").
const (
	VMBootDuration                 HistogramName = "vm.boot.duration"
	VMReadyDuration                HistogramName = "vm.ready.duration"
	VMLifetime                     HistogramName = "vm.lifetime"
	VMJobDuration                  HistogramName = "vm.job.duration"
	HostBootDuration               HistogramName = "host.boot.duration"
	HostGCSSyncDuration            HistogramName = "host.gcs_sync.duration"
	HostHeartbeatLatency           HistogramName = "host.heartbeat.latency"
	ManagerEndpointRequestDuration HistogramName = "capsule_manager.endpoint.request.duration"
	ManagerEndpointRequestSize     HistogramName = "capsule_manager.endpoint.request.size"
	ManagerEndpointResponseSize    HistogramName = "capsule_manager.endpoint.response.size"
	CPWebhookLatency               HistogramName = "control_plane.webhook.latency"
	CPAllocationLatency            HistogramName = "control_plane.allocation.latency"
	CPQueueWait                    HistogramName = "control_plane.queue.wait"
	CPEndpointRequestDuration      HistogramName = "control_plane.endpoint.request.duration"
	CPEndpointRequestSize          HistogramName = "control_plane.endpoint.request.size"
	CPEndpointResponseSize         HistogramName = "control_plane.endpoint.response.size"
	SnapshotBuildDuration          HistogramName = "snapshot.build.duration"
	SnapshotUploadDuration         HistogramName = "snapshot.upload.duration"
	SessionPauseDuration           HistogramName = "session.pause.duration"
	SessionResumeDuration          HistogramName = "session.resume.duration"
	CacheGitCloneDuration          HistogramName = "cache.clone.duration"
	GitHubRegistrationDuration     HistogramName = "ci.runner_registration.duration"
	GitHubJobPickupLatency         HistogramName = "ci.job_pickup.latency"
	UFFDFaultServiceDuration       HistogramName = "uffd.fault_service.duration"
	ChunkFetchDuration             HistogramName = "chunk.fetch.duration"
)

// Counter names (Int64Counter).
const (
	VMAllocations           CounterName = "vm.allocations"
	VMTerminations          CounterName = "vm.terminations"
	HostHeartbeatTotal      CounterName = "host.heartbeat.total"
	ManagerEndpointRequests CounterName = "capsule_manager.endpoint.requests"
	CPWebhookRequests       CounterName = "control_plane.webhook.requests"
	CPAllocations           CounterName = "control_plane.allocations"
	CPPlacementSelections   CounterName = "control_plane.placement.selections"
	CPEndpointRequests      CounterName = "control_plane.endpoint.requests"
	CPDownscalerActions     CounterName = "control_plane.downscaler.actions"
	SnapshotRollouts        CounterName = "snapshot.rollouts"
	CacheArtifactHits       CounterName = "cache.artifact.hits"
	CacheArtifactMisses     CounterName = "cache.artifact.misses"
	CacheGitClones          CounterName = "cache.git_clones"
	ChunkedPageFaults       CounterName = "chunked.page_faults"
	ChunkedCacheHits        CounterName = "chunked.cache_hits"
	ChunkedCacheMisses      CounterName = "chunked.cache_misses"
	ChunkedChunkFetches     CounterName = "chunked.chunk_fetches"
	ChunkedDiskReads        CounterName = "chunked.disk_reads"
	ChunkedDiskWrites       CounterName = "chunked.disk_writes"
	PoolHits                CounterName = "pool.hits"
	PoolMisses              CounterName = "pool.misses"
	PoolEvictions           CounterName = "pool.evictions"
	PoolRecycleFailures     CounterName = "pool.recycle_failures"
	SessionPauseTotal       CounterName = "session.pause.total"
	SessionResumeTotal      CounterName = "session.resume.total"
	SessionResumeRouting    CounterName = "session.resume.routing"
	E2ECanarySuccess        CounterName = "e2e.canary.success"
	E2ECanaryFailure        CounterName = "e2e.canary.failure"
	ChunkFetchBytes         CounterName = "chunk.fetch.bytes"
	ChunkSingleflightDedup  CounterName = "chunk.singleflight.dedup"
	ChunkNegCacheHits       CounterName = "chunk.neg_cache.hits"
	NetworkBytesTx          CounterName = "network.bytes_tx"
	NetworkBytesRx          CounterName = "network.bytes_rx"
	HostGCBytesReclaimed    CounterName = "host.gc.bytes_reclaimed"
	HostGCFilesRemoved      CounterName = "host.gc.files_removed"
)

// Gauge names (Int64Gauge).
const (
	HostCPUTotal            GaugeName = "host.cpu_millicores.total"
	HostCPUUsed             GaugeName = "host.cpu_millicores.used"
	HostMemTotal            GaugeName = "host.memory_mb.total"
	HostMemUsed             GaugeName = "host.memory_mb.used"
	CPHostsTotal            GaugeName = "control_plane.hosts"
	CPHostsReady            GaugeName = "control_plane.hosts.ready"
	CPHostsDraining         GaugeName = "control_plane.hosts.draining"
	CPHostsTerminating      GaugeName = "control_plane.hosts.terminating"
	CPHostsUnhealthy        GaugeName = "control_plane.hosts.unhealthy"
	CPHostsTerminated       GaugeName = "control_plane.hosts.terminated"
	CPRunnersTotal          GaugeName = "control_plane.runners"
	CPRunnersTotalCurrent   GaugeName = "control_plane.runners.total"
	CPRunnersIdleCurrent    GaugeName = "control_plane.runners.idle"
	CPRunnersBusyCurrent    GaugeName = "control_plane.runners.busy"
	CPWorkloadRunnersTotal  GaugeName = "control_plane.workload.runners.total"
	CPWorkloadRunnersIdle   GaugeName = "control_plane.workload.runners.idle"
	CPWorkloadRunnersBusy   GaugeName = "control_plane.workload.runners.busy"
	CPWorkloadHostsActive   GaugeName = "control_plane.workload.hosts.active"
	CPFleetCPUTotal         GaugeName = "control_plane.fleet.cpu_millicores.total"
	CPFleetCPUUsed          GaugeName = "control_plane.fleet.cpu_millicores.used"
	CPFleetCPUFree          GaugeName = "control_plane.fleet.cpu_millicores.free"
	CPFleetMemTotal         GaugeName = "control_plane.fleet.memory_mb.total"
	CPFleetMemUsed          GaugeName = "control_plane.fleet.memory_mb.used"
	CPFleetMemFree          GaugeName = "control_plane.fleet.memory_mb.free"
	ChunkedDiskCacheSize    GaugeName = "chunked.disk_cache.size"
	ChunkedDiskCacheMax     GaugeName = "chunked.disk_cache.max"
	ChunkedDiskCacheItems   GaugeName = "chunked.disk_cache.items"
	ChunkedMemCacheSize     GaugeName = "chunked.mem_cache.size"
	ChunkedMemCacheMax      GaugeName = "chunked.mem_cache.max"
	ChunkedMemCacheItems    GaugeName = "chunked.mem_cache.items"
	ChunkedDirtyChunks      GaugeName = "chunked.dirty_chunks"
	PoolRunners             GaugeName = "pool.runners"
	PoolMemoryUsed          GaugeName = "pool.memory.used"
	PoolMemoryMax           GaugeName = "pool.memory.max"
	SnapshotSize            GaugeName = "snapshot.size"
	SnapshotAge             GaugeName = "snapshot.age"
	HostGCSSyncBytes        GaugeName = "host.gcs_sync.bytes"
	HostUptime              GaugeName = "host.uptime"
	CacheArtifactSize       GaugeName = "cache.artifact.size"
	NetworkConnections      GaugeName = "network.nat_connections"
	HostGCSessionsBytes     GaugeName = "host.gc.sessions.bytes"
	HostGCSessionStateBytes GaugeName = "host.gc.session_state.bytes"
	HostGCChunkCacheBytes   GaugeName = "host.gc.chunk_cache.bytes"
	HostGCLogBytes          GaugeName = "host.gc.logs.bytes"
	HostGCQuarantineBytes   GaugeName = "host.gc.quarantine.bytes"
)

// Float64Gauge names.
const (
	CPFleetUtilization   Float64GaugeName = "control_plane.fleet.utilization"
	ChunkedCacheHitRatio Float64GaugeName = "chunked.cache_hit_ratio"
	PoolHitRatio         Float64GaugeName = "pool.hit_ratio"
)

// UpDownCounter names.
const (
	CPEndpointRequestsInFlight      UpDownCounterName = "control_plane.endpoint.requests.inflight"
	ManagerEndpointRequestsInFlight UpDownCounterName = "capsule_manager.endpoint.requests.inflight"
	HostRunnersIdle                 UpDownCounterName = "host.runners.idle"
	HostRunnersBusy                 UpDownCounterName = "host.runners.busy"
)

// Description and unit maps for counters.
var counterDescriptions = map[CounterName]string{
	VMAllocations:           "Total number of VM allocations",
	VMTerminations:          "Total number of VM terminations",
	HostHeartbeatTotal:      "Total host heartbeat attempts from capsule-manager to the control plane",
	ManagerEndpointRequests: "Total HTTP requests handled by capsule-manager endpoints",
	CPWebhookRequests:       "Total webhook requests received by the control plane",
	CPAllocations:           "Total VM allocations performed by the control plane",
	CPPlacementSelections:   "Total control-plane placement decisions",
	CPEndpointRequests:      "Total HTTP requests handled by control plane endpoints",
	CPDownscalerActions:     "Total downscaler actions taken",
	SnapshotRollouts:        "Total snapshot rollouts",
	CacheArtifactHits:       "Artifact cache hits",
	CacheArtifactMisses:     "Artifact cache misses",
	CacheGitClones:          "Total git clone operations",
	ChunkedPageFaults:       "Total page faults handled by chunked loader",
	ChunkedCacheHits:        "Chunk cache hits",
	ChunkedCacheMisses:      "Chunk cache misses",
	ChunkedChunkFetches:     "Total chunk fetches from remote storage",
	ChunkedDiskReads:        "Total disk read operations for chunked storage",
	ChunkedDiskWrites:       "Total disk write operations for chunked storage",
	PoolHits:                "Pool lookup hits",
	PoolMisses:              "Pool lookup misses",
	PoolEvictions:           "Total pool evictions",
	PoolRecycleFailures:     "Total pool recycle failures",
	SessionPauseTotal:       "Total session pause operations",
	SessionResumeTotal:      "Total session resume operations",
	SessionResumeRouting:    "Total session resume routing decisions",
	E2ECanarySuccess:        "Total successful e2e canary checks",
	E2ECanaryFailure:        "Total failed e2e canary checks",
	ChunkFetchBytes:         "Total bytes fetched for chunks",
	ChunkSingleflightDedup:  "Total deduplicated chunk fetches via singleflight",
	ChunkNegCacheHits:       "Negative cache hits for chunk lookups",
	NetworkBytesTx:          "Total network bytes transmitted",
	NetworkBytesRx:          "Total network bytes received",
	HostGCBytesReclaimed:    "Total bytes reclaimed by host garbage collection",
	HostGCFilesRemoved:      "Total files or directories removed by host garbage collection",
}

var counterUnits = map[CounterName]string{
	ChunkFetchBytes:      "By",
	NetworkBytesTx:       "By",
	NetworkBytesRx:       "By",
	HostGCBytesReclaimed: "By",
}

// Description and unit maps for histograms.
var histogramDescriptions = map[HistogramName]string{
	VMBootDuration:                 "Duration of VM boot",
	VMReadyDuration:                "Duration until VM is ready",
	VMLifetime:                     "Total VM lifetime",
	VMJobDuration:                  "Duration of a VM job execution",
	HostBootDuration:               "Duration of host boot",
	HostGCSSyncDuration:            "Duration of GCS sync operations",
	HostHeartbeatLatency:           "Latency of host heartbeat",
	ManagerEndpointRequestDuration: "End-to-end latency of a capsule-manager HTTP endpoint request",
	ManagerEndpointRequestSize:     "Size of the incoming HTTP request body handled by a capsule-manager endpoint",
	ManagerEndpointResponseSize:    "Size of the HTTP response body returned by a capsule-manager endpoint",
	CPWebhookLatency:               "Latency of control plane webhook handling",
	CPAllocationLatency:            "Latency of control plane VM allocation",
	CPQueueWait:                    "Time spent waiting in the control plane queue",
	CPEndpointRequestDuration:      "End-to-end latency of a control plane HTTP endpoint request",
	CPEndpointRequestSize:          "Size of the incoming HTTP request body handled by a control plane endpoint",
	CPEndpointResponseSize:         "Size of the HTTP response body returned by a control plane endpoint",
	SnapshotBuildDuration:          "Duration of snapshot build",
	SnapshotUploadDuration:         "Duration of snapshot upload",
	SessionPauseDuration:           "Duration of session pause operation",
	SessionResumeDuration:          "Duration of session resume operation",
	CacheGitCloneDuration:          "Duration of git clone for cache",
	GitHubRegistrationDuration:     "Duration of GitHub runner registration",
	GitHubJobPickupLatency:         "Latency from job queued to picked up",
	UFFDFaultServiceDuration:       "Duration of UFFD fault service handling",
	ChunkFetchDuration:             "Duration of individual chunk fetch",
}

var histogramUnits = map[HistogramName]string{
	VMBootDuration:                 "s",
	VMReadyDuration:                "s",
	VMLifetime:                     "s",
	VMJobDuration:                  "s",
	HostBootDuration:               "s",
	HostGCSSyncDuration:            "s",
	HostHeartbeatLatency:           "s",
	ManagerEndpointRequestDuration: "s",
	ManagerEndpointRequestSize:     "By",
	ManagerEndpointResponseSize:    "By",
	CPWebhookLatency:               "s",
	CPAllocationLatency:            "s",
	CPQueueWait:                    "s",
	CPEndpointRequestDuration:      "s",
	CPEndpointRequestSize:          "By",
	CPEndpointResponseSize:         "By",
	SnapshotBuildDuration:          "s",
	SnapshotUploadDuration:         "s",
	SessionPauseDuration:           "s",
	SessionResumeDuration:          "s",
	CacheGitCloneDuration:          "s",
	GitHubRegistrationDuration:     "s",
	GitHubJobPickupLatency:         "s",
	UFFDFaultServiceDuration:       "s",
	ChunkFetchDuration:             "s",
}

// Description and unit maps for gauges.
var gaugeDescriptions = map[GaugeName]string{
	HostCPUTotal:            "Total CPU millicores on the host",
	HostCPUUsed:             "Used CPU millicores on the host",
	HostMemTotal:            "Total memory in MB on the host",
	HostMemUsed:             "Used memory in MB on the host",
	CPHostsTotal:            "Total number of hosts in the control plane",
	CPHostsReady:            "Number of ready hosts in the control plane",
	CPHostsDraining:         "Number of draining hosts in the control plane",
	CPHostsTerminating:      "Number of terminating hosts in the control plane",
	CPHostsUnhealthy:        "Number of unhealthy hosts in the control plane",
	CPHostsTerminated:       "Number of terminated hosts tracked by the control plane",
	CPRunnersTotal:          "Total number of runners in the control plane",
	CPRunnersTotalCurrent:   "Current total number of runners in the control plane",
	CPRunnersIdleCurrent:    "Current number of idle runners in the control plane",
	CPRunnersBusyCurrent:    "Current number of busy runners in the control plane",
	CPWorkloadRunnersTotal:  "Current total number of runners for a workload in the control plane",
	CPWorkloadRunnersIdle:   "Current number of idle runners for a workload in the control plane",
	CPWorkloadRunnersBusy:   "Current number of busy runners for a workload in the control plane",
	CPWorkloadHostsActive:   "Current number of hosts serving a workload in the control plane",
	CPFleetCPUTotal:         "Total fleet CPU millicores",
	CPFleetCPUUsed:          "Used fleet CPU millicores",
	CPFleetCPUFree:          "Free fleet CPU millicores",
	CPFleetMemTotal:         "Total fleet memory in MB",
	CPFleetMemUsed:          "Used fleet memory in MB",
	CPFleetMemFree:          "Free fleet memory in MB",
	ChunkedDiskCacheSize:    "Current disk cache size in bytes",
	ChunkedDiskCacheMax:     "Maximum disk cache size in bytes",
	ChunkedDiskCacheItems:   "Number of items in the disk cache",
	ChunkedMemCacheSize:     "Current memory cache size in bytes",
	ChunkedMemCacheMax:      "Maximum memory cache size in bytes",
	ChunkedMemCacheItems:    "Number of items in the memory cache",
	ChunkedDirtyChunks:      "Number of dirty chunks pending write",
	PoolRunners:             "Number of runners in the pool",
	PoolMemoryUsed:          "Used memory in the pool in bytes",
	PoolMemoryMax:           "Maximum pool memory in bytes",
	SnapshotSize:            "Size of the current snapshot in bytes",
	SnapshotAge:             "Age of the current snapshot in seconds",
	HostGCSSyncBytes:        "Bytes synced from GCS",
	HostUptime:              "Host uptime in seconds",
	CacheArtifactSize:       "Artifact cache size in bytes",
	NetworkConnections:      "Number of active NAT connections",
	HostGCSessionsBytes:     "Current bytes consumed by local session directories",
	HostGCSessionStateBytes: "Current bytes consumed by temporary session-state files",
	HostGCChunkCacheBytes:   "Current bytes consumed by the on-disk chunk cache",
	HostGCLogBytes:          "Current bytes consumed by runner logs and metrics",
	HostGCQuarantineBytes:   "Current bytes consumed by quarantine directories",
}

var gaugeUnits = map[GaugeName]string{}

// Description and unit maps for float64 gauges.
var float64GaugeDescriptions = map[Float64GaugeName]string{
	CPFleetUtilization:   "Fleet utilization ratio",
	ChunkedCacheHitRatio: "Chunked cache hit ratio",
	PoolHitRatio:         "Pool hit ratio",
}

var float64GaugeUnits = map[Float64GaugeName]string{}

// Description and unit maps for up-down counters.
var upDownCounterDescriptions = map[UpDownCounterName]string{
	CPEndpointRequestsInFlight:      "Number of in-flight HTTP requests for control plane endpoints",
	ManagerEndpointRequestsInFlight: "Number of in-flight HTTP requests for capsule-manager endpoints",
	HostRunnersIdle:                 "Number of idle runners on the host",
	HostRunnersBusy:                 "Number of busy runners on the host",
}

var upDownCounterUnits = map[UpDownCounterName]string{}

// NewCounter creates an Int64Counter with the registered description and unit.
func NewCounter(meter metric.Meter, name CounterName) (metric.Int64Counter, error) {
	opts := []metric.Int64CounterOption{}
	if desc, ok := counterDescriptions[name]; ok {
		opts = append(opts, metric.WithDescription(desc))
	}
	if unit, ok := counterUnits[name]; ok {
		opts = append(opts, metric.WithUnit(unit))
	}
	return meter.Int64Counter(string(name), opts...)
}

// NewHistogram creates a Float64Histogram with the registered description and unit.
func NewHistogram(meter metric.Meter, name HistogramName) (metric.Float64Histogram, error) {
	opts := []metric.Float64HistogramOption{}
	if desc, ok := histogramDescriptions[name]; ok {
		opts = append(opts, metric.WithDescription(desc))
	}
	if unit, ok := histogramUnits[name]; ok {
		opts = append(opts, metric.WithUnit(unit))
	}
	return meter.Float64Histogram(string(name), opts...)
}

// NewGauge creates an Int64Gauge with the registered description and unit.
func NewGauge(meter metric.Meter, name GaugeName) (metric.Int64Gauge, error) {
	opts := []metric.Int64GaugeOption{}
	if desc, ok := gaugeDescriptions[name]; ok {
		opts = append(opts, metric.WithDescription(desc))
	}
	if unit, ok := gaugeUnits[name]; ok {
		opts = append(opts, metric.WithUnit(unit))
	}
	return meter.Int64Gauge(string(name), opts...)
}

// NewFloat64Gauge creates a Float64Gauge with the registered description and unit.
func NewFloat64Gauge(meter metric.Meter, name Float64GaugeName) (metric.Float64Gauge, error) {
	opts := []metric.Float64GaugeOption{}
	if desc, ok := float64GaugeDescriptions[name]; ok {
		opts = append(opts, metric.WithDescription(desc))
	}
	if unit, ok := float64GaugeUnits[name]; ok {
		opts = append(opts, metric.WithUnit(unit))
	}
	return meter.Float64Gauge(string(name), opts...)
}

// NewUpDownCounter creates an Int64UpDownCounter with the registered description and unit.
func NewUpDownCounter(meter metric.Meter, name UpDownCounterName) (metric.Int64UpDownCounter, error) {
	opts := []metric.Int64UpDownCounterOption{}
	if desc, ok := upDownCounterDescriptions[name]; ok {
		opts = append(opts, metric.WithDescription(desc))
	}
	if unit, ok := upDownCounterUnits[name]; ok {
		opts = append(opts, metric.WithUnit(unit))
	}
	return meter.Int64UpDownCounter(string(name), opts...)
}

// Common attribute keys.
const (
	AttrResult          = attribute.Key("result")
	AttrStatus          = attribute.Key("status")
	AttrMethod          = attribute.Key("method")
	AttrRoute           = attribute.Key("route")
	AttrReason          = attribute.Key("reason")
	AttrRouting         = attribute.Key("routing")
	AttrSelectionReason = attribute.Key("selection_reason")
	AttrCacheState      = attribute.Key("cache_state")
	AttrSource          = attribute.Key("source")
	AttrPhase           = attribute.Key("phase")
	AttrStatusCode      = attribute.Key("status_code")
	AttrStatusClass     = attribute.Key("status_class")
	AttrArtifactClass   = attribute.Key("artifact_class")
	AttrWorkloadKey     = attribute.Key("workload_key")
	AttrHostID          = attribute.Key("host_id")
	AttrRunnerID        = attribute.Key("runner_id")
	AttrSessionID       = attribute.Key("session_id")
)

// Common attribute values.
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
	ResultTimeout = "timeout"
	ResultError   = "error"

	RoutingSameHost  = "same_host"
	RoutingCrossHost = "cross_host"
	RoutingLocal     = "local"
	RoutingGCS       = "gcs"
)
