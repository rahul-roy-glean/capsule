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
	VMBootDuration             HistogramName = "vm.boot.duration"
	VMReadyDuration            HistogramName = "vm.ready.duration"
	VMLifetime                 HistogramName = "vm.lifetime"
	VMJobDuration              HistogramName = "vm.job.duration"
	HostBootDuration           HistogramName = "host.boot.duration"
	HostGCSSyncDuration        HistogramName = "host.gcs_sync.duration"
	HostHeartbeatLatency       HistogramName = "host.heartbeat.latency"
	CPWebhookLatency           HistogramName = "control_plane.webhook.latency"
	CPAllocationLatency        HistogramName = "control_plane.allocation.latency"
	CPQueueWait                HistogramName = "control_plane.queue.wait"
	CPEndpointRequestDuration  HistogramName = "control_plane.endpoint.request.duration"
	CPEndpointRequestSize      HistogramName = "control_plane.endpoint.request.size"
	CPEndpointResponseSize     HistogramName = "control_plane.endpoint.response.size"
	SnapshotBuildDuration      HistogramName = "snapshot.build.duration"
	SnapshotUploadDuration     HistogramName = "snapshot.upload.duration"
	SessionPauseDuration       HistogramName = "session.pause.duration"
	SessionResumeDuration      HistogramName = "session.resume.duration"
	CacheGitCloneDuration      HistogramName = "cache.clone.duration"
	GitHubRegistrationDuration HistogramName = "ci.runner_registration.duration"
	GitHubJobPickupLatency     HistogramName = "ci.job_pickup.latency"
	UFFDFaultServiceDuration   HistogramName = "uffd.fault_service.duration"
	ChunkFetchDuration         HistogramName = "chunk.fetch.duration"
)

// Counter names (Int64Counter).
const (
	VMAllocations          CounterName = "vm.allocations"
	VMTerminations         CounterName = "vm.terminations"
	CPWebhookRequests      CounterName = "control_plane.webhook.requests"
	CPAllocations          CounterName = "control_plane.allocations"
	CPEndpointRequests     CounterName = "control_plane.endpoint.requests"
	CPDownscalerActions    CounterName = "control_plane.downscaler.actions"
	SnapshotRollouts       CounterName = "snapshot.rollouts"
	CacheArtifactHits      CounterName = "cache.artifact.hits"
	CacheArtifactMisses    CounterName = "cache.artifact.misses"
	CacheGitClones         CounterName = "cache.git_clones"
	CITokenRequests        CounterName = "ci.token_requests"
	CIJobs                 CounterName = "ci.jobs"
	ChunkedPageFaults      CounterName = "chunked.page_faults"
	ChunkedCacheHits       CounterName = "chunked.cache_hits"
	ChunkedChunkFetches    CounterName = "chunked.chunk_fetches"
	ChunkedDiskReads       CounterName = "chunked.disk_reads"
	ChunkedDiskWrites      CounterName = "chunked.disk_writes"
	PoolHits               CounterName = "pool.hits"
	PoolMisses             CounterName = "pool.misses"
	PoolEvictions          CounterName = "pool.evictions"
	PoolRecycleFailures    CounterName = "pool.recycle_failures"
	SessionPauseTotal      CounterName = "session.pause.total"
	SessionResumeTotal     CounterName = "session.resume.total"
	SessionResumeRouting   CounterName = "session.resume.routing"
	E2ECanarySuccess       CounterName = "e2e.canary.success"
	E2ECanaryFailure       CounterName = "e2e.canary.failure"
	ChunkFetchBytes        CounterName = "chunk.fetch.bytes"
	ChunkSingleflightDedup CounterName = "chunk.singleflight.dedup"
	ChunkNegCacheHits      CounterName = "chunk.neg_cache.hits"
	NetworkBytesTx         CounterName = "network.bytes_tx"
	NetworkBytesRx         CounterName = "network.bytes_rx"
)

