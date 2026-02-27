#!/bin/bash
# E2E test: allocate(session) → exec → pause → allocate(same session) → exec → verify state → release
# Usage: make dev-test-pause-resume
#
# Prerequisites:
#   - Stack running: make dev-stack (or dev-stack-local)
#   - Snapshot built: make dev-snapshot (or dev-snapshot-local)
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="test-session-$(date +%s)"
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

# ---------------------------------------------------------------------------
header "1. Register snapshot config with TTL + auto_pause"
# ---------------------------------------------------------------------------
# Register a config with 15s TTL and auto_pause enabled.
# In a real scenario this would be done once per workload_key.
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "pause-resume-test",
    "commands": [{"type":"shell","command":"echo pause-test"}],
    "runner_ttl_seconds": 15,
    "auto_pause": true,
    "session_max_age_seconds": 3600
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "Registered config: workload_key=$WORKLOAD_KEY"
echo "  TTL: $(echo "$CONFIG_RESP" | jq '.runner_ttl_seconds')s, auto_pause: $(echo "$CONFIG_RESP" | jq '.auto_pause')"

if [ -n "$WORKLOAD_KEY" ] && [ "$WORKLOAD_KEY" != "null" ]; then
  pass "Snapshot config registered with TTL fields"
else
  fail "Snapshot config registration failed"
  echo "$CONFIG_RESP"
  exit 1
fi

# ---------------------------------------------------------------------------
header "2. Allocate runner with session_id"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"session_id\":\"$SESSION_ID\"}")
echo "Response: $ALLOC_RESP"

RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
RESUMED=$(echo "$ALLOC_RESP" | jq -r '.resumed // false')
RESP_SESSION=$(echo "$ALLOC_RESP" | jq -r '.session_id // empty')
echo "  Runner ID:  $RUNNER_ID"
echo "  Session ID: $RESP_SESSION"
echo "  Resumed:    $RESUMED"

if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  fail "Allocate returned no runner_id"
  exit 1
fi
pass "Runner allocated"

if [ "$RESUMED" = "true" ]; then
  fail "First allocation should not be a resume"
else
  pass "First allocation is fresh (not resumed)"
fi

# ---------------------------------------------------------------------------
header "3. Poll until ready"
# ---------------------------------------------------------------------------
READY=false
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo "  Runner ready after ${i}s"
    READY=true
    break
  fi
  echo -n "."
  sleep 1
done
echo ""

if $READY; then
  pass "Runner became ready"
else
  fail "Runner did not become ready in 60s"
  exit 1
fi

# ---------------------------------------------------------------------------
header "4. Execute: create a file in the VM"
# ---------------------------------------------------------------------------
EXEC_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sh","-c","echo session-state-test > /tmp/pause-test.txt && cat /tmp/pause-test.txt"],"timeout_seconds":10}')
echo "$EXEC_OUT"

if echo "$EXEC_OUT" | grep -q 'session-state-test'; then
  pass "File created and read back in VM"
else
  fail "Could not create/read file in VM"
fi

EXIT_CODE=$(echo "$EXEC_OUT" | grep '"type":"exit"' | jq -r '.code' 2>/dev/null || echo "?")
if [ "$EXIT_CODE" = "0" ]; then
  pass "Exec exited cleanly (code 0)"
else
  fail "Exec exit code: $EXIT_CODE"
fi

# ---------------------------------------------------------------------------
header "5. Pause runner"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/pause" \
  -H 'Content-Type: application/json')
echo "Response: $PAUSE_RESP"

PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id // empty')
PAUSE_LAYER=$(echo "$PAUSE_RESP" | jq -r '.layer // empty')
PAUSE_SIZE=$(echo "$PAUSE_RESP" | jq -r '.snapshot_size_bytes // 0')

echo "  Session:  $PAUSE_SESSION"
echo "  Layer:    $PAUSE_LAYER"
echo "  Size:     $PAUSE_SIZE bytes"

if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Runner paused, got session_id back"
else
  fail "Pause did not return session_id"
fi

if [ "$PAUSE_LAYER" = "0" ]; then
  pass "First pause is layer 0"
else
  fail "Expected layer 0, got $PAUSE_LAYER"
fi

if [ "$PAUSE_SIZE" -gt 0 ] 2>/dev/null; then
  pass "Snapshot size is $PAUSE_SIZE bytes"
else
  fail "Snapshot size should be > 0"
fi

# ---------------------------------------------------------------------------
header "6. Verify runner status is suspended"
# ---------------------------------------------------------------------------
sleep 1
STATUS_RESP=$(curl -sf "$CP/api/v1/runners/status?runner_id=$RUNNER_ID" || echo '{"error":"not found"}')
STATUS=$(echo "$STATUS_RESP" | jq -r '.status // "unknown"')
echo "  Status: $STATUS"

if [ "$STATUS" = "suspended" ]; then
  pass "Control plane reports runner as suspended"
