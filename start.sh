#!/bin/sh
set -e

PORT="${PORT:-8080}"

echo "[start] launching dg-engine..."
./dg-engine --policy policy.dg --config config/dg-engine-dev.yaml &

echo "[start] waiting for engine to be ready..."
sleep 3

echo "[start] launching demo-app on :${PORT}"
exec ./demo-app \
  --addr ":${PORT}" \
  --sidecar "http://localhost:8081" \
  --engine  "http://localhost:9191"
