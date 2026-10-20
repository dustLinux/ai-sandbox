#!/usr/bin/env bash
# Deploy the sandbox pool (control box) to a Vibe Hosting server.
#
#   scripts/deploy_pool.sh <host> <port> <keyfile> <public_url>
#
# The pool token is read from ~/secrets/ai-sandbox-vibe/pool.env (created on
# first run) and pushed to the box as /root/pool.env.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SECRETS="$HOME/secrets/ai-sandbox-vibe"
POOL_ENV="$SECRETS/pool.env"

HOST="${1:?host}"
PORT="${2:?port}"
KEY="${3:?key file}"
PUBLIC_URL="${4:?public url}"

mkdir -p "$SECRETS"
chmod 700 "$SECRETS"
if [ ! -f "$POOL_ENV" ]; then
  umask 077
  TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/' | cut -c1-40)
  cat > "$POOL_ENV" <<EOF
POOL_TOKEN=$TOKEN
EOF
  chmod 600 "$POOL_ENV"
  echo "created $POOL_ENV"
fi
# shellcheck disable=SC1090
. "$POOL_ENV"

SSH=(ssh -i "$KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no
     -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -p "$PORT" "root@$HOST")
SCP=(scp -q -i "$KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no
     -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$PORT")

echo "→ uploading binaries"
for pair in "sandbox-pool-linux-amd64:sandbox-pool" \
            "sandboxd-linux-amd64:sandboxd-linux-amd64" \
            "sandboxd-linux-arm64:sandboxd-linux-arm64"; do
  src="${pair%%:*}"
  dst="${pair##*:}"
  for attempt in 1 2 3 4 5; do
    if "${SCP[@]}" "$ROOT/dist/$src" "root@$HOST:/root/pool/bin/$dst.new"; then
      break
    fi
    echo "   retry $attempt for $src"
    sleep 3
  done
done
# A running binary cannot be overwritten in place: replace it atomically.
"${SSH[@]}" 'cd /root/pool/bin && for f in *.new; do mv -f "$f" "${f%.new}"; done && chmod +x sandbox-pool && pkill -x sandbox-pool || true'

ARCHIVE_REMOTE="${ARCHIVE_REMOTE:-/root/gdrive}"

echo "→ writing /root/pool.env"
cat <<EOF | "${SSH[@]}" 'cat > /root/pool.env && chmod 600 /root/pool.env'
POOL_LISTEN=:80
POOL_PUBLIC_URL=$PUBLIC_URL
POOL_TOKEN=$POOL_TOKEN
POOL_STATE_DIR=/root/pool
POOL_NAME=ai-sandbox
ARCHIVE_REMOTE=$ARCHIVE_REMOTE
RCLONE_BIN=rclone
RCLONE_CONFIG=/root/.config/rclone/rclone.conf
ARCHIVE_QUOTA_BYTES=${ARCHIVE_QUOTA_BYTES:-10737418240}
HEALTH_INTERVAL=${HEALTH_INTERVAL:-5m}
QUEUE_INTERVAL=${QUEUE_INTERVAL:-10s}
EOF

echo "→ writing /root/run.sh"
cat <<'EOF' | "${SSH[@]}" 'cat > /root/run.sh && chmod 755 /root/run.sh'
#!/bin/sh
set -a
. /root/pool.env
set +a
exec /root/pool/bin/sandbox-pool >> /root/pool/pool.log 2>&1
EOF

echo "→ preparing directories"
"${SSH[@]}" 'mkdir -p /root/pool/keys /root/gdrive && chmod 700 /root/pool /root/pool/keys /root/gdrive && chmod +x /root/pool/bin/sandbox-pool'

echo "→ done; the supervisor switches to the pool within a second"
echo "   health: $PUBLIC_URL/health"
echo "   token:  $POOL_TOKEN"