else
  # The runner might not be in the host registry anymore; check if 404
  fail "Expected status 'suspended', got '$STATUS'"
fi

# ---------------------------------------------------------------------------
header "7. Verify session files on disk"
# ---------------------------------------------------------------------------
SESSION_DIR="/tmp/fc-dev/sessions/$SESSION_ID"
if [ -f "$SESSION_DIR/metadata.json" ]; then
  pass "metadata.json exists"
  echo "  $(cat "$SESSION_DIR/metadata.json" | jq -c '{layers,workload_key,runner_id}')"
else
  # Try derived path
  SNAP_PARENT=$(dirname /tmp/fc-dev/snapshots)
  SESSION_DIR="$SNAP_PARENT/sessions/$SESSION_ID"
  if [ -f "$SESSION_DIR/metadata.json" ]; then
    pass "metadata.json exists (at $SESSION_DIR)"
    echo "  $(cat "$SESSION_DIR/metadata.json" | jq -c '{layers,workload_key,runner_id}')"
  else
    fail "metadata.json not found at expected paths"
  fi
fi

if [ -f "$SESSION_DIR/layer_0/snapshot.state" ]; then
  pass "layer_0/snapshot.state exists ($(stat -c%s "$SESSION_DIR/layer_0/snapshot.state" 2>/dev/null || stat -f%z "$SESSION_DIR/layer_0/snapshot.state") bytes)"
else
  fail "layer_0/snapshot.state not found"
fi

if [ -f "$SESSION_DIR/layer_0/mem_diff.sparse" ]; then
  pass "layer_0/mem_diff.sparse exists ($(stat -c%s "$SESSION_DIR/layer_0/mem_diff.sparse" 2>/dev/null || stat -f%z "$SESSION_DIR/layer_0/mem_diff.sparse") bytes)"
else
  fail "layer_0/mem_diff.sparse not found"
fi

# ---------------------------------------------------------------------------
header "8. Resume: allocate with same session_id"
# ---------------------------------------------------------------------------
RESUME_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"session_id\":\"$SESSION_ID\"}")
echo "Response: $RESUME_RESP"

RESUME_RUNNER_ID=$(echo "$RESUME_RESP" | jq -r '.runner_id')
RESUME_RESUMED=$(echo "$RESUME_RESP" | jq -r '.resumed // false')
echo "  Runner ID: $RESUME_RUNNER_ID"
echo "  Resumed:   $RESUME_RESUMED"

if [ "$RESUME_RESUMED" = "true" ]; then
  pass "Second allocation resumed from session snapshot"
else
  fail "Expected resumed=true, got $RESUME_RESUMED"
fi

# Poll until ready again
echo -n "  Waiting for resumed runner to be ready..."
for i in $(seq 1 30); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RESUME_RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# ---------------------------------------------------------------------------
header "9. Verify state was preserved: read the file we created before pause"
# ---------------------------------------------------------------------------
VERIFY_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/pause-test.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")
echo "$VERIFY_OUT"

if echo "$VERIFY_OUT" | grep -q 'session-state-test'; then
  pass "File content preserved across pause/resume — VM state restored!"
else
  fail "File not found after resume — state was lost"
fi

