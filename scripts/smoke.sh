#!/usr/bin/env bash
# End-to-end smoke test for IDDC.
#
#   1. builds dg, dg-engine and demo-app into a temp dir
#   2. runs the dg CLI against every policy in examples/demo-set/
#   3. starts dg-engine + demo-app and drives the whole tier ladder over HTTP,
#      including every human-gate outcome, overrides and the write queue
#
# Usage: scripts/smoke.sh            (needs go, curl, python3)
# Ports: CONTROL_PORT=9191 SIDECAR_PORT=8081 GATE_PORT=9292 METRICS_PORT=9090 DEMO_PORT=8088
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
CONTROL_PORT="${CONTROL_PORT:-9191}"
SIDECAR_PORT="${SIDECAR_PORT:-8081}"
GATE_PORT="${GATE_PORT:-9292}"
METRICS_PORT="${METRICS_PORT:-9090}"
DEMO_PORT="${DEMO_PORT:-8088}"
CTL="http://localhost:$CONTROL_PORT"
SIDE="http://localhost:$SIDECAR_PORT"
GATE="http://localhost:$GATE_PORT"
DEMO="http://localhost:$DEMO_PORT"

PASS=0
FAIL=0
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done
  wait 2>/dev/null
  if [ "$FAIL" -gt 0 ]; then echo "engine log kept at $WORK/engine.log"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

