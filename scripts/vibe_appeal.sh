#!/usr/bin/env bash
# Appeal a blocked Vibe Hosting account (the only channel they answer on).
#
#   scripts/vibe_appeal.sh <email> <server_id> [message]
#
# The email is the only way they can reply, so it must be one you read.
set -euo pipefail

EMAIL="${1:?email}"
SERVER_ID="${2:-}"
MESSAGE="${3:-Hi. My Vibe Hosting account was blocked with \"violation of the terms of service\". I run a small self-hosted MCP sandbox service on my own accounts: each machine is a throwaway Ubuntu box where an AI agent runs shell commands, and the account is only used for that. I would like to understand which rule was broken and to fix it, or to have the account restored. Server id: ${SERVER_ID:-n/a}.}"

umask 077
PAYLOAD=$(python3 - "$EMAIL" "$MESSAGE" <<'PY'
import json, sys
print(json.dumps({"kind": "appeal", "message": sys.argv[2], "email": sys.argv[1]}))
PY
)

curl -sS -X POST https://thevibehosting.com/feedback \
  -H 'content-type: application/json' \
  -d "$PAYLOAD"
echo