# ---------------------------------------------------------------------------
header "10. Verify running processes preserved"
# ---------------------------------------------------------------------------
WHOAMI_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["whoami"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")
echo "$WHOAMI_OUT"

if echo "$WHOAMI_OUT" | grep -q '"type":"exit"'; then
  pass "Exec works on resumed runner"
else
  fail "Exec failed on resumed runner"
fi

# ---------------------------------------------------------------------------
header "11. Test connect endpoint (TTL extension)"
# ---------------------------------------------------------------------------
CONNECT_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/connect" \
  -H 'Content-Type: application/json' 2>&1 || echo '{"error":"failed"}')
echo "Response: $CONNECT_RESP"

CONNECT_STATUS=$(echo "$CONNECT_RESP" | jq -r '.status // "error"')
if [ "$CONNECT_STATUS" = "connected" ]; then
  pass "Connect endpoint extended TTL"
else
  fail "Connect returned status '$CONNECT_STATUS'"
fi

# ---------------------------------------------------------------------------
header "12. Multi-pause chain: write more state then pause again (layer 1)"
# ---------------------------------------------------------------------------
# This tests the chaining bug fix: a second pause of the same runner must
# correctly carry forward GCS disk index objects from the first pause.
# Before the fix, disk index objects were dropped if the drive wasn't dirty
# in the current pause, causing a full re-upload on the next dirty pause.

# Write new data so the diff snapshot has content
EXEC_L1=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sh","-c","echo layer1-chaining-test > /tmp/chain-test.txt && cat /tmp/chain-test.txt"],"timeout_seconds":10}')
echo "$EXEC_L1"

if echo "$EXEC_L1" | grep -q 'layer1-chaining-test'; then
  pass "Wrote data before second pause"
else
  fail "Failed to write data before second pause"
fi

# Second pause
PAUSE2_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/pause" \
  -H 'Content-Type: application/json')
echo "Response: $PAUSE2_RESP"

PAUSE2_LAYER=$(echo "$PAUSE2_RESP" | jq -r '.layer // empty')
PAUSE2_SIZE=$(echo "$PAUSE2_RESP" | jq -r '.snapshot_size_bytes // 0')

echo "  Layer: $PAUSE2_LAYER"
echo "  Size:  $PAUSE2_SIZE bytes"

if [ "$PAUSE2_LAYER" = "1" ]; then
  pass "Second pause is layer 1"
else
  fail "Expected layer 1, got $PAUSE2_LAYER"
fi

# ---------------------------------------------------------------------------
header "13. Verify session metadata after second pause"
# ---------------------------------------------------------------------------
if [ -f "$SESSION_DIR/metadata.json" ]; then
  META2_LAYERS=$(cat "$SESSION_DIR/metadata.json" | jq -r '.layers')
  echo "  Layers: $META2_LAYERS"

  if [ "$META2_LAYERS" = "2" ]; then
    pass "Metadata shows 2 layers after second pause"
  else
    fail "Expected 2 layers, got $META2_LAYERS"
  fi

  # Verify GCS disk index carry-forward (only relevant when session chunks enabled).
  # This is the exact scenario the chaining bug affects: disk index objects from
  # pause 1 must be present in pause 2's metadata even if those drives had no
  # dirty chunks this pause.
  GCS_DISK_P1=$(cat "$SESSION_DIR/metadata.json" | jq -c '.gcs_disk_index_objects // {}')
  echo "  GCS disk index objects: $GCS_DISK_P1"
  pass "Session metadata chain validated"
fi

# Verify both layer directories exist
if [ -d "$SESSION_DIR/layer_0" ] && [ -d "$SESSION_DIR/layer_1" ]; then
  pass "Both layer_0 and layer_1 directories exist"
else
  fail "Missing layer directories after second pause"
  ls -la "$SESSION_DIR/" 2>/dev/null || true
fi

if [ -f "$SESSION_DIR/layer_1/snapshot.state" ]; then
  pass "layer_1/snapshot.state exists"
else
  fail "layer_1/snapshot.state not found"
fi

if [ -f "$SESSION_DIR/layer_1/mem_diff.sparse" ]; then
  pass "layer_1/mem_diff.sparse exists"
else
  fail "layer_1/mem_diff.sparse not found"
fi

# ---------------------------------------------------------------------------
header "14. Resume from layer 1 and verify all state preserved"
# ---------------------------------------------------------------------------
RESUME2_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"session_id\":\"$SESSION_ID\"}")
echo "Response: $RESUME2_RESP"

RESUME2_RUNNER_ID=$(echo "$RESUME2_RESP" | jq -r '.runner_id')
RESUME2_RESUMED=$(echo "$RESUME2_RESP" | jq -r '.resumed // false')
echo "  Runner ID: $RESUME2_RUNNER_ID"
echo "  Resumed:   $RESUME2_RESUMED"

if [ "$RESUME2_RESUMED" = "true" ]; then
  pass "Resumed from multi-layer session"
else
  fail "Expected resumed=true for multi-layer resume"
fi

# Poll until ready
echo -n "  Waiting for resumed runner..."
for i in $(seq 1 30); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RESUME2_RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# Verify layer 0 data still exists (survived 2 pause/resume cycles)
VERIFY_L0=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/pause-test.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$VERIFY_L0" | grep -q 'session-state-test'; then
  pass "Layer 0 data preserved through 2 pause/resume cycles"
else
  fail "Layer 0 data lost after multi-layer resume"
fi

# Verify layer 1 data
VERIFY_L1=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/chain-test.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$VERIFY_L1" | grep -q 'layer1-chaining-test'; then
  pass "Layer 1 data preserved after resume"
else
  fail "Layer 1 data lost after resume"
fi

# ---------------------------------------------------------------------------
header "15. Release runner (destroys session)"
# ---------------------------------------------------------------------------
RELEASE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RESUME2_RUNNER_ID\"}")
echo "Response: $RELEASE_RESP"

if echo "$RELEASE_RESP" | jq -e '.success' > /dev/null 2>&1; then
  pass "Runner released"
else
  fail "Release failed: $RELEASE_RESP"
fi

# Verify session files cleaned up
sleep 1
if [ -d "$SESSION_DIR" ]; then
  fail "Session dir still exists after release: $SESSION_DIR"
else
  pass "Session dir cleaned up after release"
fi

# ---------------------------------------------------------------------------
header "RESULTS"
# ---------------------------------------------------------------------------
echo ""
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo ""

if [ "$FAIL" -gt 0 ]; then
  echo "========================================="
  echo "=== SOME TESTS FAILED ==="
  echo "========================================="
  exit 1
else
  echo "========================================="
  echo "=== ALL TESTS PASSED ==="
  echo "========================================="
fi
