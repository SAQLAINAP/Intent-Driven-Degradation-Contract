# IDDC — Intent-Driven Degradation Contracts

A declarative failure-topology framework for distributed systems.
Engineers define exactly how their service behaves under stress **at design time**, before failure occurs.

## Core idea

Instead of discovering degraded behavior in production at 3 AM, you author a `degradation.yaml` alongside your `deployment.yaml`:

```yaml
tiers:
  - name: nominal
    when: { rps: { lt: 800 } }
    behavior: { mode: full_service }

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
```

The compiler validates your intent, catches gaps and contradictions at CI time, and emits a `.dg` binary that the runtime enforces continuously.

## Quick start

```bash
# Build the dg CLI
make build

# Validate a policy
./dg validate config/example-degradation.yaml

# Compile to binary
./dg compile config/example-degradation.yaml

# Inspect the compiled bundle
./dg inspect example-degradation.dg

# Simulate a signal scenario instantly (no engine required)
./dg simulate config/example-degradation.yaml \
    --signal rps:2500 --signal db_latency_p99:500

# Run all tests
make test
```

## Project structure

```
cmd/dg/             — CLI: validate, compile, inspect, graph, simulate
pkg/policy/         — DSL types + YAML parser
pkg/compiler/       — 4-stage compiler (parse → validate → blast graph → emit)
pkg/runtime/        — Signal bus + Tier evaluator + Dispatcher
pkg/gate/           — Human-in-the-loop dead man's switch (Slack)
pkg/enforcement/    — Sidecar HTTP server, K8s controller, write queue, fallback
pkg/metrics/        — Prometheus metric registrations
pkg/audit/          — Structured NDJSON audit log
pkg/collector/      — /proc and eBPF metric collectors
config/             — Example policy + runtime config
charts/dg/          — Helm chart skeleton
```

## Architecture

```
degradation.yaml
    │
    ▼
[dg compile]  ──────────────────────────► policy.dg
                                               │
                              ┌────────────────▼────────────────┐
                              │         Runtime Engine           │
                              │  SignalBus → TierEvaluator       │
                              │          → Dispatcher            │
                              └──────────────┬──────────────────┘
                                             │ TierTransition
                     ┌───────────────────────┼─────────────────────┐
                     ▼                       ▼                     ▼
              K8s Controller          App Sidecar            Write Queue
              (replica mgmt)          (/flags API)           (buffered writes)
```

## Compiler diagnostics

The compiler catches problems that would become 3 AM incidents:

```
ERROR  [tier gap]      No tier covers RPS range 800–1200.
ERROR  [undeclared]    Tier 'critical' references signal 'db_latency_p99' not declared.
ERROR  [contradiction] 'payment-service' in both cascade_to and isolation_boundary.
ERROR  [cycle]         Circular dependency: A → B → A
WARN   [ttl-sla]       Write queue TTL (30s) < DB recovery SLA (120s).
```

## Deployment (Kubernetes)

```bash
# 1. Compile
./dg compile degradation.yaml -o policy.dg

# 2. Create secrets
kubectl create secret generic dg-secrets \
  --from-literal=SLACK_WEBHOOK_URL=<url> \
  --from-literal=GATE_AUTH_TOKEN=<token>

# 3. Create ConfigMap with policy binary
kubectl create configmap dg-policy --from-file=policy.dg

# 4. Deploy via Helm
helm install degradation-engine ./charts/dg \
  --set service=api-gateway

# 5. Verify sidecar
curl http://dg-sidecar:8081/tier
curl http://dg-sidecar:8081/flags
```

## Roadmap

| Phase | Scope | Status |
|-------|-------|--------|
| 1 | Compiler MVP | ✅ Scaffolded |
| 2 | Runtime Engine | ✅ Scaffolded |
| 3 | Human Gate (Slack) | ✅ Scaffolded |
| 4 | K8s Enforcement + Sidecar | ✅ Scaffolded |
| 5 | eBPF Collector (Rust) | 🔲 Stub |
| 6 | Research Paper (IEEE/arXiv) | 🔲 Planned |
