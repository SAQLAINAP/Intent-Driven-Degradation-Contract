// Package metrics registers all IDDC Prometheus metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// CurrentTier exposes exactly one series per service: the enforced tier's
	// position in the policy ladder (0 = least degraded), labelled with its name.
	CurrentTier = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "dg_current_tier",
		Help: "Ladder index of the enforced degradation tier (0 = least degraded); the tier label names it.",
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

// SetCurrentTier replaces the service's dg_current_tier series so only the
// enforced tier is reported (stale series from earlier tiers are removed).
func SetCurrentTier(service, tier string, index int) {
	CurrentTier.DeletePartialMatch(prometheus.Labels{"service": service})
	CurrentTier.WithLabelValues(service, tier).Set(float64(index))
}