// Gauge names (Int64Gauge).
const (
	HostCPUTotal          GaugeName = "host.cpu_millicores.total"
	HostCPUUsed           GaugeName = "host.cpu_millicores.used"
	HostMemTotal          GaugeName = "host.memory_mb.total"
	HostMemUsed           GaugeName = "host.memory_mb.used"
	CPHostsTotal          GaugeName = "control_plane.hosts"
	CPRunnersTotal        GaugeName = "control_plane.runners"
	CPQueueDepth          GaugeName = "control_plane.queue.depth"
	CPFleetCPUTotal       GaugeName = "control_plane.fleet.cpu_millicores.total"
	CPFleetCPUUsed        GaugeName = "control_plane.fleet.cpu_millicores.used"
	CPFleetCPUFree        GaugeName = "control_plane.fleet.cpu_millicores.free"
	CPFleetMemTotal       GaugeName = "control_plane.fleet.memory_mb.total"
	CPFleetMemUsed        GaugeName = "control_plane.fleet.memory_mb.used"
	CPFleetMemFree        GaugeName = "control_plane.fleet.memory_mb.free"
	ChunkedDiskCacheSize  GaugeName = "chunked.disk_cache.size"
	ChunkedDiskCacheMax   GaugeName = "chunked.disk_cache.max"
	ChunkedDiskCacheItems GaugeName = "chunked.disk_cache.items"
	ChunkedMemCacheSize   GaugeName = "chunked.mem_cache.size"
	ChunkedMemCacheMax    GaugeName = "chunked.mem_cache.max"
	ChunkedMemCacheItems  GaugeName = "chunked.mem_cache.items"
	ChunkedDirtyChunks    GaugeName = "chunked.dirty_chunks"
	PoolRunners           GaugeName = "pool.runners"
	PoolMemoryUsed        GaugeName = "pool.memory.used"
	PoolMemoryMax         GaugeName = "pool.memory.max"
	SnapshotSize          GaugeName = "snapshot.size"
	SnapshotAge           GaugeName = "snapshot.age"
	HostGCSSyncBytes      GaugeName = "host.gcs_sync.bytes"
	HostUptime            GaugeName = "host.uptime"
	CacheArtifactSize     GaugeName = "cache.artifact.size"
	NetworkConnections    GaugeName = "network.nat_connections"
)

// Float64Gauge names.
const (
	CPFleetUtilization   Float64GaugeName = "control_plane.fleet.utilization"
	ChunkedCacheHitRatio Float64GaugeName = "chunked.cache_hit_ratio"
	PoolHitRatio         Float64GaugeName = "pool.hit_ratio"
)

// UpDownCounter names.
const (
	CPEndpointRequestsInFlight UpDownCounterName = "control_plane.endpoint.requests.inflight"
	HostRunnersIdle            UpDownCounterName = "host.runners.idle"
	HostRunnersBusy            UpDownCounterName = "host.runners.busy"
)

// Description and unit maps for counters.
var counterDescriptions = map[CounterName]string{
	VMAllocations:          "Total number of VM allocations",
	VMTerminations:         "Total number of VM terminations",
	CPWebhookRequests:      "Total webhook requests received by the control plane",
	CPAllocations:          "Total VM allocations performed by the control plane",
	CPEndpointRequests:     "Total HTTP requests handled by control plane endpoints",
	CPDownscalerActions:    "Total downscaler actions taken",
	SnapshotRollouts:       "Total snapshot rollouts",
	CacheArtifactHits:      "Artifact cache hits",
	CacheArtifactMisses:    "Artifact cache misses",
	CacheGitClones:         "Total git clone operations",
	CITokenRequests:        "Total CI token requests",
	CIJobs:                 "Total CI jobs processed",
	ChunkedPageFaults:      "Total page faults handled by chunked loader",
	ChunkedCacheHits:       "Chunk cache hits",
	ChunkedChunkFetches:    "Total chunk fetches from remote storage",
	ChunkedDiskReads:       "Total disk read operations for chunked storage",
	ChunkedDiskWrites:      "Total disk write operations for chunked storage",
	PoolHits:               "Pool lookup hits",
	PoolMisses:             "Pool lookup misses",
	PoolEvictions:          "Total pool evictions",
	PoolRecycleFailures:    "Total pool recycle failures",
	SessionPauseTotal:      "Total session pause operations",
	SessionResumeTotal:     "Total session resume operations",
	SessionResumeRouting:   "Total session resume routing decisions",
	E2ECanarySuccess:       "Total successful e2e canary checks",
	E2ECanaryFailure:       "Total failed e2e canary checks",
	ChunkFetchBytes:        "Total bytes fetched for chunks",
	ChunkSingleflightDedup: "Total deduplicated chunk fetches via singleflight",
	ChunkNegCacheHits:      "Negative cache hits for chunk lookups",
	NetworkBytesTx:         "Total network bytes transmitted",
	NetworkBytesRx:         "Total network bytes received",
}

var counterUnits = map[CounterName]string{
	ChunkFetchBytes: "By",
	NetworkBytesTx:  "By",
	NetworkBytesRx:  "By",
}

