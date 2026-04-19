// Package collector provides signal collection via /proc (default) or eBPF (Phase 5).
//
// The Collector interface is what the SignalBus calls to get metric values.
// The ProcCollector reads from Linux /proc filesystem.
// The eBPF implementation lives in collector_ebpf.go (build tag: linux,ebpf).
package collector

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Collector is the interface every metric source must implement.
type Collector interface {
	// Collect returns the current value for a named metric.
	Collect(ctx context.Context, metricID string) (float64, error)
}

// ProcCollector reads metrics from /proc on Linux.
// On macOS or unsupported platforms it returns synthetic test values.
type ProcCollector struct{}

// NewProcCollector returns a /proc-based Collector.
func NewProcCollector() *ProcCollector {
	return &ProcCollector{}
}

// Collect returns the current value for a known metric ID.
// Supported IDs: "mem_pressure" (from /proc/meminfo).
// Unknown metrics return 0.
func (c *ProcCollector) Collect(ctx context.Context, metricID string) (float64, error) {
	switch metricID {
	case "mem_pressure":
		return c.memPressure()
	default:
		// For POC: unknown metrics return 0 (safe default, won't trigger tiers).
		return 0, nil
	}
}

// memPressure returns a 0–1 fraction of used memory from /proc/meminfo.
func (c *ProcCollector) memPressure() (float64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		// Not on Linux (macOS dev) — return synthetic value.
		return 0.2, nil
	}
	fields := parseMeminfo(string(data))
	total, ok1 := fields["MemTotal"]
	avail, ok2 := fields["MemAvailable"]
	if !ok1 || !ok2 || total == 0 {
		return 0, fmt.Errorf("meminfo fields missing")
	}
	return 1.0 - float64(avail)/float64(total), nil
}

func parseMeminfo(raw string) map[string]int64 {
	out := make(map[string]int64)
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		val, err := strconv.ParseInt(parts[1], 10, 64)
		if err == nil {
			out[key] = val
		}
	}
	return out
}

// InjectedCollector is a test/simulation collector that returns values set by the caller.
// Used by `dg simulate` and unit tests.
type InjectedCollector struct {
	values map[string]float64
}

// NewInjectedCollector creates a collector with an initial value map.
func NewInjectedCollector(values map[string]float64) *InjectedCollector {
	if values == nil {
		values = map[string]float64{}
	}
	return &InjectedCollector{values: values}
}

// Set updates a single signal value.
func (c *InjectedCollector) Set(id string, v float64) {
	c.values[id] = v
}

// Collect returns the injected value for the metric, or 0 if not set.
func (c *InjectedCollector) Collect(_ context.Context, metricID string) (float64, error) {
	if v, ok := c.values[metricID]; ok {
		return v, nil
	}
	return 0, nil
}
