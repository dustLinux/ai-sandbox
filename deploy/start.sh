#!/bin/sh
# Control box entrypoint (Deplexo and friends).
#
# The image root is read only, so everything writable lives in the data
# volume. The rclone configuration arrives as base64 in an environment
# variable, because secrets must not be baked into the image.
set -eu

STATE_DIR="${POOL_STATE_DIR:-/data/pool}"
mkdir -p "$STATE_DIR/keys"

if [ -n "${RCLONE_CONF_B64:-}" ]; then
  CONF_DIR=$(dirname "${RCLONE_CONFIG:-/data/rclone/rclone.conf}")
  mkdir -p "$CONF_DIR"
  printf '%s' "$RCLONE_CONF_B64" | base64 -d > "$CONF_DIR/rclone.conf"
  chmod 600 "$CONF_DIR/rclone.conf"
  export RCLONE_CONFIG="$CONF_DIR/rclone.conf"
  echo "start.sh: rclone config installed at $RCLONE_CONFIG"
fi

if [ -n "${ARCHIVE_REMOTE:-}" ]; then
  echo "start.sh: archive remote $ARCHIVE_REMOTE"
else
  echo "start.sh: ARCHIVE_REMOTE is not set, archives stay local"
fi

export POOL_STATE_DIR="$STATE_DIR"
export POOL_BIN_DIR="${POOL_BIN_DIR:-/app/bin}"
export POOL_KEYS_DIR="${POOL_KEYS_DIR:-$STATE_DIR/keys}"

exec /app/bin/sandbox-pool