// Description and unit maps for histograms.
var histogramDescriptions = map[HistogramName]string{
	VMBootDuration:             "Duration of VM boot",
	VMReadyDuration:            "Duration until VM is ready",
	VMLifetime:                 "Total VM lifetime",
	VMJobDuration:              "Duration of a VM job execution",
	HostBootDuration:           "Duration of host boot",
	HostGCSSyncDuration:        "Duration of GCS sync operations",
	HostHeartbeatLatency:       "Latency of host heartbeat",
	CPWebhookLatency:           "Latency of control plane webhook handling",
	CPAllocationLatency:        "Latency of control plane VM allocation",
	CPQueueWait:                "Time spent waiting in the control plane queue",
	CPEndpointRequestDuration:  "End-to-end latency of a control plane HTTP endpoint request",
	CPEndpointRequestSize:      "Size of the incoming HTTP request body handled by a control plane endpoint",
	CPEndpointResponseSize:     "Size of the HTTP response body returned by a control plane endpoint",
	SnapshotBuildDuration:      "Duration of snapshot build",
	SnapshotUploadDuration:     "Duration of snapshot upload",
	SessionPauseDuration:       "Duration of session pause operation",
	SessionResumeDuration:      "Duration of session resume operation",
	CacheGitCloneDuration:      "Duration of git clone for cache",
	GitHubRegistrationDuration: "Duration of GitHub runner registration",
	GitHubJobPickupLatency:     "Latency from job queued to picked up",
	UFFDFaultServiceDuration:   "Duration of UFFD fault service handling",
	ChunkFetchDuration:         "Duration of individual chunk fetch",
}

var histogramUnits = map[HistogramName]string{
	VMBootDuration:             "s",
	VMReadyDuration:            "s",
	VMLifetime:                 "s",
	VMJobDuration:              "s",
	HostBootDuration:           "s",
	HostGCSSyncDuration:        "s",
	HostHeartbeatLatency:       "s",
	CPWebhookLatency:           "s",
	CPAllocationLatency:        "s",
	CPQueueWait:                "s",
	CPEndpointRequestDuration:  "s",
	CPEndpointRequestSize:      "By",
	CPEndpointResponseSize:     "By",
	SnapshotBuildDuration:      "s",
	SnapshotUploadDuration:     "s",
	SessionPauseDuration:       "s",
	SessionResumeDuration:      "s",
	CacheGitCloneDuration:      "s",
	GitHubRegistrationDuration: "s",
	GitHubJobPickupLatency:     "s",
	UFFDFaultServiceDuration:   "s",
	ChunkFetchDuration:         "s",
}

// Description and unit maps for gauges.
var gaugeDescriptions = map[GaugeName]string{
	HostCPUTotal:          "Total CPU millicores on the host",
	HostCPUUsed:           "Used CPU millicores on the host",
	HostMemTotal:          "Total memory in MB on the host",
	HostMemUsed:           "Used memory in MB on the host",
	CPHostsTotal:          "Total number of hosts in the control plane",
	CPRunnersTotal:        "Total number of runners in the control plane",
	CPQueueDepth:          "Current control plane queue depth",
	CPFleetCPUTotal:       "Total fleet CPU millicores",
	CPFleetCPUUsed:        "Used fleet CPU millicores",
	CPFleetCPUFree:        "Free fleet CPU millicores",
	CPFleetMemTotal:       "Total fleet memory in MB",
	CPFleetMemUsed:        "Used fleet memory in MB",
	CPFleetMemFree:        "Free fleet memory in MB",
	ChunkedDiskCacheSize:  "Current disk cache size in bytes",
	ChunkedDiskCacheMax:   "Maximum disk cache size in bytes",
	ChunkedDiskCacheItems: "Number of items in the disk cache",
	ChunkedMemCacheSize:   "Current memory cache size in bytes",
	ChunkedMemCacheMax:    "Maximum memory cache size in bytes",
	ChunkedMemCacheItems:  "Number of items in the memory cache",
	ChunkedDirtyChunks:    "Number of dirty chunks pending write",
	PoolRunners:           "Number of runners in the pool",
	PoolMemoryUsed:        "Used memory in the pool in bytes",
	PoolMemoryMax:         "Maximum pool memory in bytes",
	SnapshotSize:          "Size of the current snapshot in bytes",
	SnapshotAge:           "Age of the current snapshot in seconds",
	HostGCSSyncBytes:      "Bytes synced from GCS",
	HostUptime:            "Host uptime in seconds",
	CacheArtifactSize:     "Artifact cache size in bytes",
	NetworkConnections:    "Number of active NAT connections",
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
	CPEndpointRequestsInFlight: "Number of in-flight HTTP requests for control plane endpoints",
	HostRunnersIdle:            "Number of idle runners on the host",
	HostRunnersBusy:            "Number of busy runners on the host",
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
	AttrResult      = attribute.Key("result")
	AttrStatus      = attribute.Key("status")
	AttrMethod      = attribute.Key("method")
	AttrRoute       = attribute.Key("route")
	AttrReason      = attribute.Key("reason")
	AttrRouting     = attribute.Key("routing")
	AttrSource      = attribute.Key("source")
	AttrPhase       = attribute.Key("phase")
	AttrStatusCode  = attribute.Key("status_code")
	AttrStatusClass = attribute.Key("status_class")
	AttrWorkloadKey = attribute.Key("workload_key")
	AttrHostID      = attribute.Key("host_id")
	AttrRunnerID    = attribute.Key("runner_id")
	AttrSessionID   = attribute.Key("session_id")
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
