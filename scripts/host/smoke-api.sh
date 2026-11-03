#!/usr/bin/env bash
# Smoke-test a running acd through its public URL: auth, sandbox lifecycle,
# egress, WebSocket streaming and MCP.
#
# Usage: AC_API_KEY_FILE=/path/to/token scripts/host/smoke-api.sh https://agents.example.com
# The key is read from a file so it never appears in argv or shell history.
set -euo pipefail

URL="${1:?usage: smoke-api.sh URL}"
URL="${URL%/}"
KEY_FILE="${AC_API_KEY_FILE:?set AC_API_KEY_FILE}"
AC_API_KEY="$(cat "$KEY_FILE")"
export AC_URL="$URL" AC_API_KEY
ACCTL="${ACCTL:-acctl}"
# curl reads the header from a private file, keeping the key out of argv.
hdr_file=$(mktemp)
chmod 0600 "$hdr_file"
printf 'Authorization: Bearer %s\n' "$AC_API_KEY" >"$hdr_file"
auth=(-H "@$hdr_file")
cleanup() {
  rm -f "$hdr_file"
  [ -z "${id:-}" ] || "$ACCTL" rm "$id" >/dev/null 2>&1 || true
}
trap cleanup EXIT

pass() { printf '  ok  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }

echo "==> $URL"
[ "$(curl -fsS "$URL/healthz")" = '{"status":"ok"}' ] && pass healthz
code=$(curl -s -o /dev/null -w '%{http_code}' "$URL/v1/sandboxes")
[ "$code" = 401 ] && pass "no key -> 401" || fail "no key -> $code"

start=$(date +%s%N)
id=$("$ACCTL" create -q -ttl 10m)
pass "create $id in $(( ($(date +%s%N) - start) / 1000000 )) ms (client round trip, server startup_ms=$("$ACCTL" get "$id" | grep -o '"startup_ms": [0-9]*' | grep -o '[0-9]*$'))"

out=$("$ACCTL" sh "$id" 'echo hello from $(hostname)')
[ "$out" = "hello from $id" ] && pass "exec over WebSocket" || fail "exec: $out"

code=$("$ACCTL" sh "$id" 'curl -s -o /dev/null -w "%{http_code}" --max-time 15 https://pypi.org/simple/requests/')
[ "$code" = 200 ] && pass "egress to pypi.org" || fail "egress: $code"
if "$ACCTL" sh "$id" 'curl -s --max-time 3 http://169.254.169.254/ >/dev/null'; then
  fail "metadata address reachable"
fi
pass "metadata address blocked"

out=$("$ACCTL" sh "$id" 'python3 -m venv /work/v && /work/v/bin/pip install -q requests && /work/v/bin/python -c "import requests; print(requests.get(\"https://example.com\").status_code)"')
[ "$out" = 200 ] && pass "pip install + HTTPS from Python" || fail "pip: $out"

echo 'print(6*7)' | "$ACCTL" put "$id" - /work/main.py 2>/dev/null
[ "$("$ACCTL" exec "$id" -- python3 /work/main.py)" = 42 ] && pass "file upload + run"

if [ "${LONG:-0}" = 1 ]; then
  start=$(date +%s)
  "$ACCTL" sh "$id" 'sleep 130; echo done' | grep -q done || fail "long WebSocket exec"
  pass "silent 130 s WebSocket exec survived ($(( $(date +%s) - start )) s)"
fi

# MCP over streamable HTTP.
hdrs=$(mktemp)
init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
curl -fsS -D "$hdrs" -o /dev/null "${auth[@]}" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' -d "$init" "$URL/v1/mcp"
sid=$(grep -i '^mcp-session-id:' "$hdrs" | cut -d' ' -f2 | tr -d '\r')
rm -f "$hdrs"
[ -n "$sid" ] || fail "mcp: no session id"
mcp() {
  curl -fsS "${auth[@]}" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H "Mcp-Session-Id: $sid" -H 'Mcp-Protocol-Version: 2025-06-18' -d "$1" "$URL/v1/mcp"
}
mcp '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
call=$(printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run_command","arguments":{"computer_id":"%s","command":"uname -sr"}}}' "$id")
mcp "$call" | grep -q 'Linux' && pass "MCP run_command (session $sid)" || fail "mcp tools/call"

echo "==> all checks passed"
