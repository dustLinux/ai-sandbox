#!/usr/bin/env bash
# Keeps a public tunnel to the local pool alive and prints the current address.
#
#   scripts/tunnel.sh <local-port> <state-file>
#
# The address changes when the tunnel reconnects, so the state file always
# holds the current one.
set -u

PORT="${1:-8123}"
STATE="${2:-$HOME/tmp/pool-local/tunnel.url}"
LOG="${STATE%.url}.log"

url=""
while true; do
  rm -f "$HOME"/tmp/phone-tunnel.log
  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ServerAliveInterval=20 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes \
      -R "80:localhost:${PORT}" nokey@localhost.run >"$HOME"/tmp/phone-tunnel.log 2>&1 &
  pid=$!
  for _ in $(seq 1 25); do
    url=$(grep -oE 'https://[a-z0-9]{6,}\.lhr\.life' "$HOME"/tmp/phone-tunnel.log | grep -v admin | head -1 || true)
    if [ -n "$url" ]; then
      printf '%s' "$url" > "$STATE"
      printf '%s tunnel up: %s\n' "$(date -u +%H:%M:%S)" "$url" >> "$LOG"
      break
    fi
    sleep 2
  done
  wait $pid 2>/dev/null
  printf '%s tunnel down, reconnecting\n' "$(date -u +%H:%M:%S)" >> "$LOG"
  sleep 3
done
