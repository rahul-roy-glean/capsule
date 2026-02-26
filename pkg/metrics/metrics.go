package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Host metrics
	hostCPUTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_cpu_millicores_total",
		Help: "Total CPU millicores on this host",
	})

	hostCPUUsed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_cpu_millicores_used",
		Help: "Used CPU millicores on this host",
	})

	hostMemTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_memory_mb_total",
		Help: "Total memory MB on this host",
	})

	hostMemUsed = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_memory_mb_used",
		Help: "Used memory MB on this host",
	})

	hostIdleRunners = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_idle_runners",
		Help: "Number of idle runners on this host",
	})

	hostBusyRunners = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_busy_runners",
		Help: "Number of busy runners on this host",
	})

	// UFFD fault service time
	UFFDFaultServiceSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "uffd_fault_service_seconds",
		Help:    "Time to service a single UFFD page fault",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	})

	// Chunk fetch duration by source
	ChunkFetchSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "chunk_fetch_seconds",
		Help:    "Time to fetch a chunk by source",
		Buckets: []float64{0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
	}, []string{"source"})

	// Chunk fetch bytes by source
	ChunkFetchBytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "chunk_fetch_bytes_total",
		Help: "Total bytes fetched by source",
	}, []string{"source"})

	// Cache hit ratio
	ChunkCacheHitRatio = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "chunk_cache_hit_ratio",
		Help: "Ratio of chunk cache hits to total lookups",
	})

	// Per-runner GCS egress
	RunnerGCSEgressBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "runner_gcs_egress_bytes",
		Help: "Total bytes fetched from GCS per runner",
	}, []string{"runner_id"})

	// Singleflight dedup ratio
	SingleflightDedupTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chunk_singleflight_dedup_total",
		Help: "Total number of chunk fetches deduplicated by singleflight",
	})

	// Negative cache hits
	ChunkNegCacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "chunk_neg_cache_hits_total",
		Help: "Total negative cache hits (known-missing chunks)",
	})
)

// RegisterHostMetrics registers host-level metrics (called once at startup)
func RegisterHostMetrics() {
	// Metrics are auto-registered via promauto
}

// UpdateHostMetrics updates the host-level metrics
func UpdateHostMetrics(cpuTotal, cpuUsed, memTotal, memUsed, idle, busy int) {
	hostCPUTotal.Set(float64(cpuTotal))
	hostCPUUsed.Set(float64(cpuUsed))
	hostMemTotal.Set(float64(memTotal))
	hostMemUsed.Set(float64(memUsed))
	hostIdleRunners.Set(float64(idle))
	hostBusyRunners.Set(float64(busy))
}
