# IDDC — Intent-Driven Degradation Contracts

[![CI](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/actions/workflows/ci.yml/badge.svg)](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/SAQLAINAP/Intent-Driven-Degradation-Contract)](https://goreportcard.com/report/github.com/SAQLAINAP/Intent-Driven-Degradation-Contract)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/go-%3E%3D1.24-blue)](go.mod)

> A declarative failure-topology framework for distributed systems.  
> Define exactly how your service behaves under stress — **at design time, before failure occurs.**

---

## The Problem

Service degradation is reactive by default. Engineers discover what their system does under load at 3 AM, mid-incident, with customers impacted. Runbooks go stale. On-call responders make inconsistent decisions. Features get disabled in different orders by different people every time.

**IDDC changes the contract.** You author a `degradation.yaml` alongside your `deployment.yaml`, compile it into a verified binary, and the runtime enforces it continuously — automatically adjusting replicas, feature flags, and write queues as signals cross thresholds. A human gate blocks dangerous transitions until an operator approves.

---

## Core Concept

```yaml
service: api-gateway
version: "1.0.0"

signals:
  - id: rps
  - id: db_latency_p99

tiers:
  - name: nominal
    when: { rps: { lt: 800 } }
    behavior: { mode: full_service }

  - name: warm
    when: { rps: { gte: 800, lt: 1500 } }
    behavior:
      mode: full_service
      disable: [recommendations, analytics]

  - name: critical
    when:
      any_of:
        - rps: { gte: 2000 }
        - db_latency_p99: { gte: 2000 }
    human_gate:
      wait: 45s
      on_timeout: execute_policy
    behavior:
      mode: read_only
      queue_writes: { max_depth: 2000, ttl: 120s }

  - name: survival
    when: { mem_pressure: { gte: 0.95 } }
    behavior: { mode: static_fallback }
```

The **compiler** validates your intent and catches gaps, contradictions, and dependency cycles at CI time — not in production. It emits a `.dg` binary that the **runtime engine** enforces continuously.

---

## Features

- **Declarative degradation policy** — YAML DSL with a 4-stage compiler (parse → validate → blast-radius → emit)
- **Compiler diagnostics** — catches tier gaps, undeclared signals, circular blast-radius dependencies, and TTL/SLA mismatches before deploy
- **Signal bus** — polls Prometheus, HTTP endpoints, or injected test values on configurable cadences
- **Hysteresis engine** — prevents flapping with configurable up/down stabilisation windows
- **Human gate** — dead man's switch blocks risky transitions; sends Slack Block Kit notifications + PagerDuty incidents, waits for operator approval
- **Kubernetes enforcement** — patches Deployment replica counts, neutralises HPAs, pauses rollouts, cordons nodes (no client-go dependency — raw REST only)
- **Sidecar API** — lightweight `/flags` endpoint apps query per-request to get current feature state
- **Bounded write queue** — TTL-evicting buffer for deferred writes during degraded tiers
- **Extended collector** — CPU, network throughput, and TCP retransmit rate via `/proc` (Linux, build tag `ebpf`)
- **Prometheus metrics** — tier transitions, signal values, gate outcomes, queue depth
- **Structured audit log** — NDJSON event stream for all tier transitions and gate decisions
- **Live demo** — interactive browser UI for signal injection and tier simulation

---

## Quick Start

### Prerequisites

- Go ≥ 1.24
- Docker (for the demo container)
- `make` (optional, all commands are in the Makefile)

### Build & run locally

```bash
git clone https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract.git
cd Intent-Driven-Degradation-Contract

# Build all binaries
make build

# Validate the example policy
./dg validate config/example-degradation.yaml

# Compile to a .dg bundle
./dg compile config/example-degradation.yaml -o policy.dg

# Inspect the compiled bundle
./dg inspect policy.dg

# Simulate a signal scenario instantly (no engine required)
./dg simulate config/example-degradation.yaml \
    --signal rps:2500 --signal db_latency_p99:500

# Run the full engine (needs policy.dg)
./dg-engine --policy policy.dg --config config/dg-engine-dev.yaml
```

### Run the interactive demo

```bash
# Start everything in one container
docker compose up

# Or via Docker directly
docker build -t iddc-demo .
docker run -p 8080:8080 iddc-demo
```

Open `http://localhost:8080` in your browser. Click any scenario button to inject signals and watch the tier change in real-time.

---

## Project Structure

```
.
├── cmd/
│   ├── dg/             — CLI: validate, compile, inspect, graph, simulate, version
│   ├── dg-engine/      — Runtime engine (signal bus + evaluator + enforcement)
│   └── demo-app/       — Interactive e-commerce demo with live tier UI
├── pkg/
│   ├── audit/          — Structured NDJSON audit logger
│   ├── collector/      — /proc metric collector (+ eBPF variant, build tag: linux && ebpf)
│   ├── compiler/       — 4-stage policy compiler
│   ├── config/         — dg-engine.yaml loader with env-var substitution
│   ├── enforcement/
│   │   ├── fallback/   — Static 503 fallback handler (survival tier)
│   │   ├── k8s/        — Kubernetes REST client + tier reconciler
│   │   ├── sidecar/    — Feature-flag HTTP server (/flags, /tier, /override)
│   │   └── writequeue/ — Bounded TTL write buffer
│   ├── gate/           — Human gate (Slack, PagerDuty, callback server)
│   ├── metrics/        — Prometheus metric registrations
│   ├── policy/         — DSL types + YAML parser
│   ├── runtime/        — Signal bus, tier evaluator, dispatcher, engine
│   └── sigfetch/       — Prometheus + HTTP + injected signal fetchers
├── config/             — Example policies and engine configs
├── charts/dg/          — Helm chart skeleton
├── docker/             — Grafana dashboard + Prometheus config
├── Dockerfile          — Multi-stage build (all binaries + policy.dg)
└── start.sh            — Container entrypoint
```

---

## Architecture

```
degradation.yaml
      │
      ▼
 [dg compile]
      │  4 stages:
      │  1. parse YAML → policy.DegradationPolicy
      │  2. validate (tier gaps, undeclared signals, contradictions)
      │  3. blast-radius analysis (cycle detection)
      │  4. emit gob-encoded .dg bundle
      │
      ▼
  policy.dg  ──────────────────────────────────────────────────────────────────►
                                                                                │
                          ┌──────────────────────────────────────────────────┐  │
                          │                 Runtime Engine                    │  │
                          │                                                   │  │
                          │   SignalBus ──► TierEvaluator ──► Dispatcher      │  │
                          │     │              (hysteresis)        │          │  │
                          │   [Prometheus / HTTP / proc fetchers]  │          │  │
                          │                                        │          │  │
                          └────────────────────────────────────────┼──────────┘  │
                                                                   │
                          ┌────────────────────────────────────────┼──────────────┐
                          │              Tier Transition Handler    │              │
                          └──────────────────┬──────────┬──────────┴──────────────┘
                                             │          │          │
                               ┌─────────────┘  ┌───────┘  ┌──────┘
                               ▼                ▼          ▼
                         K8s Controller    Sidecar     Write Queue
                         • patch replicas  • /flags    • buffer writes
                         • lock/restore    • /tier     • drain on recover
                           HPA bounds      • /health
                         • pause rollouts
                         • cordon nodes
                         (survival tier)
```

**Human Gate** fires asynchronously for any tier that declares `human_gate:`. It sends a Slack Block Kit message and/or PagerDuty incident, then blocks until an operator responds via the callback API (`POST /gate/approve`, `/gate/override`, `/gate/escalate`) or the wait timeout elapses.

---

## Compiler Diagnostics

The compiler catches problems that become 3 AM incidents:

```
ERROR  [tier gap]      No tier covers RPS range 800–1200; signals in that range
                       will skip directly from warm → critical.
ERROR  [undeclared]    Tier 'critical' references signal 'db_latency_p99' not
                       declared in the signals block.
ERROR  [contradiction] 'payment-service' appears in both cascade_to and
                       isolation_boundary — these are mutually exclusive.
ERROR  [cycle]         Circular blast-radius dependency: A → B → A
WARN   [ttl-sla]       Write queue TTL (30s) is shorter than DB recovery SLA
                       (120s). Queued writes may be evicted before replay.
```

Integrate into CI:

```yaml
- name: validate policy
  run: ./dg validate config/degradation.yaml
```

---

## Signal Sources

| Source | Config | Notes |
|--------|--------|-------|
| Prometheus | `source: prometheus` + engine `prometheus_url` | PromQL query per signal |
| HTTP endpoint | `source: http`, `url:`, `field:` | GETs JSON, extracts named field |
| Injected (dev/test) | default | `POST /inject` on engine port 9191 |
| `/proc` (Linux) | `source: proc` | mem pressure from `/proc/meminfo` |
| Extended (`linux && ebpf`) | build tag | CPU, net KB/s, TCP retransmit rate |

---

## Deployment (Kubernetes)

```bash
# 1. Compile the policy
./dg compile degradation.yaml -o policy.dg

# 2. Create secrets
kubectl create secret generic dg-secrets \
  --from-literal=SLACK_WEBHOOK_URL=https://hooks.slack.com/... \
  --from-literal=PAGERDUTY_ROUTING_KEY=<key> \
  --from-literal=GATE_AUTH_TOKEN=$(openssl rand -hex 16)

# 3. Store the compiled policy
kubectl create configmap dg-policy --from-file=policy.dg

# 4. Deploy via Helm
helm install degradation-engine ./charts/dg --set service=api-gateway

# 5. Verify the sidecar is serving flags
kubectl exec -it deploy/api-gateway -c dg-sidecar -- \
  curl http://localhost:8081/flags
```

Your application queries the sidecar before rendering features:

```go
// In your application handler
resp, _ := http.Get("http://localhost:8081/flags")
var flags struct {
    Tier     string          `json:"tier"`
    Features map[string]bool `json:"features"`
}
json.NewDecoder(resp.Body).Decode(&flags)

if flags.Features["recommendations"] {
    // serve personalised recommendations
}
```

---

## Configuration Reference

`config/dg-engine.yaml`:

```yaml
signal_bus:
  tick_interval: 2s
  stale_signal_timeout: 30s

evaluator:
  up_hysteresis: 30s      # signal must breach for 30s before tier changes up
  down_hysteresis: 90s    # signal must recover for 90s before tier drops back

gate:
  slack_webhook_url: ${SLACK_WEBHOOK_URL}
  callback_base_url: https://your-engine.internal:9292
  auth_token: ${GATE_AUTH_TOKEN}
  pagerduty_routing_key: ${PAGERDUTY_ROUTING_KEY}

k8s:
  namespace: production
  deployment_name: api-gateway
  target_warm: 6          # minimum replicas in warm tier
  max_pods: 20            # replicas to set in hot tier

metrics:
  prometheus_port: 9090
  prometheus_url: http://prometheus:9090

audit:
  output: /var/log/iddc-audit.ndjson
```

---

## API Reference

| Endpoint | Method | Description |
|----------|--------|-------------|
| `:8081/flags` | GET | Feature flags for the current tier |
| `:8081/tier` | GET | Current tier state + signal snapshot |
| `:8081/override` | POST | Force a tier override (requires auth token) |
| `:9191/inject` | POST | Inject signal values (dev/test) |
| `:9191/signals` | GET | View current signal values |
| `:9191/write` | POST | Enqueue a write (when queue is active) |
| `:9191/write/depth` | GET | Queue depth + drop count |
| `:9191/override` | POST | Lock the evaluator to a tier for N minutes |
| `:9292/gate/approve` | POST | Operator approve a pending gate |
| `:9292/gate/override` | POST | Operator override target tier |
| `:9292/gate/escalate` | POST | Escalate to survival tier |
| `:9292/gate/status` | GET | Current gate state |
| `:9090/metrics` | GET | Prometheus metrics |

---

## Running Tests

```bash
make test           # run all tests with race detector
make test-cover     # generate coverage report
make vet            # go vet all packages
make lint           # golangci-lint
```

---

## Contributing

Contributions are welcome and appreciated. IDDC is an open project — whether you're fixing a bug, adding an integration, or improving the docs, your work matters.

### Getting started

1. **Fork** the repository and clone your fork:
   ```bash
   git clone https://github.com/<your-username>/Intent-Driven-Degradation-Contract.git
   ```
2. **Create a branch** for your change:
   ```bash
   git checkout -b feat/your-feature-name
   ```
3. **Make your changes**, add tests, and verify everything passes:
   ```bash
   make test && make vet
   ```
4. **Open a pull request** against `main`. Describe what you changed and why.

### Guidelines

- Keep pull requests focused — one logical change per PR
- Add tests for any new behaviour (aim to maintain or improve coverage)
- Follow existing code style — run `gofmt` and `go vet` before committing
- Update documentation if you change a public API or add a new feature
- For large changes, open an issue first to discuss the approach

### Reporting issues

Please file issues on [GitHub Issues](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/issues) with:
- A clear description of the problem or feature request
- Steps to reproduce (for bugs)
- Expected vs actual behaviour

---

## Roadmap & Contribution Opportunities

The following enhancements are planned and open for contributors. Pick one, open an issue to claim it, and go.

### Core engine

1. **OTel trace context propagation through tier transitions**  
   Emit OpenTelemetry spans for every tier transition and gate event. Each `TierTransition` becomes a span with signal values as attributes, enabling trace-based root cause analysis in Jaeger, Tempo, or any OTel-compatible backend.

2. **eBPF-native signal collection (no /proc polling)**  
   Replace the current `/proc` delta-sampling in `collector_ebpf.go` with actual eBPF programs attached to kernel tracepoints. Use `cilium/ebpf` or `libbpf-go` for CO-RE portability. Candidates: TCP retransmit events, scheduler latency (BPF_PROG_TYPE_TRACEPOINT), and memory reclaim pressure.

3. **Multi-signal hysteresis — require N of M signals to confirm a tier**  
   The current evaluator advances on the first matching tier condition. Add a `quorum:` field to tier specs so an operator can require, e.g., 2 out of 3 signals to sustain the threshold before a transition confirms.

4. **WASM policy evaluation sandbox**  
   Compile the condition evaluator to WebAssembly so policies can be evaluated in any environment (browser, edge, non-Go services) without deploying the full engine. Useful for client-side preview and multi-language SDK support.

### Observability integrations

5. **Grafana dashboard as code + alert rules**  
   Extend `docker/grafana/` with a complete, importable Grafana dashboard covering all IDDC metrics: tier history timeline, signal sparklines, gate outcomes, queue depth, and transition heatmap. Include pre-built Grafana alert rules for stuck gates and queue overflow.

6. **Datadog integration — tier as a service-level objective**  
   Add a `DatadogSLOReporter` that maps IDDC tier history to a Datadog SLO. When the tier is `nominal` the SLO accrues uptime; degraded tiers accrue error budget. Expose tier as a Datadog service tag so it appears automatically in APM service maps.

7. **OpenTelemetry Collector receiver**  
   Build an OTLP-compatible receiver so any OTel-instrumented service can push its metrics directly to IDDC as signal values, without a Prometheus intermediary. Enables IDDC adoption in services that already export metrics via OTel SDKs.

8. **Loki log annotation on tier transitions**  
   On each `TierTransition`, push a log annotation to Grafana Loki so engineers can correlate application log spikes with tier changes on a single timeline — similar to Grafana's annotation API but via the Loki push endpoint.

9. **Elastic APM / Kibana integration**  
   Publish tier transition events to Elasticsearch as structured documents, with a pre-built Kibana index pattern and dashboard. Enables IDDC visibility for teams using the ELK stack rather than Prometheus/Grafana.

10. **AWS CloudWatch Metrics sink**  
    Add a `CloudWatchReporter` that publishes the active tier and signal values as CloudWatch custom metrics. Allows IDDC-driven Auto Scaling policies and CloudWatch Alarms to react to tier state.

### Policy and workflow

11. **PagerDuty service dependency map export**  
    Read the `blast_radius.depends_on` / `cascade_to` graph from a compiled `.dg` bundle and push it to the PagerDuty Services API as a technical service dependency map. Gives on-call engineers a live dependency view that matches the declared IDDC topology.

12. **GitHub Actions policy diff comment**  
    Build a GitHub Actions workflow that runs the compiler on both base and head branches, diffs the compiled bundles, and posts a PR comment summarising what changed: new tiers, modified thresholds, added signals, removed gates. Makes policy reviews as readable as code reviews.

13. **ArgoCD / Flux GitOps plugin**  
    Implement a Kubernetes Custom Resource Definition (`DegradationPolicy`) and a controller that watches for policy changes in Git, recompiles, and hot-reloads the engine without downtime. Enables full GitOps for degradation policies.

14. **Slack interactive component approvals**  
    Replace the current Slack plain-text instructions with Slack Block Kit interactive buttons (`/gate/approve`, `/gate/override`). The operator clicks a button in Slack; the Slack interactivity endpoint calls back to IDDC. No copy-pasting curl commands during an incident.

15. **Policy language server (LSP)**  
    Implement a Language Server Protocol server for `degradation.yaml` files. Provides schema validation, autocomplete for signal IDs and tier names, and inline diagnostics in VS Code and other LSP-compatible editors as you type — before you even run the compiler.

---

## License

MIT — see [LICENSE](LICENSE).

## Maintainer

**Saqlain Ahmed** ([@SAQLAINAP](https://github.com/SAQLAINAP))  
Pull requests, issues, and forks are all welcome.
