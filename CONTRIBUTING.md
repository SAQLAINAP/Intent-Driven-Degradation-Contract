# Contributing to IDDC

Thanks for helping. Bug reports, docs fixes, new signal sources and compiler checks are all welcome. This guide covers everything you need to get a change merged.

By participating you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report a bug.** Use the [bug report form](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/issues/new?template=bug_report.yml). Please include the policy YAML, the engine config and the exact output.
- **Propose a feature.** Open a [feature request](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/issues/new?template=feature_request.yml) before writing a large change, so we can agree on the design first.
- **Pick up planned work.** See [docs/ROADMAP.md](docs/ROADMAP.md) and issues labelled `good first issue` or `help wanted`. Comment on an issue to claim it.
- **Report a vulnerability.** Please don't open a public issue; follow [SECURITY.md](SECURITY.md).

## Development setup

You need **Go 1.26+** and `make`. `curl` and `python3` are needed for the smoke test. Docker and [golangci-lint v2](https://golangci-lint.run/welcome/install/) are optional.

```bash
git clone https://github.com/<you>/Intent-Driven-Degradation-Contract.git
cd Intent-Driven-Degradation-Contract
make            # list all targets
make demo       # engine + console (:9191) + demo shop (:8080)
```

Build output goes to `bin/` (gitignored). Code layout is described in [Repository layout](README.md#repository-layout).

## Before you open a pull request

Run the same checks CI runs:

```bash
make fmt        # gofmt
make vet
make lint       # golangci-lint v2, config in .golangci.yml
make test       # unit tests with the race detector
make smoke      # end-to-end: CLI demo set + live engine, gate, queue, demo shop
```

Checklist:

- [ ] One logical change per PR, with a description of *what* and *why*.
- [ ] New behaviour has tests. Bug fixes include a regression test that fails without the fix.
- [ ] If you changed the policy format, an HTTP API, config keys or CLI output, update the README and add an entry under **Unreleased** in [CHANGELOG.md](CHANGELOG.md).
- [ ] If you changed the console (`cmd/dg-engine/console.html`), check it in a browser. Screenshots in the PR help reviewers.

## Where things go

| You want to… | Start here | Test with |
|---|---|---|
| Add a compiler diagnostic | `pkg/compiler/validator.go` (or `blast.go`) | `pkg/compiler/compiler_test.go`, plus a policy in `examples/demo-set/` and a line in `scripts/smoke.sh` |
| Change how tiers are chosen | `pkg/runtime/evaluator.go` (`MatchTier`, hysteresis) | `pkg/runtime/evaluator_test.go` |
| Change what happens on a transition or gate | `pkg/enforcement/enforcer/enforcer.go` | `pkg/enforcement/enforcer/enforcer_test.go` |
| Add a signal source | `pkg/sigfetch/`, wired in `buildFetchers` in `cmd/dg-engine/main.go` | unit test with `httptest` |
| Add a control API endpoint or console feature | `cmd/dg-engine/control.go`, `console.html` | `cmd/dg-engine/control_test.go` |
| Add a notification channel for the gate | `pkg/gate/` (see `slack.go`, `pagerduty.go`) | `pkg/gate/gate_test.go` |

## Style

- Standard Go: `gofmt`, small packages, and errors wrapped with context (`fmt.Errorf("…: %w", err)`).
- Match the comment density of the code around you. Explain *why*, not *what*.
- No new dependencies without discussion. The engine deliberately talks to Kubernetes over plain REST, without client-go.
- Commit messages: a short imperative summary (`fix: …`, `feat: …`, `docs: …` prefixes are welcome but not required), then a body explaining the reasoning when it isn't obvious.

## Releases (maintainers)

1. Move the **Unreleased** entries in `CHANGELOG.md` under a new version heading.
2. Tag and push: `git tag v0.X.Y && git push origin v0.X.Y`.
3. The [release workflow](.github/workflows/release.yml) runs the tests and publishes binaries for Linux, macOS and Windows plus checksums to GitHub Releases. It also pushes the `ghcr.io/saqlainap/iddc` image.

Dry-run the packaging locally with `make release-local VERSION=v0.X.Y` (the archives land in `dist/`).
