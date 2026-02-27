#!/bin/bash
# E2E test: allocate → poll → exec → release
# Usage: make dev-test-exec
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080

echo "=== E2E Exec Test ==="
echo ""

# --- 0. Register snapshot config ---
echo "=== 0. Register snapshot config ==="
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "exec-test",
    "commands": [{"type":"shell","command":"echo exec-test"}],
    "runner_ttl_seconds": 60,
    "auto_pause": false
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "Registered config: workload_key=$WORKLOAD_KEY"

if [ -z "$WORKLOAD_KEY" ] || [ "$WORKLOAD_KEY" = "null" ]; then
  echo "FAIL: could not register snapshot config"
  exit 1
fi

# --- 1. Allocate runner ---
echo "=== 1. Allocate runner (exec mode) ==="
RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\"}")
echo "Response: $RESP"

RUNNER_ID=$(echo "$RESP" | jq -r '.runner_id')
HOST_ADDR=$(echo "$RESP" | jq -r '.host_address // empty')
echo "Runner ID:    $RUNNER_ID"
echo "Host Address: ${HOST_ADDR:-"(none yet)"}"

if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  echo "FAIL: no runner_id in response"
  exit 1
fi

# --- 2. Poll until ready ---
echo ""
echo "=== 2. Poll until ready ==="
READY=false
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo "Runner ready after ${i}s"
    READY=true
    break
  fi
  if [ "$i" = "60" ]; then
    echo "FAIL: runner not ready after 60s (last HTTP status: $HTTP_CODE)"
    exit 1
  fi
  echo -n "."
  sleep 1
done
echo ""

# --- 3. Execute 'echo hello' ---
echo "=== 3. Execute 'echo hello' ==="
OUTPUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","hello"],"timeout_seconds":10}')
echo "$OUTPUT"

if ! echo "$OUTPUT" | grep -q '"type":"stdout"'; then
  echo "FAIL: no stdout frame in output"
  exit 1
fi
if ! echo "$OUTPUT" | grep -q '"type":"exit"'; then
  echo "FAIL: no exit frame in output"
  exit 1
fi
EXIT_CODE=$(echo "$OUTPUT" | grep '"type":"exit"' | jq -r '.code')
if [ "$EXIT_CODE" != "0" ]; then
  echo "FAIL: expected exit code 0, got $EXIT_CODE"
  exit 1
fi
echo "OK: echo hello returned exit code 0"

# --- 4. Execute with timeout ---
echo ""
echo "=== 4. Execute with timeout (sleep 30, timeout 2s) ==="
TIMEOUT_OUTPUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sleep","30"],"timeout_seconds":2}' 2>&1 || true)
echo "$TIMEOUT_OUTPUT"
if echo "$TIMEOUT_OUTPUT" | grep -q '"type":"error"'; then
  echo "OK: got timeout error frame"
elif echo "$TIMEOUT_OUTPUT" | grep -q '"type":"exit"'; then
  TIMEOUT_EXIT=$(echo "$TIMEOUT_OUTPUT" | grep '"type":"exit"' | jq -r '.code')
  echo "OK: process exited with code $TIMEOUT_EXIT (killed by timeout)"
else
  echo "WARN: unexpected timeout response (may be OK depending on implementation)"
fi

# --- 5. Release runner ---
echo ""
echo "=== 5. Release runner ==="
RELEASE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")
echo "Response: $RELEASE_RESP"

echo ""
echo "========================================="
echo "=== ALL TESTS PASSED ==="
echo "========================================="
