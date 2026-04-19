//go:build linux,ebpf

// Extended kernel-metric collector for Linux. Build with -tags ebpf to activate.
//
// Reads CPU utilisation, network throughput, and TCP retransmit rate from /proc
// using delta-sampling between consecutive Collect calls. This provides the
// higher-fidelity signals that a future full eBPF implementation would also
// expose — the interface is intentionally identical so the upgrade is a drop-in.
package collector

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SysCollector reads extended kernel metrics from /proc.
// It supersedes ProcCollector when built with -tags ebpf.
type SysCollector struct {
	mu      sync.Mutex
	lastCPU *cpuSample
	lastNet map[string]*netSample
	lastTCP *tcpSample
	lastAt  time.Time
	iface   string // primary network interface (auto-detected on first call)
}

// NewSysCollector returns an extended system metrics collector.
func NewSysCollector() *SysCollector {
	return &SysCollector{}
}

// Collect returns the current value for a named metric.
// Supported: mem_pressure, cpu_utilization, net_rx_kbps, net_tx_kbps, tcp_retransmit_rate.
func (c *SysCollector) Collect(ctx context.Context, metricID string) (float64, error) {
	switch metricID {
	case "mem_pressure":
		return c.memPressure()
	case "cpu_utilization":
		return c.cpuUtilization()
	case "net_rx_kbps":
		return c.netKbps(true)
	case "net_tx_kbps":
		return c.netKbps(false)
	case "tcp_retransmit_rate":
		return c.tcpRetransmitRate()
	default:
		return 0, nil
	}
}

// ─── Memory ───────────────────────────────────────────────────────────────────

func (c *SysCollector) memPressure() (float64, error) {
	// Prefer cgroup v2 memory.pressure (PSI) when available.
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.pressure"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "some avg10=") {
				// "some avg10=0.00 avg60=0.00 avg300=0.00 total=0"
				f := strings.Fields(line)
				for _, part := range f {
					if strings.HasPrefix(part, "avg10=") {
						v, err := strconv.ParseFloat(strings.TrimPrefix(part, "avg10="), 64)
						if err == nil {
							return v / 100.0, nil // convert percent to 0-1
						}
					}
				}
			}
		}
	}
	// Fall back to /proc/meminfo.
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0.2, nil // synthetic for non-Linux dev
	}
	fields := parseProcKeyVal(string(data))
	total, ok1 := fields["MemTotal"]
	avail, ok2 := fields["MemAvailable"]
	if !ok1 || !ok2 || total == 0 {
		return 0, fmt.Errorf("meminfo fields missing")
	}
	return 1.0 - float64(avail)/float64(total), nil
}

// ─── CPU ──────────────────────────────────────────────────────────────────────

type cpuSample struct {
	user, nice, system, idle, iowait, irq, softirq, steal int64
	at                                                     time.Time
}

func readCPUSample() (*cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return nil, fmt.Errorf("unexpected /proc/stat format")
		}
		parse := func(i int) int64 {
			v, _ := strconv.ParseInt(fields[i], 10, 64)
			return v
		}
		return &cpuSample{
			user: parse(1), nice: parse(2), system: parse(3), idle: parse(4),
			iowait: parse(5), irq: parse(6), softirq: parse(7), steal: parse(8),
			at: time.Now(),
		}, nil
	}
	return nil, fmt.Errorf("/proc/stat: cpu line not found")
}

func (c *SysCollector) cpuUtilization() (float64, error) {
	cur, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	prev := c.lastCPU
	c.lastCPU = cur
	c.mu.Unlock()

	if prev == nil {
		return 0, nil // first call: no delta yet
	}

	prevTotal := prev.user + prev.nice + prev.system + prev.idle + prev.iowait + prev.irq + prev.softirq + prev.steal
	curTotal := cur.user + cur.nice + cur.system + cur.idle + cur.iowait + cur.irq + cur.softirq + cur.steal
	deltaTotal := curTotal - prevTotal
	if deltaTotal == 0 {
		return 0, nil
	}

	deltaIdle := cur.idle + cur.iowait - prev.idle - prev.iowait
	return 1.0 - float64(deltaIdle)/float64(deltaTotal), nil
}

// ─── Network ──────────────────────────────────────────────────────────────────

type netSample struct {
	rxBytes, txBytes int64
	at               time.Time
}

func readNetSamples() (map[string]*netSample, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := map[string]*netSample{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.Contains(line, ":") {
			continue
		}
		idx := strings.Index(line, ":")
		iface := strings.TrimSpace(line[:idx])
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		result[iface] = &netSample{rxBytes: rx, txBytes: tx, at: time.Now()}
	}
	return result, nil
}

// primaryIface returns the first non-loopback, non-virtual interface.
func primaryIface(samples map[string]*netSample) string {
	for name := range samples {
		if name != "lo" && !strings.HasPrefix(name, "veth") && !strings.HasPrefix(name, "docker") {
			return name
		}
	}
	for name := range samples {
		if name != "lo" {
			return name
		}
	}
	return "lo"
}

func (c *SysCollector) netKbps(rx bool) (float64, error) {
	cur, err := readNetSamples()
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	if c.iface == "" {
		c.iface = primaryIface(cur)
	}
	iface := c.iface
	prev := c.lastNet
	c.lastNet = cur
	c.mu.Unlock()

	if prev == nil {
		return 0, nil // first call
	}

	prevS, okP := prev[iface]
	curS, okC := cur[iface]
	if !okP || !okC {
		return 0, nil
	}

	elapsed := curS.at.Sub(prevS.at).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	var deltaBytes int64
	if rx {
		deltaBytes = curS.rxBytes - prevS.rxBytes
	} else {
		deltaBytes = curS.txBytes - prevS.txBytes
	}
	return float64(deltaBytes) / elapsed / 1024, nil // bytes/s → KB/s
}

// ─── TCP retransmit rate ──────────────────────────────────────────────────────

type tcpSample struct {
	outSegs, retransSegs int64
	at                   time.Time
}

func readTCPSample() (*tcpSample, error) {
	data, err := os.ReadFile("/proc/net/snmp")
	if err != nil {
		return nil, err
	}

	// Find the Tcp: header and value lines.
	var headers, values []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Tcp:") {
			f := strings.Fields(line)[1:] // drop "Tcp:"
			if headers == nil {
				headers = f
			} else {
				values = f
			}
		}
	}
	if len(headers) == 0 || len(headers) != len(values) {
		return nil, fmt.Errorf("/proc/net/snmp: Tcp section not found or malformed")
	}

	fieldIdx := map[string]int{}
	for i, h := range headers {
		fieldIdx[h] = i
	}
	get := func(key string) int64 {
		i, ok := fieldIdx[key]
		if !ok || i >= len(values) {
			return 0
		}
		v, _ := strconv.ParseInt(values[i], 10, 64)
		return v
	}
	return &tcpSample{
		outSegs:    get("OutSegs"),
		retransSegs: get("RetransSegs"),
		at:         time.Now(),
	}, nil
}

func (c *SysCollector) tcpRetransmitRate() (float64, error) {
	cur, err := readTCPSample()
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	prev := c.lastTCP
	c.lastTCP = cur
	c.mu.Unlock()

	if prev == nil {
		return 0, nil
	}

	deltaOut := cur.outSegs - prev.outSegs
	deltaRetrans := cur.retransSegs - prev.retransSegs
	if deltaOut <= 0 {
		return 0, nil
	}
	return float64(deltaRetrans) / float64(deltaOut), nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func parseProcKeyVal(raw string) map[string]int64 {
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
