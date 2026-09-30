#!/bin/sh
# Starts dg-engine and the demo shop together (used by `make demo` and the Docker image).
#   PORT    demo shop port            (default 8080)
#   BIN     directory with binaries   (default ./bin)
#   POLICY  compiled policy bundle    (default $BIN/policy.dg)
set -e

PORT="${PORT:-8080}"
BIN="${BIN:-./bin}"
POLICY="${POLICY:-$BIN/policy.dg}"

echo "[demo] launching dg-engine (console on :9191)..."
"$BIN/dg-engine" --policy "$POLICY" --config config/dg-engine-dev.yaml &
ENGINE_PID=$!
trap 'kill $ENGINE_PID 2>/dev/null' EXIT INT TERM

sleep 2

echo "[demo] launching demo shop on :${PORT}"
"$BIN/demo-app" \
  --addr ":${PORT}" \
  --sidecar "http://localhost:8081" \
  --engine  "http://localhost:9191"
