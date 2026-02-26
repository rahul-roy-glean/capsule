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