section() { printf '\n\033[1m%s\033[0m\n' "$1"; }
pass()    { PASS=$((PASS + 1)); printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail()    { FAIL=$((FAIL + 1)); printf '  \033[31m✗\033[0m %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }

# expect <description> <expected> <actual>
expect() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "expected: $2   got: $3"; fi; }

# jq-lite: jget <python expression over d> — reads JSON on stdin.
jget() { python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except Exception: print("<invalid json>"); sys.exit()
try: print(eval(sys.argv[1]))
except Exception as e: print("<%s>" % e)' "$1"; }

status() { curl -s "$CTL/api/status" | jget "$1"; }
code()   { curl -s -o /dev/null -w '%{http_code}' "$@"; }
post()   { curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$2" "$1"; }

# wait_for <description> <python expr over /api/status> <expected> [timeout_s]
wait_for() {
  local desc="$1" expr="$2" want="$3" timeout="${4:-20}" got="" i
  for ((i = 0; i < timeout * 4; i++)); do
    got="$(status "$expr")"
    [ "$got" = "$want" ] && { pass "$desc"; return 0; }
    sleep 0.25
  done
  fail "$desc" "expected: $want   got: $got (after ${timeout}s)"
}

# ─── build ───────────────────────────────────────────────────────────────────
section "Build"
cd "$ROOT"
if go build -o "$WORK/" ./cmd/dg ./cmd/dg-engine ./cmd/demo-app; then pass "go build ./cmd/..."; else fail "go build"; exit 1; fi
DG="$WORK/dg"

# ─── CLI against the demo set ────────────────────────────────────────────────
section "dg validate — demo set"
# file | expected exit code | text that must appear in the output
while IFS='|' read -r file want_code want_text; do
  out="$("$DG" validate "examples/demo-set/$file" 2>&1)"; rc=$?
  if [ "$rc" = "$want_code" ] && grep -qF -- "$want_text" <<<"$out"; then
    pass "$file → exit $rc, \"$want_text\""
  else
    fail "$file" "want exit $want_code with \"$want_text\"; got exit $rc: $(head -3 <<<"$out" | tr '\n' ' ')"
  fi
done <<'EOF'
01-valid-simple.yaml|0|is valid
02-tier-gap.yaml|1|No tier covers rps in [800, 1200)
03-undeclared-signal.yaml|1|[undeclared]
04-contradiction.yaml|1|[contradiction]
05-cycle.yaml|1|[cycle]
06-ttl-warning.yaml|0|[ttl-sla]
07-malformed.yaml|1|parsing policy YAML
08-runtime-smoke.yaml|0|is valid
09-shape-errors.yaml|1|declared more than once
EOF
out="$("$DG" validate examples/demo-set/09-shape-errors.yaml 2>&1)"
for needle in "unknown mode \"readonly\"" "on_timeout \"panic\"" "can never be true"; do
  grep -qF -- "$needle" <<<"$out" && pass "09 also reports: $needle" || fail "09 missing diagnostic: $needle"
done
grep -q "^Usage:" <<<"$out" && fail "compile errors should not dump CLI usage" || pass "no usage dump on compile errors"

section "README walkthrough policy (examples/bookshop)"
out="$("$DG" validate examples/bookshop/degradation.yaml 2>&1)"; grep -qF "is valid" <<<"$out" && pass "bookshop policy is valid" || fail "bookshop policy" "$out"
sed 's/db_latency_p99: { gte: 1500 }/db_latancy_p99: { gte: 1500 }/' examples/bookshop/degradation.yaml >"$WORK/bookshop-typo.yaml"
out="$("$DG" validate "$WORK/bookshop-typo.yaml" 2>&1)"; grep -qF '[undeclared]' <<<"$out" && pass "misspelled signal is caught" || fail "bookshop typo" "$out"
while IFS='|' read -r signals want; do
  args=()
  for kv in $signals; do args+=(--signal "$kv"); done
  got="$("$DG" simulate examples/bookshop/degradation.yaml "${args[@]}" | sed -n 's/^Matched tier: //p')"
  expect "bookshop $signals → $want" "$want" "$got"
done <<'EOF2'
rps:200|normal
rps:900|busy
rps:2400|overloaded
rps:300 db_latency_p99:1800|overloaded
EOF2

section "dg compile / inspect / graph"
"$DG" compile config/example-degradation.yaml -o "$WORK/example.dg" >/dev/null && pass "compile example policy" || fail "compile example policy"
# Capture output first: `cmd | grep -q` can SIGPIPE cmd, which pipefail reports as failure.
out="$("$DG" inspect "$WORK/example.dg")"; grep -qF "Tiers (5)" <<<"$out" && pass "inspect shows 5 tiers" || fail "inspect"
out="$("$DG" graph "$WORK/example.dg")"; grep -qF "auth-service" <<<"$out" && pass "graph shows dependencies" || fail "graph"
echo junk >"$WORK/junk.dg"
"$DG" inspect "$WORK/junk.dg" >/dev/null 2>&1 && fail "inspect accepted a corrupt bundle" || pass "inspect rejects a corrupt bundle"

section "dg simulate — most degraded matching tier wins"
while IFS='|' read -r signals want; do
  args=()
  for kv in $signals; do args+=(--signal "$kv"); done
  got="$("$DG" simulate config/example-degradation.yaml "${args[@]}" | sed -n 's/^Matched tier: //p')"
  expect "$signals → $want" "$want" "$got"
done <<'EOF'
rps:100|nominal
rps:900|warm
rps:1600 pod_ceiling:0.9|hot
rps:2500|critical
rps:100 db_latency_p99:2500|critical
rps:100 mem_pressure:0.96|survival
rps:12000|survival
EOF

# ─── runtime ─────────────────────────────────────────────────────────────────
section "Runtime — start engine + demo app"
for port in "$CONTROL_PORT" "$SIDECAR_PORT" "$GATE_PORT" "$METRICS_PORT" "$DEMO_PORT"; do
  if curl -s -o /dev/null "http://localhost:$port" 2>/dev/null; then
    fail "port $port is already in use — set *_PORT env vars to free ports"; exit 1
  fi
done
"$DG" compile examples/demo-set/08-runtime-smoke.yaml -o "$WORK/smoke.dg" >/dev/null
sed -e "s/\":9191\"/\":$CONTROL_PORT\"/" -e "s/\":8081\"/\":$SIDECAR_PORT\"/" -e "s/\":9292\"/\":$GATE_PORT\"/" \
    -e "s/prometheus_port: 9090/prometheus_port: $METRICS_PORT/" config/dg-engine-dev.yaml >"$WORK/engine.yaml"
"$WORK/dg-engine" --policy "$WORK/smoke.dg" --config "$WORK/engine.yaml" >"$WORK/engine.log" 2>&1 &
PIDS+=($!)
"$WORK/demo-app" --addr ":$DEMO_PORT" --sidecar "$SIDE" --engine "$CTL" >"$WORK/demo.log" 2>&1 &
PIDS+=($!)
wait_for "engine control API is up" 'd["applied"]["tier"]' nominal 15
for ((i = 0; i < 40; i++)); do [ "$(code "$DEMO/health")" = 200 ] && break; sleep 0.25; done
expect "demo app is up" 200 "$(code "$DEMO/health")"

expect "console served at /" 200 "$(code "$CTL/")"
out="$(curl -s "$CTL/")"; grep -qF "IDDC Console" <<<"$out" && pass "console HTML" || fail "console HTML"
expect "every tier has a verified scenario" 5 "$(curl -s "$CTL/api/policy" | jget 'sum(1 for t in d["tiers"] if t.get("scenario"))')"
expect "sidecar starts at nominal" nominal "$(curl -s "$SIDE/flags" | jget 'd["tier"]')"

section "Runtime — input validation"
expect "inject rejects undeclared signal" 400 "$(post "$CTL/inject" '{"rsp":1}')"
expect "inject rejects malformed JSON" 400 "$(post "$CTL/inject" 'nope')"
expect "write rejected while queue inactive" 409 "$(post "$CTL/write" '{"id":"w0"}')"
expect "override rejects unknown tier" 400 "$(post "$CTL/override" '{"tier":"banana"}')"
expect "gate approve with nothing pending" 409 "$(post "$GATE/gate/approve" '{}')"

section "Runtime — ungated tiers apply after hysteresis"
expect "inject through the demo app's proxy" 200 "$(post "$DEMO/inject" '{"rps":900}')"
wait_for "rps=900 → warm" 'd["applied"]["tier"]' warm
expect "warm disables recommendations" False "$(curl -s "$SIDE/flags" | jget 'd["features"]["recommendations"]')"
expect "demo /recommendations disabled" disabled "$(curl -s "$DEMO/recommendations" | jget 'd["status"]')"
post "$CTL/inject" '{"rps":1600,"pod_ceiling":0.9}' >/dev/null
wait_for "rps=1600 pod=0.9 → hot" 'd["applied"]["tier"]' hot
expect "hot disables uploads" False "$(curl -s "$SIDE/flags" | jget 'd["features"]["uploads"]')"

section "Runtime — gate holds critical until approved"
post "$CTL/inject" '{"rps":2500}' >/dev/null
wait_for "gate opens for critical" 'd["gate"]["state"]+":"+d["gate"]["tier"]' pending:critical
expect "apps still see hot while the gate is open" hot "$(curl -s "$SIDE/tier" | jget 'd["current"]')"
expect "demo checkout still confirmed (not read-only yet)" 200 "$(code -X POST "$DEMO/checkout")"
expect "approve via gate port" 200 "$(post "$GATE/gate/approve" '{}')"
wait_for "critical applied after approval" 'd["applied"]["tier"]' critical 5
expect "sidecar mode is read_only" read_only "$(curl -s "$SIDE/flags" | jget 'd["mode"]')"
expect "demo checkout is queued (202)" 202 "$(code -X POST "$DEMO/checkout")"
expect "demo search serves cache" True "$(curl -s "$DEMO/search?q=hub" | jget 'd["cached"]')"
expect "write queue depth 1" 1 "$(status 'd["queue"]["depth"]')"

section "Runtime — recovery drains the queue"
post "$CTL/inject" '{"rps":50,"pod_ceiling":0.1}' >/dev/null
wait_for "recover → nominal" 'd["applied"]["tier"]' nominal
expect "queue drained" 0 "$(status 'd["queue"]["depth"]')"
expect "queue_drained audit event" 1 "$(curl -s "$CTL/api/events" | jget 'sum(1 for e in d if e["event"]=="queue_drained")')"

section "Runtime — gate timeout runs on_timeout: execute_policy"
post "$CTL/inject" '{"rps":2500}' >/dev/null
wait_for "gate opens for critical" 'd["gate"]["state"]' pending
wait_for "critical applied after the 6s wait" 'd["applied"]["tier"]' critical 15
expect "last gate outcome timed_out" timed_out "$(status 'd["gate"]["last_outcome"]')"

section "Runtime — survival gate: override, release, escalate"
post "$CTL/inject" '{"mem_pressure":0.96}' >/dev/null
wait_for "gate opens for survival" 'd["gate"]["state"]+":"+d["gate"]["tier"]' pending:survival
expect "override via console route" 200 "$(post "$CTL/gate/override" '{"target_tier":"warm","duration_min":1}')"
wait_for "override applies warm" 'd["applied"]["tier"]' warm 5
expect "evaluator locked to warm" warm "$(status 'd["lock"]["tier"]')"
expect "release lock" 200 "$(post "$CTL/override/release" '{}')"
wait_for "signals re-open the survival gate" 'd["gate"]["state"]+":"+d["gate"]["tier"]' pending:survival
expect "escalate" 200 "$(post "$GATE/gate/escalate" '{"message":"smoke"}')"
wait_for "escalation applies survival" 'd["applied"]["tier"]' survival 5
expect "demo app serves the 503 fallback" 503 "$(code "$DEMO/")"

section "Runtime — manual override keeps the sidecar in sync"
expect "force nominal" 200 "$(post "$CTL/override" '{"tier":"nominal","duration_min":1}')"
wait_for "nominal applied" 'd["applied"]["tier"]' nominal 5
expect "sidecar follows the override" nominal "$(curl -s "$SIDE/tier" | jget 'd["current"]')"
post "$CTL/inject" '{"rps":50,"pod_ceiling":0.1,"mem_pressure":0.1,"db_latency_p99":0}' >/dev/null
post "$CTL/override/release" '{}' >/dev/null
sleep 3
expect "stays nominal after release with calm signals" nominal "$(status 'd["applied"]["tier"]')"

section "Runtime — metrics"
metrics="$(curl -s "http://localhost:$METRICS_PORT/metrics")"
expect "exactly one dg_current_tier series" 1 "$(grep -c '^dg_current_tier{' <<<"$metrics")"
grep -qF 'dg_current_tier{service="api-gateway",tier="nominal"} 0' <<<"$metrics" && pass "gauge reports nominal" || fail "gauge value"
grep -qF 'dg_gate_events_total{outcome="escalated"' <<<"$metrics" && pass "gate outcomes counted" || fail "gate outcome metric"

# ─── summary ─────────────────────────────────────────────────────────────────
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
