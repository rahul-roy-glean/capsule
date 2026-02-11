package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Host metrics
	hostTotalSlots = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_total_slots",
		Help: "Total runner slots on this host",
	})

	hostUsedSlots = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "firecracker_host_used_slots",
		Help: "Used runner slots on this host",
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
func UpdateHostMetrics(total, used, idle, busy int) {
	hostTotalSlots.Set(float64(total))
	hostUsedSlots.Set(float64(used))
	hostIdleRunners.Set(float64(idle))
	hostBusyRunners.Set(float64(busy))
}
