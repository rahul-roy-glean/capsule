#!/bin/bash
# E2E test: Non-destructive checkpoint (WS4)
#
# Tests that a checkpoint creates a snapshot without killing the VM:
#   1. Write marker → checkpoint → verify VM still running
#   2. Write post-checkpoint marker → pause → resume → verify BOTH markers
#
# Usage:
#   SESSION_CHUNK_BUCKET=rroy-gc-testing make dev-test-checkpoint
#
# Prerequisites:
#   - Golden chunked snapshot uploaded
#   - Stack running with GCS sessions
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="cp-e2e-$(date +%s)"
GCS_BUCKET=${SESSION_CHUNK_BUCKET:-}
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

cleanup() {
  if [ -n "${RUNNER_ID:-}" ]; then
    echo "Cleaning up runner $RUNNER_ID..."
    curl -s -X POST "$CP/api/v1/runners/release" \
      -H 'Content-Type: application/json' \
      -d "{\"runner_id\": \"$RUNNER_ID\", \"destroy\": true}" > /dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "Session ID: $SESSION_ID"

# ---------------------------------------------------------------------------
header "1. Register snapshot config"
# ---------------------------------------------------------------------------
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}
CONFIG_RESP=$(curl -s -X POST "$CP/api/v1/layered-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "checkpoint-test",
    "commands": '"$SNAPSHOT_COMMANDS"',
    "runner_ttl_seconds": 300,
    "auto_pause": true,
    "session_max_age_seconds": 3600
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "  workload_key=$WORKLOAD_KEY"

if [ -n "$WORKLOAD_KEY" ] && [ "$WORKLOAD_KEY" != "null" ]; then
  pass "Snapshot config registered"
else
  fail "Snapshot config registration failed: $CONFIG_RESP"
  exit 1
fi

# ---------------------------------------------------------------------------
header "2. Allocate runner with session"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{
    \"workload_key\": \"$WORKLOAD_KEY\",
    \"session_id\": \"$SESSION_ID\",
    \"ttl_seconds\": 300,
    \"auto_pause\": true
  }")
RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id // .runner.id')
HOST_ADDR=$(echo "$ALLOC_RESP" | jq -r '.host_address')
echo "  runner_id=$RUNNER_ID host=$HOST_ADDR"

if [ -n "$RUNNER_ID" ] && [ "$RUNNER_ID" != "null" ]; then
  pass "Runner allocated"
else
  fail "Failed to allocate runner: $ALLOC_RESP"
  exit 1
fi

# Use the manager's HTTP endpoint (not the gRPC address from allocate response)
HOST=$MGR
sleep 2  # wait for thaw-agent readiness

# ---------------------------------------------------------------------------
header "3. Write pre-checkpoint marker"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "echo cp-test > /tmp/cp.txt && echo ok"]}')
if echo "$EXEC_RESP" | grep -q "ok"; then
  pass "Wrote pre-checkpoint marker"
else
  fail "Failed to write marker: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "4. Create non-destructive checkpoint"
# ---------------------------------------------------------------------------
CP_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/checkpoint")
echo "  checkpoint response: $CP_RESP"
CP_RUNNING=$(echo "$CP_RESP" | jq -r '.running')
CP_SESSION=$(echo "$CP_RESP" | jq -r '.session_id')

if [ "$CP_RUNNING" = "true" ]; then
  pass "Checkpoint returned running=true"
else
  fail "Expected running=true, got: $CP_RESP"
fi

if [ -n "$CP_SESSION" ] && [ "$CP_SESSION" != "null" ]; then
  pass "Checkpoint returned session_id=$CP_SESSION"
else
  fail "No session_id in checkpoint response"
fi

# ---------------------------------------------------------------------------
header "5. Verify VM is still running after checkpoint"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["echo", "still-alive"]}')
if echo "$EXEC_RESP" | grep -q "still-alive"; then
  pass "VM still running after checkpoint"
else
  fail "VM not responding after checkpoint: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "6. Write post-checkpoint marker"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "echo post-cp > /tmp/cp2.txt && echo ok"]}')
if echo "$EXEC_RESP" | grep -q "ok"; then
  pass "Wrote post-checkpoint marker"
else
  fail "Failed to write post-checkpoint marker: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "7. Pause runner (normal pause after checkpoint)"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/pause")
echo "  pause response: $PAUSE_RESP"
PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id')

if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Runner paused after checkpoint"
else
  fail "Failed to pause: $PAUSE_RESP"
fi

sleep 2

# ---------------------------------------------------------------------------
header "8. Resume and verify BOTH markers exist"
# ---------------------------------------------------------------------------
CONNECT_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/connect")
echo "  connect response: $CONNECT_RESP"
sleep 3  # wait for thaw-agent readiness

# Check pre-checkpoint marker
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["cat", "/tmp/cp.txt"]}')
if echo "$EXEC_RESP" | grep -q "cp-test"; then
  pass "Pre-checkpoint marker survived"
else
  fail "Pre-checkpoint marker lost: $EXEC_RESP"
fi

# Check post-checkpoint marker
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["cat", "/tmp/cp2.txt"]}')
if echo "$EXEC_RESP" | grep -q "post-cp"; then
  pass "Post-checkpoint marker survived"
else
  fail "Post-checkpoint marker lost: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "9. Release runner"
# ---------------------------------------------------------------------------
RELEASE_RESP=$(curl -s -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\": \"$RUNNER_ID\", \"destroy\": true}")
RUNNER_ID=""  # prevent cleanup trap from double-releasing
pass "Runner released"

# ---------------------------------------------------------------------------
header "Results"
# ---------------------------------------------------------------------------
echo ""
echo "Passed: $PASS  Failed: $FAIL"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
echo "All tests passed!"
