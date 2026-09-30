# Security policy

## Supported versions

IDDC is pre-1.0. Security fixes land on `main` and ship in the next release. Only the latest release is supported.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Report privately through GitHub: go to the repository's **Security** tab and choose **Report a vulnerability** ([direct link](https://github.com/SAQLAINAP/Intent-Driven-Degradation-Contract/security/advisories/new)). Include:

- the affected component (engine control API, gate callbacks, sidecar, compiler, CLI, Docker image, Helm chart)
- the version or commit
- steps to reproduce, and the impact you believe it has

You should get an acknowledgement within 5 working days. We'll agree a fix and disclosure timeline with you, and credit you in the release notes unless you'd rather stay anonymous.

## Deployment hardening notes

These are design properties rather than vulnerabilities, but please keep them in mind:

- **The control port (`:9191`) is an admin interface.** It can inject signals, force tiers and answer gates, and it has no authentication of its own. Bind it to localhost or an internal network, or put it behind an authenticating proxy.
- **Set `gate.auth_token`** in production. Without it, anyone who can reach the gate port can approve, override or escalate a gate.
- **`config/dg-engine-dev.yaml` is for demos only.** It enables signal injection for every signal and uses very short hysteresis.
