// Package metrics registers all IDDC Prometheus metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// CurrentTier tracks the active degradation tier as a gauge (0=nominal … 4=survival).
	CurrentTier = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dg_current_tier",
		Help: "Active degradation tier (0=nominal, 1=warm, 2=hot, 3=critical, 4=survival).",
	}, []string{"service", "tier"})

	// SignalValue tracks the latest sampled value for each declared signal.
	SignalValue = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dg_signal_value",
		Help: "Current value of each declared signal.",
	}, []string{"service", "signal_id"})

	// TierTransitionsTotal counts all confirmed tier transition events.
	TierTransitionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dg_tier_transitions_total",
		Help: "Total number of confirmed tier transition events.",
	}, []string{"service", "from_tier", "to_tier"})

	// GateEventsTotal counts human gate outcomes.
	GateEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dg_gate_events_total",
		Help: "Human gate outcomes: approved / overridden / timed_out.",
	}, []string{"service", "outcome"})

	// WriteQueueDepth tracks the current depth of the bounded write queue.
	WriteQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dg_write_queue_depth",
		Help: "Current number of entries in the write queue.",
	}, []string{"service"})

	// WriteQueueDroppedTotal counts writes evicted due to TTL expiry or queue overflow.
	WriteQueueDroppedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "dg_write_queue_dropped_total",
		Help: "Total writes dropped due to TTL expiry or queue overflow.",
	}, []string{"service"})
)

// TierNames maps tier index to canonical name for the CurrentTier gauge.
// Index 0 = nominal (least degraded), index N = survival (most degraded).
var TierNames = []string{"nominal", "warm", "hot", "critical", "survival"}

// TierIndex returns the numeric index for a tier name, or -1 if unknown.
func TierIndex(name string) float64 {
	for i, n := range TierNames {
		if n == name {
			return float64(i)
		}
	}
	return -1
}
