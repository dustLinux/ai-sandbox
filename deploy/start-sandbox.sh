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

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }

# --- 1. tunnel ---------------------------------------------------------------
# The runner is behind NAT, so the sandbox needs a public address. A reverse
# SSH tunnel is the cheapest way to get one.
TUNNEL_URL=""
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ServerAliveInterval=30 \
    -o ExitOnForwardFailure=yes -R "80:localhost:${PORT}" nokey@localhost.run >/tmp/tunnel.log 2>&1 &
TUNNEL_PID=$!

for _ in $(seq 1 30); do
  TUNNEL_URL=$(grep -oE 'https://[a-z0-9]{6,}\.lhr\.life' /tmp/tunnel.log | grep -v admin | head -1 || true)
  [ -n "$TUNNEL_URL" ] && break
  sleep 2
done

if [ -z "$TUNNEL_URL" ]; then
  log "the ssh tunnel did not come up, trying cloudflared"
  curl -fsSL -o /tmp/cloudflared https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64
  chmod +x /tmp/cloudflared
  /tmp/cloudflared tunnel --url "http://localhost:${PORT}" >/tmp/cf.log 2>&1 &
  for _ in $(seq 1 20); do
    TUNNEL_URL=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' /tmp/cf.log | head -1 || true)
    [ -n "$TUNNEL_URL" ] && break
    sleep 2
  done
fi

if [ -z "$TUNNEL_URL" ]; then
  log "no public address could be obtained"
  tail -20 /tmp/tunnel.log || true
  exit 1
fi
log "public address: $TUNNEL_URL"

# --- 2. registration --------------------------------------------------------
REGISTER=$(curl -fsS -X POST "${POOL_URL}/v1/machines/register" \
  -H 'content-type: application/json' \
  -d "{\"code\":\"${REGISTER_CODE}\",\"url\":\"${TUNNEL_URL}\",\"run_id\":\"${RUN_ID}\",\"host\":\"${TUNNEL_URL}\"}")
log "registered: $(printf '%s' "$REGISTER" | head -c 120)"

SANDBOX_TOKEN=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sandbox_token"])')
ARCHIVE_TOKEN=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["archive_token"])')
ARCHIVE_URL=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["archive_url"])')
SANDBOX_ID=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin)["sandbox_id"])')
IDLE_TIMEOUT=$(printf '%s' "$REGISTER" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("idle_timeout","15m"))')

# --- 3. the sandbox ---------------------------------------------------------
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

docker logs sandbox >/tmp/sandbox-container.log 2>&1 &
log "sandbox container started, waiting for the pool to cancel this run"
tail -f /tmp/sandbox-container.log &
tail -f /tmp/tunnel.log &

# The pool cancels the run when the sandbox is released.
while true; do
  sleep 60
  if ! docker ps --format '{{.Names}}' | grep -qx sandbox; then
    log "the sandbox container is gone, stopping"
    break
  fi
done
