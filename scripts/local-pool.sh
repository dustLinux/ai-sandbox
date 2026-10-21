#!/usr/bin/env bash
# Local control box for development: a public tunnel plus the pool.
#
#   scripts/local-pool.sh [state-dir]
#
# The tunnel address changes when it reconnects, so the pool is restarted
# with the new address. On a hosted control box the address is stable and
# this script is not needed.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STATE="${1:-$HOME/tmp/pool-local}"
PORT="${POOL_PORT:-8123}"
URL_FILE="$STATE/tunnel.url"
LOG="$STATE/supervisor.log"
PROVIDER="${POOL_PROVIDER:-github}"
export POOL_TOKEN="${POOL_TOKEN:-localtesttoken0123456789abcdef}"

mkdir -p "$STATE"
: > "$LOG"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" >> "$LOG"; }

start_pool() {
  local url="$1"
  log "starting the pool for $url"
  env \
    POOL_PROVIDER="$PROVIDER" \
    POOL_LISTEN="127.0.0.1:${PORT}" \
    POOL_PUBLIC_URL="$url" \
    POOL_TOKEN="$POOL_TOKEN" \
    POOL_STATE_DIR="$STATE" \
    ARCHIVE_REMOTE="$STATE/drive" \
    RCLONE_BIN="${RCLONE_BIN:-$HOME/tmp/pool-local/rclone}" \
    ARCHIVE_QUOTA_BYTES=10737418240 \
    GITHUB_TOKEN="${GITHUB_TOKEN:-}" \
    GITHUB_REPO="${GITHUB_REPO:-dustLinux/ai-sandbox}" \
    GITHUB_WORKFLOW=sandbox.yml \
    GITHUB_REF=main \
    PROVISION_TIMEOUT=8m \
    ACQUIRE_WAIT=6m \
    HEALTH_INTERVAL="${HEALTH_INTERVAL:-30s}" \
    QUEUE_INTERVAL=10s \
    "$ROOT/dist/sandbox-pool" >> "$STATE/pool.log" 2>&1 &
  echo $! > "$STATE/pool.pid"
}

stop_pool() {
  if [ -f "$STATE/pool.pid" ]; then
    kill "$(cat "$STATE/pool.pid")" 2>/dev/null || true
    rm -f "$STATE/pool.pid"
  fi
}

pkill -f "ssh .*localhost.run" 2>/dev/null || true
stop_pool
bash "$ROOT/scripts/tunnel.sh" "$PORT" "$URL_FILE" >> "$LOG" 2>&1 &
echo $! > "$STATE/tunnel.pid"

CURRENT=""
while true; do
  URL=$(cat "$URL_FILE" 2>/dev/null || true)
  if [ -n "$URL" ] && [ "$URL" != "$CURRENT" ]; then
    stop_pool
    start_pool "$URL"
    CURRENT="$URL"
    log "pool is up at $CURRENT"
  fi
  sleep 10
done
