# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/). While the version is below 1.0, minor versions may contain breaking changes; they are marked **Breaking**.

## [Unreleased]

First public release candidate.

### Added
- Web console embedded in `dg-engine` (`:9191`): tier ladder with one-click scenarios, signal sliders, human-gate controls, feature flags, write queue, manual override, event log and a policy lab.
- Control API: `/api/status`, `/api/policy`, `/api/events`, `/api/validate`, `/api/simulate`, `/override/release`. Gate routes are mirrored on the control port.
- Human-gate semantics: upward moves into gated tiers are held until approve, override, escalate or `on_timeout` (`execute_policy`, `hold`, `escalate`). A gate is superseded if signals move elsewhere.
- Compiler checks for duplicate tiers, unknown `mode` / `on_timeout`, gates without `wait`, impossible ranges, and values above the top tier.
- `examples/demo-set/` policies covering every diagnostic; `scripts/smoke.sh` end-to-end test.
- Per-signal `query:` (PromQL), `poll_cadence`, `stale_signal_timeout`, `signal_bus.mode` (`auto` / `inject`) and configurable listen addresses.
- `dg --version`, `dg-engine --version`; release archives for Linux, macOS and Windows; `ghcr.io/saqlainap/iddc` image; Helm chart in `deploy/helm/dg`.
- LICENSE (MIT), CONTRIBUTING, CODE_OF_CONDUCT, SECURITY, issue/PR templates, Dependabot.
- README walkthrough ("A bookshop flash sale") with Mermaid diagrams; its policy lives in `examples/bookshop/` and is checked by the smoke test.

### Changed
- **Breaking:** the Go module path is now `github.com/SAQLAINAP/Intent-Driven-Degradation-Contract` (was `iddc`).
- **Breaking:** the evaluator picks the **most degraded** matching tier (it used to pick the first match in declaration order).
- **Breaking:** sidecar feature flags come from each tier's `disable:` list and mode instead of a hard-coded table. `/flags` now includes `mode`.
- **Breaking:** `dg_current_tier` exposes a single series per service: the enforced tier's ladder index.
- Repository layout: `deploy/{docker,observability,helm}`, `scripts/`, and build output in `bin/`.
- The demo app reacts to mode and flags, and buffers checkout through the engine's write queue.
- All HTTP servers set `ReadHeaderTimeout`; the Docker image runs as a non-root user.

### Fixed
- `survival` (and any tier listed after a milder, overlapping one) was unreachable.
- Tier-gap detection ignored the lower bound of range comparators such as `{gte: 1200, lt: 2000}`.
- Gate approval and timeout had no effect; `on_timeout: escalate` and override `duration_min` were ignored; gate events always said `critical`; an old gate could clear a newer one.
- `POST /override` on the control port didn't update the sidecar and accepted unknown tiers.
- `/gate/status` reported `waiting` when no gate was open.
- Unset `${ENV_VAR}` placeholders stayed literal (the gate then required the token `${GATE_AUTH_TOKEN}`).
- `/inject` accepted undeclared signals; `/write` accepted writes in every tier.
- Audit-log write failures were silently dropped.
- The CLI printed usage on runtime errors; `make run-dev` pointed at a missing bundle.
