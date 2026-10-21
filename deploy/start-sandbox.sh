#!/usr/bin/env bash
# Runs inside a GitHub Actions job: starts the sandbox container, publishes it
# through a tunnel and registers it with the pool.
#
# The tokens are not passed through GitHub: the job asks the pool for them
# with the one time code it received.
set -euo pipefail

: "${SLUG:?slug is required}"
: "${OWNER_ID:?owner_id is required}"
: "${POOL_URL:?pool_url is required}"
: "${REGISTER_CODE:?register_code is required}"
IMAGE="${IMAGE:-ghcr.io/dustlinux/ai-sandbox/sandbox:latest}"
PORT="${PORT:-8080}"
RUN_ID="${GITHUB_RUN_ID:-0}"
LOG=/tmp/sandbox-job.log
URL_FILE=/tmp/sandbox-url

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }

# --- 1. tunnel ---------------------------------------------------------------
# cloudflared keeps the same address while it runs and reconnects by itself.
# The reverse SSH tunnel is the fallback.
TUNNEL_PID=""
TUNNEL_URL=""

start_cloudflared() {
  if [ ! -x /tmp/cloudflared ]; then
    curl -fsSL -o /tmp/cloudflared \
      https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64
    chmod +x /tmp/cloudflared
  fi
  /tmp/cloudflared tunnel --url "http://localhost:${PORT}" >/tmp/cf.log 2>&1 &
  TUNNEL_PID=$!
  for _ in $(seq 1 25); do
    TUNNEL_URL=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' /tmp/cf.log | head -1 || true)
    [ -n "$TUNNEL_URL" ] && return 0
    sleep 2
  done
  return 1
}

start_ssh_tunnel() {
  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=20 \
      -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes \
      -R "80:localhost:${PORT}" nokey@localhost.run >/tmp/tunnel.log 2>&1 &
  TUNNEL_PID=$!
  for _ in $(seq 1 25); do
    TUNNEL_URL=$(grep -oE 'https://[a-z0-9]{6,}\.lhr\.life' /tmp/tunnel.log | grep -v admin | head -1 || true)
    [ -n "$TUNNEL_URL" ] && return 0
    sleep 2
  done
  return 1
}

if ! start_cloudflared; then
  log "cloudflared did not come up, falling back to the ssh tunnel"
  TUNNEL_URL=""
  if ! start_ssh_tunnel; then
    log "no public address could be obtained"
    tail -20 /tmp/cf.log /tmp/tunnel.log 2>/dev/null || true
    exit 1
  fi
fi
log "public address: $TUNNEL_URL"

# --- 2. registration --------------------------------------------------------
register() {
  curl -fsS -X POST "${POOL_URL}/v1/machines/register" \
    -H 'content-type: application/json' \
    -d "{\"code\":\"${REGISTER_CODE}\",\"url\":\"${1}\",\"run_id\":\"${RUN_ID}\",\"host\":\"${1}\"}"
}

REGISTER=$(register "$TUNNEL_URL")
log "registered"

SANDBOX_TOKEN=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sandbox_token"])')
ARCHIVE_TOKEN=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["archive_token"])')
ARCHIVE_URL=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["archive_url"])')
SANDBOX_ID=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sandbox_id"])')
IDLE_TIMEOUT=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("idle_timeout","15m"))')
printf '%s' "$TUNNEL_URL" > "$URL_FILE"

# --- 3. the sandbox ---------------------------------------------------------
start_sandbox() {
  docker rm -f sandbox >/dev/null 2>&1 || true
  docker run -d --name sandbox \
    --restart no \
    -p "${PORT}:8080" \
    -e SANDBOX_ID="${SANDBOX_ID}" \
    -e OWNER_ID="${OWNER_ID}" \
    -e PUBLIC_URL="${TUNNEL_URL}" \
    -e SANDBOX_TOKEN="${SANDBOX_TOKEN}" \
    -e ARCHIVE_MODE=push \
    -e ARCHIVE_URL="${ARCHIVE_URL}" \
    -e ARCHIVE_TOKEN="${ARCHIVE_TOKEN}" \
    -e IDLE_TIMEOUT="${IDLE_TIMEOUT}" \
    -e STATE_FILE=/data/state.json \
    -e LOG_COMMANDS=1 \
    "${IMAGE}" >/dev/null
}

start_sandbox
log "sandbox container started"

# --- 4. watchdog ------------------------------------------------------------
# A dead tunnel or container is replaced; a moved tunnel is announced again.
# The pool may also move (a changing address of the control box): the answer
# of the registration carries the address the sandbox has to push its archive
# to, and a changed value restarts the container with the new one.
ARCHIVE_URL_SEEN="$ARCHIVE_URL"
while true; do
  sleep 20
  if ! docker ps --format '{{.Names}}' | grep -qx sandbox; then
    log "the sandbox container is gone, restarting it"
    start_sandbox || true
  fi
  if REGISTER_OUT=$(register "$TUNNEL_URL" 2>/dev/null); then
    NEW_ARCHIVE_URL=$(printf '%s' "$REGISTER_OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("archive_url",""))' 2>/dev/null || true)
    if [ -n "$NEW_ARCHIVE_URL" ] && [ "$NEW_ARCHIVE_URL" != "$ARCHIVE_URL_SEEN" ]; then
      log "the pool address changed to $NEW_ARCHIVE_URL, restarting the sandbox"
      ARCHIVE_URL="$NEW_ARCHIVE_URL"
      ARCHIVE_URL_SEEN="$NEW_ARCHIVE_URL"
      start_sandbox || true
    fi
  fi
  if ! kill -0 "$TUNNEL_PID" 2>/dev/null; then
    log "the tunnel is gone, restarting it"
    TUNNEL_URL=""
    if ! start_cloudflared; then
      TUNNEL_URL=""
      start_ssh_tunnel || true
    fi
    if [ -n "$TUNNEL_URL" ]; then
      log "new public address: $TUNNEL_URL"
      register "$TUNNEL_URL" >/dev/null || true
      printf '%s' "$TUNNEL_URL" > "$URL_FILE"
    fi
  fi
done
