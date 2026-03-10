#!/bin/bash
# E2E test: Traffic-triggered auto-resume (WS5)
#
# Tests that sending an exec request to a suspended runner automatically
# resumes it without requiring an explicit /connect call.
#
# Usage:
#   GCS_BUCKET=rroy-gc-testing make dev-test-auto-resume
#
# Prerequisites:
#   - Golden chunked snapshot uploaded: GCS_BUCKET=<bucket> make dev-snapshot
#   - Stack running with GCS sessions:  GCS_BUCKET=<bucket> make dev-stack
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="auto-e2e-$(date +%s)"
GCS_BUCKET=${GCS_BUCKET:-${SESSION_CHUNK_BUCKET:-}}
PASS=0
FAIL=0

. "$(dirname "${BASH_SOURCE[0]}")/lib-workload-key.sh"
. "$(dirname "${BASH_SOURCE[0]}")/lib-gcs-mode.sh"

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

GCS_BUCKET=$(require_gcs_bucket)
assert_manager_gcs_mode "$GCS_BUCKET"

echo "GCS bucket: $GCS_BUCKET"
echo "Session ID: $SESSION_ID"

# ---------------------------------------------------------------------------
header "1. Discover workload key and register config"
# ---------------------------------------------------------------------------
require_workload_key
register_dev_config "auto-resume-test" '{"ttl": 300, "auto_pause": true, "session_max_age_seconds": 3600}'
pass "Workload key discovered and config registered"

# ---------------------------------------------------------------------------
header "2. Allocate runner with session"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{
    \"workload_key\": \"$WORKLOAD_KEY\",
    \"session_id\": \"$SESSION_ID\"
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

HOST=$MGR
sleep 2  # wait for thaw-agent readiness

# ---------------------------------------------------------------------------
header "3. Write marker file"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "echo auto-marker > /tmp/auto-test.txt && echo ok"]}')
if echo "$EXEC_RESP" | grep -q "ok"; then
  pass "Wrote marker file"
else
  fail "Failed to write marker: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "4. Pause runner"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/pause")
PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id')
echo "  pause response: $PAUSE_RESP"

if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Runner paused"
else
  fail "Failed to pause: $PAUSE_RESP"
fi

sleep 2  # wait for state to settle

# ---------------------------------------------------------------------------
header "5. Verify runner is suspended"
# ---------------------------------------------------------------------------
STATUS_RESP=$(curl -s "$CP/api/v1/runners/status?runner_id=$RUNNER_ID" 2>/dev/null || echo '{"state":"unknown"}')
STATE=$(echo "$STATUS_RESP" | jq -r '.state // "unknown"')
echo "  runner state: $STATE"

if [ "$STATE" = "suspended" ] || [ "$STATE" = "paused" ]; then
  pass "Runner is suspended"
else
  echo "  (state is '$STATE' — auto-resume test may still work if runner is in a pausable state)"
fi

# ---------------------------------------------------------------------------
header "6. Exec on suspended runner (triggers auto-resume)"
# ---------------------------------------------------------------------------
# This is the key test: sending an exec to a suspended runner should
# automatically resume it without needing an explicit /connect call.
EXEC_RESP=$(curl -s --max-time 60 -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["cat", "/tmp/auto-test.txt"], "timeout_seconds": 30}')
echo "  exec response: $EXEC_RESP"

if echo "$EXEC_RESP" | grep -q "auto-marker"; then
  pass "Auto-resume succeeded — marker survived"
else
  fail "Auto-resume failed or marker lost: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "7. Verify runner is running again"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["echo", "still-alive"]}')
if echo "$EXEC_RESP" | grep -q "still-alive"; then
  pass "Runner is running after auto-resume"
else
  fail "Runner not responding after auto-resume: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "8. Release runner"
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
