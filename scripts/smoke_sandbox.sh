#!/usr/bin/env bash
# Smoke test for the sandbox MCP server (local, no network needed).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${BIN:-$ROOT/dist/sandboxd}"
WORK="${WORK:-$HOME/tmp/sandbox-smoke}"
PORT="${PORT:-8099}"
TOKEN="smoke-token-0123456789abcdef"

rm -rf "$WORK"
mkdir -p "$WORK/ws" "$WORK/arch"

export SANDBOX_ID=smoke
export OWNER_ID=smoke-owner
export PUBLIC_URL="http://127.0.0.1:${PORT}"
export SANDBOX_TOKEN="$TOKEN"
export SANDBOX_ROOT="$WORK/ws"
export STATE_FILE="$WORK/state.json"
export IDLE_TIMEOUT=1m
export ARCHIVE_MODE=local
export LOCAL_ARCHIVE_DIR="$WORK/arch"
export LISTEN="127.0.0.1:${PORT}"

"$BIN" >"$WORK/server.log" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null || true' EXIT

for _ in $(seq 1 40); do
  curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1 && break
  sleep 0.25
done

say() { printf '\n=== %s\n' "$1"; }

rpc() {
  curl -fsS -X POST "http://127.0.0.1:${PORT}/mcp" \
    -H "authorization: Bearer ${TOKEN}" -H 'content-type: application/json' \
    -d "$1"
}

say "health (free)"
curl -fsS "http://127.0.0.1:${PORT}/health" | python3 -m json.tool

say "unauthorized is rejected"
curl -s -o /dev/null -w 'status=%{http_code}\n' -X POST "http://127.0.0.1:${PORT}/mcp" \
  -H 'content-type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}'

say "initialize"
rpc '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' | python3 -m json.tool

say "tools/list"
rpc '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | python3 -c 'import json,sys; d=json.load(sys.stdin); print([t["name"] for t in d["result"]["tools"]])'

say "write"
rpc '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write","arguments":{"path":"notes/hello.txt","content":"привет\nмир\n"}}}' | python3 -m json.tool

say "read"
rpc '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read","arguments":{"path":"notes/hello.txt"}}}' | python3 -m json.tool

say "shell"
rpc '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"shell","arguments":{"command":"id; pwd; echo $HOME"}}}' | python3 -m json.tool

say "execute alias"
rpc '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"execute","arguments":{"command":"echo alias-ok"}}}' | python3 -m json.tool

say "path escape is refused"
rpc '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read","arguments":{"path":"../etc/passwd"}}}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["content"][0]["text"])'

say "upload raw"
rpc '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"upload","arguments":{"path":"notes/hello.txt"}}}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); r=d["result"]["content"][0]["resource"]; print(r["uri"], r["mimeType"], len(r["blob"]))'

say "upload url"
rpc '{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"upload","arguments":{"path":"notes/hello.txt","mode":"url"}}}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["structuredContent"]["url"])'

say "delete"
rpc '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"delete","arguments":{"path":"notes"}}}' | python3 -m json.tool

say "health (busy)"
curl -fsS "http://127.0.0.1:${PORT}/health" | python3 -m json.tool

say "end_work"
rpc '{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"end_work","arguments":{}}}' | python3 -m json.tool

say "wait for the archive to finish"
for _ in $(seq 1 40); do
  STATE=$(curl -fsS "http://127.0.0.1:${PORT}/health" | python3 -c 'import json,sys; print(json.load(sys.stdin)["state"])')
  [ "$STATE" = "free" ] && break
  sleep 0.5
done
curl -fsS "http://127.0.0.1:${PORT}/health" | python3 -m json.tool

say "archive file"
ls -l "$WORK/arch"

say "workspace is empty"
ls -A "$WORK/ws" | wc -l
