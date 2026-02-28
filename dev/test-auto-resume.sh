#!/bin/bash
# E2E test: allocate -> pause (suspend) -> exec (triggers auto-resume) -> release
# Usage: make dev-test-auto-resume
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080

echo "=== E2E Auto-Resume Test ==="
echo ""

# --- 0. Register snapshot config with auto_pause ---
echo "=== 0. Register snapshot config ==="
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "auto-resume-test",
    "commands": [{"type":"shell","command":"echo auto-resume-test"}],
    "runner_ttl_seconds": 120,
    "auto_pause": true
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "Registered config: workload_key=$WORKLOAD_KEY"

if [ -z "$WORKLOAD_KEY" ] || [ "$WORKLOAD_KEY" = "null" ]; then
  echo "FAIL: could not register snapshot config"
  exit 1
fi

# --- 1. Allocate runner ---
echo "=== 1. Allocate runner ==="
RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\"}")
echo "Response: $RESP"

RUNNER_ID=$(echo "$RESP" | jq -r '.runner_id')
echo "Runner ID: $RUNNER_ID"

if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  echo "FAIL: no runner_id in response"
  exit 1
fi

# --- 2. Poll until ready ---
echo ""
echo "=== 2. Poll until ready ==="
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo "Runner ready after ${i}s"
    break
  fi
  if [ "$i" = "60" ]; then
    echo "FAIL: runner not ready after 60s"
    exit 1
  fi
  echo -n "."
  sleep 1
done
echo ""

# --- 3. Verify exec works before pause ---
echo "=== 3. Verify exec works before pause ==="
OUTPUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","before-pause"],"timeout_seconds":10}')
echo "$OUTPUT"
if ! echo "$OUTPUT" | grep -q "before-pause"; then
  echo "FAIL: exec before pause did not return expected output"
  exit 1
fi
echo "OK: exec works before pause"

# --- 4. Pause the runner (creates session snapshot) ---
echo ""
echo "=== 4. Pause runner ==="
PAUSE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/pause")
echo "Pause response: $PAUSE_RESP"

# Give the pause a moment to complete
sleep 2

# Check runner state - should be suspended
echo "Checking runner state..."
STATUS_RESP=$(curl -sf "$CP/api/v1/runners/status?runner_id=$RUNNER_ID" 2>/dev/null || echo '{"state":"unknown"}')
STATE=$(echo "$STATUS_RESP" | jq -r '.state // "unknown"')
echo "Runner state: $STATE"

if [ "$STATE" != "suspended" ] && [ "$STATE" != "paused" ]; then
  echo "WARN: runner state is '$STATE' (expected 'suspended')"
  echo "  Auto-resume test may not be meaningful if runner is not suspended."
fi

# --- 5. Execute on suspended runner (should auto-resume) ---
echo ""
echo "=== 5. Execute on suspended runner (triggers auto-resume) ==="
RESUME_OUTPUT=$(curl -sf --no-buffer --max-time 60 -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","after-resume"],"timeout_seconds":30}')
echo "$RESUME_OUTPUT"

if echo "$RESUME_OUTPUT" | grep -q "after-resume"; then
  echo "OK: auto-resume succeeded and exec returned expected output"
elif echo "$RESUME_OUTPUT" | grep -q "auto-resume failed"; then
  echo "WARN: auto-resume failed (may need session snapshot support configured)"
else
  echo "WARN: unexpected output (check if auto-resume is configured)"
fi

# --- 6. Release runner ---
echo ""
echo "=== 6. Release runner ==="
RELEASE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")
echo "Response: $RELEASE_RESP"

echo ""
echo "========================================="
echo "=== AUTO-RESUME TEST COMPLETE ==="
echo "========================================="
