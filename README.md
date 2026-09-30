# IDDC: Intent-Driven Degradation Contracts

[![CI](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/actions/workflows/ci.yml/badge.svg)](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/SAQLAINAP/Intent-Driven-Degradation-Contract)](https://goreportcard.com/report/github.com/SAQLAINAP/Intent-Driven-Degradation-Contract)
[![Go Version](https://img.shields.io/badge/go-1.26-blue)](go.mod)

IDDC is a declarative failure-topology framework for distributed systems. You define how your service degrades under stress at design time, before anything fails.

![IDDC console with a human gate holding the hot → critical transition](docs/screenshots/console-gate.png)

**Contents:**
[The problem](#the-problem) ·
[How it works](#how-it-works) ·
[Quick start](#quick-start) ·
[Screenshots](#screenshots) ·
[The console](#the-console-gui) ·
[Writing a policy](#writing-a-policy) ·
[CLI](#cli-reference-dg) ·
[Runtime behaviour](#runtime-behaviour) ·
[Configuration](#configuration-dg-engineyaml) ·
[API](#api-reference) ·
[Testing](#testing) ·
[Repository layout](#repository-layout) ·
[Known limitations](#known-limitations) ·
[Contributing](#contributing)

---

## The problem

Service degradation is usually reactive. Engineers find out what their system does under load at 3 AM, mid-incident, with customers affected. Runbooks go stale, on-call responders make inconsistent decisions, and features get switched off in a different order every time.

IDDC turns that into a contract:

1. You write a `degradation.yaml` next to your `deployment.yaml`.
2. `dg compile` checks it for gaps, contradictions and cycles in CI, then emits a `.dg` bundle.
3. `dg-engine` enforces the bundle continuously. As signals cross thresholds it switches feature flags, buffers writes, and adjusts Kubernetes replicas.
4. Risky transitions wait behind a **human gate** until an operator approves them or the timeout policy takes over.

## How it works

```text
degradation.yaml ──► dg compile ──► policy.dg ──► dg-engine
                     parse, validate,             │
                     blast radius, emit           ├─ signal bus    Prometheus / HTTP / /proc / injected
                                                  ├─ evaluator     most degraded matching tier + hysteresis
                                                  ├─ enforcer      applies tiers; human gate holds risky ones
                                                  │    ├─ sidecar :8081   /flags for your app
                                                  │    ├─ write queue      buffer writes in read-only tiers
                                                  │    └─ k8s reconciler   replicas / HPA / rollouts / cordon
                                                  ├─ control API + web console :9191
                                                  ├─ gate callbacks :9292  (Slack / PagerDuty / curl)
                                                  └─ Prometheus metrics :9090, NDJSON audit log
```

The engine follows five rules:

| Rule | What happens |
|------|--------------|
| **Tier order** | Tiers are listed from least to most degraded. On every tick the engine picks the **most degraded tier whose condition matches**. If nothing matches, it uses the first tier. |
| **Hysteresis** | A new tier must stay matched for `up_hysteresis` (degrading) or `down_hysteresis` (recovering) before the transition is confirmed. This prevents flapping. |
| **Human gate** | A move *up* into a tier with `human_gate:` is **held**. Apps stay on the current tier until an operator answers or `wait` expires (see [Human gate](#human-gate)). Recovery (moving down) is never gated. |
| **Feature flags** | Come from the policy. A `full_service` tier enables every feature except its `disable:` list. `read_only` and `static_fallback` tiers disable everything. |
| **Write queue** | Tiers with `queue_writes:` buffer writes (`POST /write`). Leaving such a tier drains the queue. |

---

## Quick start

**Prerequisites:** Go 1.26+, `make`. Docker is optional; `curl` and `python3` are needed only for the smoke test.

```bash
git clone https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract.git
cd Intent-Driven-Degradation-Contract

make demo            # builds into bin/, compiles the example policy, starts engine + demo shop
```

Run `make` on its own to list every target.

| Open | What it is |
|------|------------|
| http://localhost:9191/ | **IDDC console**: tier ladder, signals, gate, flags, queue, overrides, policy lab |
| http://localhost:8080/ | **Demo shop**: a toy e-commerce app that obeys the sidecar's flags |

Port 8080 busy? Run `make demo DEMO_PORT=8088`.

To run it with Docker instead:

```bash
make docker-run      # = docker build -f deploy/docker/Dockerfile -t iddc-demo . && docker run -p 8080:8080 -p 9191:9191 iddc-demo
```

### A two-minute tour

1. In the console, click **Simulate** on the `warm` row. After about 3s of dev hysteresis, the enforced tier turns `warm` and `recommendations`/`analytics` switch off. The demo shop's feature table follows.
2. Click **Simulate** on `critical`. The gate opens, but the enforced tier **stays `warm`**: the console shows `warm → critical` with a countdown, and the shop keeps serving full checkout.
3. Click **Approve critical**. The tier becomes `critical` (`read_only`). Checkout in the demo shop now returns `202 queued`, and the write queue fills.
4. Click **Simulate** on `nominal` (or **Reset all to 0**). The service recovers and the queue drains (`queue_drained` shows in the event log).
5. Click **Simulate** on `survival`, then **Escalate**. The shop now serves its static 503 fallback page.
6. Use **Manual override → nominal → Force & lock**, then **Release lock**, to hand control back to the signals.

The same flow works with curl, for scripts and CI:

```bash
curl -XPOST localhost:9191/inject -d '{"rps": 2500}'   # propose critical (gated)
curl localhost:9292/gate/status                        # {"state":"pending","tier":"critical",...}
curl -XPOST localhost:9292/gate/approve                # apply it
curl localhost:8081/flags                              # {"tier":"critical","mode":"read_only",...}
```

---

## Screenshots

Captured from a live engine + demo shop session (the console was driven by clicking through it in headless Chrome) and from real terminal runs.

| | |
|---|---|
| **Console, `hot` tier.** Ladder, signals, flags and the event log (the gate view is at the top of this page).<br>![console overview](docs/screenshots/console-overview.png) | **After approving the gate.** `critical` is enforced (read-only), features are off, writes are buffered.<br>![console in critical](docs/screenshots/console-critical.png) |
| **Demo shop in `critical`.** Reacts to the sidecar's mode and flags.<br>![demo shop critical](docs/screenshots/demo-shop-critical.png) | **Demo shop in `survival`.** Static 503 fallback page.<br>![demo shop survival](docs/screenshots/demo-shop-survival.png) |
| **Policy lab.** Validates pasted YAML and flags a tier gap.<br>![policy lab](docs/screenshots/console-policy-lab.png) | **`make smoke`.** 73 end-to-end checks.<br>![smoke test output](docs/screenshots/terminal-smoke.png) |

Terminal snaps: [`dg` diagnostics and simulate](#compiler-diagnostics), and [a `dg-engine` session with a gate held and then approved](#human-gate).

---

## The console (GUI)

The engine embeds a single-page console with no build step and no extra process. It's served on the control port (`:9191` by default) and polls the JSON API every second.

| Panel | What you can do |
|-------|-----------------|
| **Header** | Shows the enforced tier. Also shows *"⏸ X awaiting gate"* while a transition is held, and a 🔒 badge while the evaluator is locked. |
| **Tier ladder** | Each tier's condition, mode, disabled features, queue and gate settings. **Simulate** injects a verified set of signal values that lands exactly on that tier. |
| **Signals** | Slider and number input per signal, showing the live value the evaluator sees. Signals collected from live sources are read-only. |
| **Human gate** | Countdown, *from → to* tiers, and the on-timeout action. Buttons for **Approve**, **Escalate**, and **Override to** *tier* for *N* minutes. Optional auth token field. |
| **Feature flags** | Exactly what `GET :8081/flags` returns to your applications right now. |
| **Write queue** | Depth, capacity and drops. **Enqueue test write** is only enabled in queueing tiers. |
| **Manual override** | Force and lock any tier for N minutes; release the lock. |
| **Event log** | The in-memory tail of the audit log: transitions, gate events, overrides, queue drains. |
| **Policy lab** | Paste any `degradation.yaml` to validate it or simulate signal values against it, without deploying. |

> The control port can inject signals and override tiers, so treat it like an admin interface. Keep it on an internal network.

---

## Writing a policy

```yaml
service: api-gateway
version: "1.0.0"

signals:
  - id: rps
    source: prometheus
    query: sum(rate(http_requests_total{service="api-gateway"}[1m]))   # optional; defaults exist for common ids
    poll_cadence: 2s
  - id: db_latency_p99
    source: prometheus
  - id: mem_pressure
    source: proc                 # /proc/meminfo on Linux
  - id: queue_lag
    source: http
    url: http://worker:8080/stats
    field: lag                   # JSON field holding a number

tiers:                           # least → most degraded
  - name: nominal
    when: { rps: { lt: 800 } }
    behavior: { mode: full_service }

  - name: warm
    when: { rps: { gte: 800, lt: 1500 } }          # a comparator may combine a lower and an upper bound
    behavior:
      mode: full_service
      disable: [recommendations, analytics]

  - name: critical
    when:
      any_of:                                      # OR; all_of = AND; flat keys in one map are ANDed
        - rps: { gte: 2000 }
        - db_latency_p99: { gte: 2000 }
    human_gate:
      notify: ["slack:#incidents", "pagerduty"]
      wait: 45s
      on_timeout: execute_policy                   # execute_policy | hold | escalate
    behavior:
      mode: read_only
      queue_writes: { max_depth: 2000, ttl: 120s }

  - name: survival
    when: { mem_pressure: { gte: 0.95 } }
    behavior: { mode: static_fallback }

blast_radius:
  depends_on: [auth-service, user-db]
  cascade_to: [notification-service]
  isolation_boundary: payment-service
  recovery_sla_seconds: 120
```

| Field | Meaning |
|-------|---------|
| `signals[].source` | `prometheus` (needs `metrics.prometheus_url`), `http` (`url` + optional `field`), or `proc` (Linux). If a source can't be collected, the signal falls back to injected values. |
| comparators | `lt`, `lte`, `gt`, `gte`, `eq`. Combine a lower and an upper bound for a range. |
| `behavior.mode` | `full_service` (default), `read_only` (all flags off, writes queued if `queue_writes`), or `static_fallback` (all flags off, apps serve a 503 page). |
| `behavior.disable` | Feature names turned off in this tier. The union of every tier's list is the feature set the sidecar serves. |
| `human_gate.wait` | How long an operator has to respond. |
| `human_gate.on_timeout` | `execute_policy` applies the tier. `hold` stays on the current tier and locks it for 10 minutes. `escalate` jumps to the most degraded tier. |

### Compiler diagnostics

`dg validate` fails the build on errors and prints warnings. Every example below is in [`examples/demo-set/`](examples/demo-set):

| Category | Severity | Caught when… | Demo |
|----------|----------|--------------|------|
| `tier gap` | ERROR | A single-signal ladder leaves a range uncovered, including values above the top tier | `02-tier-gap.yaml` |
| `undeclared` | ERROR | A tier references a signal missing from `signals:` | `03-undeclared-signal.yaml` |
| `contradiction` | ERROR | A service is both `cascade_to` and `isolation_boundary`, or a comparator range is empty (`gte: 2000, lt: 1500`) | `04`, `09` |
| `cycle` | ERROR | The blast-radius graph has a cycle | `05-cycle.yaml` |
| `structure` | ERROR | Duplicate or missing tier name, or no tiers at all | `09-shape-errors.yaml` |
| `behavior` / `gate` | ERROR | Unknown `mode`, unknown `on_timeout`, or a gate without a positive `wait` | `09-shape-errors.yaml` |
| `ttl-sla` | WARN | Write-queue TTL is shorter than `recovery_sla_seconds` | `06-ttl-warning.yaml` |

![dg validate and simulate in a terminal](docs/screenshots/terminal-cli.png)

```text
$ dg validate examples/demo-set/02-tier-gap.yaml
  ERROR   [tier gap] No tier covers rps in [800, 1200) — values there fall back to tier "nominal".
Compilation failed with 1 error(s), 0 warning(s).

$ dg validate examples/demo-set/09-shape-errors.yaml
  ERROR   [structure] Tier "nominal" is declared more than once.
  ERROR   [behavior] Tier "nominal" has unknown mode "readonly" (want full_service, read_only or static_fallback).
  ERROR   [gate] Tier "critical" has unknown human_gate.on_timeout "panic" (want execute_policy, hold or escalate).
  ERROR   [contradiction] Tier "critical": the comparator on signal "rps" can never be true (lower bound is above upper bound).
```

Add it to CI:

```yaml
- name: validate degradation policy
  run: go run ./cmd/dg validate config/degradation.yaml
```

After `make build`, the binary is `bin/dg`.

---

## CLI reference (`dg`)

| Command | Description |
|---------|-------------|
| `dg validate <policy.yaml>` | Run every compiler check; exit 1 on errors |
| `dg compile <policy.yaml> [-o out.dg]` | Validate and emit the gob-encoded bundle (default `<policy>.dg`) |
| `dg inspect <bundle.dg>` | Print the service, checksum, tier ladder and signal sources |
| `dg graph <bundle.dg>` | Print the blast-radius tree |
| `dg simulate <policy.yaml> --signal id:value …` | Show the tier the engine would converge to, without hysteresis |
| `dg version` | Print the version |

```text
$ dg simulate config/example-degradation.yaml --signal rps:2500
Signals: map[rps:2500]
Matched tier: critical
  mode: read_only
  human gate: wait=45s on_timeout=execute_policy (the engine holds the transition until decided)
```

---

## Runtime behaviour

### Human gate

When the evaluator proposes moving *up* into a gated tier, the enforcer keeps the current tier applied and opens the gate. It sends Slack Block Kit and PagerDuty notifications if configured, and prints curl instructions to the terminal otherwise. Only one gate is open at a time.

| Operator action / event | Result |
|-------------------------|--------|
| `POST /gate/approve` | Gated tier is applied |
| `POST /gate/override {"target_tier","duration_min"}` | Target tier is applied and the evaluator is locked to it for `duration_min` (default 10) |
| `POST /gate/escalate` | Most degraded tier is applied and locked for 10 minutes |
| timeout + `execute_policy` | Gated tier is applied |
| timeout + `hold` | Current tier stays, locked for 10 minutes. The gate re-opens later if signals still demand it |
| timeout + `escalate` | Most degraded tier is applied and locked |
| signals move elsewhere first | The gate is **superseded** and the new proposal is handled normally |

Gate routes are served on the gate port (`:9292`) and mirrored on the control port for the console. If `gate.auth_token` is set, every response must include `"token": "…"`.

![engine log: transition, gate held, approval](docs/screenshots/terminal-engine.png)

### Integrating your application

Your application asks the sidecar for flags on each request (or caches them for a second):

```go
resp, _ := http.Get("http://localhost:8081/flags")
var flags struct {
    Tier     string          `json:"tier"`
    Mode     string          `json:"mode"`     // full_service | read_only | static_fallback
    Features map[string]bool `json:"features"`
}
json.NewDecoder(resp.Body).Decode(&flags)

switch {
case flags.Mode == "static_fallback":
    // serve a static 503 page
case flags.Mode == "read_only":
    // serve cached reads; POST writes to http://engine:9191/write
case flags.Features["recommendations"]:
    // serve personalised recommendations
}
```

[`cmd/demo-app`](cmd/demo-app/main.go) is a complete example that reacts only to `mode` and `features`, never to tier names.

### Kubernetes enforcement

If `k8s.deployment_name` is set, every applied tier is reconciled through the Kubernetes REST API directly (no client-go):

- `warm`: scale to at least `target_warm`
- `hot`: scale to `max_pods` and pin the HPA
- `critical`: pause rollouts
- `survival`: pause rollouts and cordon nodes

These actions are keyed on those four tier names (see [Known limitations](#known-limitations)).

### Observability

| Metric | Type | Notes |
|--------|------|-------|
| `dg_current_tier{service,tier}` | gauge | One series per service: the **enforced** tier's ladder index (0 = least degraded) |
| `dg_signal_value{service,signal_id}` | gauge | Latest sampled value |
| `dg_tier_transitions_total{service,from_tier,to_tier}` | counter | Applied transitions |
| `dg_gate_events_total{service,outcome}` | counter | `approved` · `overridden` · `escalated` · `timed_out` · `superseded` |
| `dg_write_queue_depth{service}` / `dg_write_queue_dropped_total{service}` | gauge / counter | Write queue |

The audit log (stdout or a file, NDJSON) records `tier_transition` (with a `reason`), `gate_fired`, `gate_approved`, `gate_overridden`, `gate_escalated`, `gate_timed_out`, `gate_superseded`, `tier_override`, and `queue_drained`. `make observability-up` starts Prometheus (:9091) and a provisioned Grafana dashboard (:3000, admin/admin) from [`deploy/observability/`](deploy/observability).

---

## Configuration (`dg-engine.yaml`)

`${ENV_VAR}` placeholders are expanded at load time; unset variables become empty, which disables the integration that uses them. Every key is optional.

```yaml
server:
  control_addr: ":9191"      # control API + web console
  sidecar_addr: ":8081"      # feature flags for apps
  gate_addr: ":9292"         # gate callbacks

signal_bus:
  tick_interval: 2s
  stale_signal_timeout: 30s
  mode: auto                 # auto = declared sources; inject = everything via POST /inject

evaluator:
  up_hysteresis: 30s         # a degraded tier must hold this long before it's applied
  down_hysteresis: 90s       # recovery must hold this long

gate:
  slack_webhook_url: ${SLACK_WEBHOOK_URL}
  callback_base_url: https://dg.internal:9292
  auth_token: ${GATE_AUTH_TOKEN}
  pagerduty_routing_key: ${PAGERDUTY_ROUTING_KEY}

metrics:
  prometheus_port: 9090
  prometheus_url: http://prometheus:9090   # needed for source: prometheus

audit:
  output: stdout             # or a file path

k8s:
  namespace: production
  deployment_name: api-gateway   # empty = Kubernetes enforcement off
  target_warm: 6
  max_pods: 20
```

[`config/dg-engine-dev.yaml`](config/dg-engine-dev.yaml) uses 3s/5s hysteresis and `mode: inject` for demos. Don't use it in production.

---

## API reference

| Endpoint | Method | Description |
|----------|--------|-------------|
| `:9191/` | GET | Web console |
| `:9191/api/status` | GET | Enforced tier, proposed tier, lock, gate, flags, signals, queue |
| `:9191/api/policy` | GET | Tier ladder with human-readable conditions, verified scenarios, signal ranges |
| `:9191/api/events?n=50` | GET | Recent audit events, newest first |
| `:9191/api/validate` | POST | Body: policy YAML. Returns diagnostics |
| `:9191/api/simulate` | POST | `{"yaml": "…optional…", "signals": {…}}` → matched tier, mode and flags |
| `:9191/inject` | POST | `{"rps": 2500}`. Rejects undeclared or live-collected signals with 400 |
| `:9191/signals` | GET | Injected values |
| `:9191/write` | POST | `{"id","payload"}`. 409 unless the enforced tier queues writes |
| `:9191/write/depth` | GET | Depth, drops, active |
| `:9191/override` | POST | `{"tier","duration_min"}`. Force and lock a tier (400 for unknown tiers) |
| `:9191/override/release` | POST | Release the lock |
| `:9191/gate/*`, `:9292/gate/*` | — | `POST approve` · `POST override` · `POST escalate` · `GET status` |
| `:8081/flags` | GET | `{"tier","mode","features"}` for apps |
| `:8081/tier` | GET | Current tier, mode, since, signals |
| `:8081/health` | GET | Liveness |
| `:8081/override` | POST | `{"tier","token"}`. Sidecar-only override (bypasses the engine) |
| `:9090/metrics` | GET | Prometheus metrics |

---

## Testing

```bash
make vet          # go vet
make test         # unit tests with the race detector
make test-cover   # the same, with a coverage summary
make smoke        # end-to-end smoke test (needs curl + python3)
```

A full run is pictured in [Screenshots](#screenshots).

`scripts/smoke.sh` runs 73 checks against real binaries:

- **CLI:** `dg validate` against every policy in `examples/demo-set/` (exit codes and exact diagnostics), `compile`/`inspect`/`graph`, corrupt-bundle handling, and a `simulate` matrix.
- **Runtime:** it starts `dg-engine` and the demo app, then walks the whole ladder over HTTP:
  - input validation
  - hysteresis
  - the gate holding a transition, then approve, timeout, override, lock release and escalate
  - write queueing and draining
  - the demo shop's read-only and 503 behaviour
  - manual overrides keeping the sidecar in sync
  - metrics hygiene

CI runs vet, unit tests, the smoke test, and example-policy validation on every push.

---

## Deployment on Kubernetes

```bash
dg compile degradation.yaml -o policy.dg
kubectl create configmap dg-policy --from-file=policy.dg
kubectl create secret generic dg-secrets \
  --from-literal=SLACK_WEBHOOK_URL=https://hooks.slack.com/... \
  --from-literal=PAGERDUTY_ROUTING_KEY=<key> \
  --from-literal=GATE_AUTH_TOKEN=$(openssl rand -hex 16)
```

Run `dg-engine --policy /etc/dg/policy.dg --config /etc/dg/dg-engine.yaml` as a sidecar container. Your app then reads `http://localhost:8081/flags`. A Helm chart skeleton lives in `deploy/helm/dg`.

---

## Known limitations

These are known gaps, and good first contributions:

- **Gap detection** covers ladders where every tier compares the same single signal. Compound (`all_of`/`any_of`) ladders aren't analysed, so a hole like `rps=1600, pod_ceiling=0.1` in the example policy (which falls back to `nominal`) isn't reported. Use `dg simulate` or the policy lab to check those.
- **Kubernetes actions** are keyed on the tier names `warm`, `hot`, `critical` and `survival`. Other names get no replica changes. The reconciler, Slack and PagerDuty paths are not covered by automated tests (they need a cluster or credentials).
- **Drained writes** are counted and logged but not replayed. Replaying them against your backend is left to the integrator (see `enforcer.applyLocked`).
- **`source: proc`** supports only `mem_pressure`, and only on Linux. Elsewhere it falls back to injection.
- Hysteresis windows apply to every tier, and only one gate can be open at a time.

---

## Repository layout

```text
.
├── cmd/                     entry points
│   ├── dg/                  CLI: validate, compile, inspect, graph, simulate, version
│   ├── dg-engine/           runtime engine, control API (control.go), web console (console.html, embedded)
│   └── demo-app/            demo shop that obeys the sidecar and uses the write queue
├── pkg/                     libraries
│   ├── policy/              DSL types + YAML loader
│   ├── compiler/            validation, blast-radius analysis, bundle emit
│   ├── runtime/             signal bus, tier evaluator (MatchTier), dispatcher, engine
│   ├── enforcement/
│   │   ├── enforcer/        applies tiers; human-gate hold / approve / timeout / supersede
│   │   ├── sidecar/         /flags server (flags derived from the policy)
│   │   ├── writequeue/      bounded TTL write buffer
│   │   ├── k8s/             Kubernetes REST client + reconciler
│   │   └── fallback/        static 503 page
│   ├── gate/                human gate, Slack, PagerDuty, callback server
│   ├── sigfetch/            Prometheus / HTTP / injected fetchers
│   ├── collector/           /proc collector (+ eBPF variant, build tag linux && ebpf)
│   └── audit/ metrics/ config/
├── config/                  example policy + engine configs (prod and dev)
├── examples/demo-set/       policies exercising every compiler diagnostic + the runtime smoke policy
├── deploy/
│   ├── docker/              Dockerfile for the demo image
│   ├── observability/       docker-compose for Prometheus + Grafana, with provisioning
│   └── helm/                Helm chart skeleton
├── scripts/
│   ├── demo.sh              starts engine + demo shop (make demo, Docker image)
│   └── smoke.sh             end-to-end smoke test (make smoke, CI)
├── docs/screenshots/        images used in this README
├── .github/workflows/       CI: vet, tests, smoke test, Docker build, policy validation
├── Makefile                 `make` lists every target
└── render.yaml              Render blueprint (must live at the root)
```

Build output (`bin/`) is gitignored. `make clean` removes it, along with binaries that older versions of the Makefile left in the root.

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
   make vet && make test && make smoke
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
   Extend `deploy/observability/grafana/` with a complete, importable Grafana dashboard covering all IDDC metrics: tier history timeline, signal sparklines, gate outcomes, queue depth, and transition heatmap. Include pre-built Grafana alert rules for stuck gates and queue overflow.

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
