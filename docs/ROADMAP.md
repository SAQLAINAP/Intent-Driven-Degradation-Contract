# Roadmap

IDDC is pre-1.0: the policy format and the HTTP APIs may still change between minor versions. Breaking changes are called out in [CHANGELOG.md](../CHANGELOG.md).

## Near term: known gaps

These are the limitations listed in the README. Each makes a good first or second contribution.

- **Compound gap analysis.** Extend `detectTierGaps` (`pkg/compiler/validator.go`) beyond single-signal ladders, for example by sampling the signal space or with interval arithmetic over `all_of`/`any_of`. Today `rps=1600, pod_ceiling=0.1` in the example policy silently falls back to `nominal`.
- **Policy-driven Kubernetes actions.** `pkg/enforcement/k8s` keys replica/HPA/rollout actions on the tier names `warm`, `hot`, `critical` and `survival`. Let tiers declare them instead (`behavior.replicas` already exists in the DSL).
- **Write replay.** Drained writes are counted but not replayed. Add a pluggable `Replayer` (HTTP webhook first).
- **Per-tier hysteresis** and **several concurrent gates**.
- **More `proc` signals.** Only `mem_pressure` is implemented (see `pkg/collector`).
- **Helm chart.** `deploy/helm/dg` runs the engine with a policy ConfigMap. Still to do: optional RBAC (Role/RoleBinding to patch the target Deployment and HPA) for Kubernetes enforcement, a sidecar-injection example, and `helm test` hooks.

## Longer term

Longer-term enhancements, open to contributors. To pick one up, open an issue saying you'd like to work on it, so nobody duplicates effort; see [CONTRIBUTING.md](../CONTRIBUTING.md).

### Core engine

1. **OTel trace context propagation through tier transitions**  
   Emit OpenTelemetry spans for every tier transition and gate event. Each `TierTransition` becomes a span with signal values as attributes, enabling trace-based root cause analysis in Jaeger, Tempo, or any OTel-compatible backend.

2. **eBPF-native signal collection (no /proc polling)**  
   Replace the current `/proc` delta-sampling in `collector_ebpf.go` with actual eBPF programs attached to kernel tracepoints. Use `cilium/ebpf` or `libbpf-go` for CO-RE portability. Candidates: TCP retransmit events, scheduler latency (BPF_PROG_TYPE_TRACEPOINT), and memory reclaim pressure.

3. **Multi-signal hysteresis — require N of M signals to confirm a tier**  
   Today a tier is confirmed once its condition holds for the hysteresis window. Add a `quorum:` field to tier specs so an operator can require, e.g., 2 out of 3 signals to sustain the threshold before a transition confirms.

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
