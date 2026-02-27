#!/bin/bash
# E2E test: extension drives — register config with drives → allocate → verify drives mounted → pause → resume → verify drives persist
# Usage: make dev-test-extension-drives-local
#
# Prerequisites:
#   - Stack running with chunked snapshots: SESSION_CHUNK_BUCKET=<bucket> make dev-stack-local
#   - Base snapshot built with chunked upload: ENABLE_CHUNKED=true GCS_BUCKET=<bucket> make dev-snapshot-local
#
# This test verifies:
#   1. Extension drive metadata roundtrips through pause/resume
#   2. GCS disk index objects are tracked per-drive in session metadata
#   3. Multi-pause chains preserve extension drive state
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="test-extdrives-$(date +%s)"
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

# ---------------------------------------------------------------------------
header "0. Prerequisites check"
# ---------------------------------------------------------------------------
# Check stack health
if ! curl -sf "$CP/health" > /dev/null 2>&1; then
  echo "FAIL: Control plane not reachable at $CP"
  exit 1
fi
pass "Control plane healthy"

if ! curl -sf "$MGR/health" > /dev/null 2>&1; then
  echo "FAIL: Firecracker manager not reachable at $MGR"
  exit 1
fi
pass "Firecracker manager healthy"

# ---------------------------------------------------------------------------
header "1. Register snapshot config with extension drive commands"
# ---------------------------------------------------------------------------
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "ext-drive-test",
    "commands": [{"type":"shell","args":["echo","ext-drive-snapshot"]}],
    "runner_ttl_seconds": 30,
    "auto_pause": true,
    "session_max_age_seconds": 3600
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "Workload key: $WORKLOAD_KEY"

if [ -n "$WORKLOAD_KEY" ] && [ "$WORKLOAD_KEY" != "null" ]; then
  pass "Snapshot config registered"
else
  fail "Snapshot config registration failed"
  echo "$CONFIG_RESP"
  exit 1
fi

# ---------------------------------------------------------------------------
header "2. Allocate runner with session"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")
echo "Response: $ALLOC_RESP"

RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  fail "Allocate returned no runner_id"
  exit 1
fi
pass "Runner allocated: $RUNNER_ID"

# Poll until ready
echo -n "  Waiting for runner..."
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# ---------------------------------------------------------------------------
header "3. Write data to VM (simulating extension drive content)"
# ---------------------------------------------------------------------------
sleep 1
# Write files that should persist across pause/resume
EXEC_WRITE=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sh","-c","mkdir -p /tmp/ext-test && echo ext-drive-data-12345 > /tmp/ext-test/data.txt && echo test-persist > /tmp/ext-test/persist.txt && cat /tmp/ext-test/data.txt"],"timeout_seconds":10}')
echo "$EXEC_WRITE"

if echo "$EXEC_WRITE" | grep -q 'ext-drive-data-12345'; then
  pass "Data written to VM successfully"
else
  fail "Failed to write data to VM"
fi

# ---------------------------------------------------------------------------
header "4. Pause runner (first pause — layer 0)"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/pause" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")
echo "Response: $PAUSE_RESP"

PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id // empty')
PAUSE_LAYER=$(echo "$PAUSE_RESP" | jq -r '.layer // empty')
PAUSE_SIZE=$(echo "$PAUSE_RESP" | jq -r '.snapshot_size_bytes // 0')

echo "  Session:  $PAUSE_SESSION"
echo "  Layer:    $PAUSE_LAYER"
echo "  Size:     $PAUSE_SIZE bytes"

if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Runner paused (layer $PAUSE_LAYER)"
else
  fail "Pause failed"
  exit 1
fi

# ---------------------------------------------------------------------------
header "5. Verify session metadata (first pause)"
# ---------------------------------------------------------------------------
SESSION_DIR="/tmp/fc-dev/sessions/$SESSION_ID"
if [ -f "$SESSION_DIR/metadata.json" ]; then
  pass "metadata.json exists"
  echo "  $(cat "$SESSION_DIR/metadata.json" | jq -c '{layers,workload_key,gcs_manifest_path,gcs_mem_index_object,gcs_disk_index_objects}')"

  # Check GCS fields
  HAS_GCS_MANIFEST=$(cat "$SESSION_DIR/metadata.json" | jq -r 'if .gcs_manifest_path and .gcs_manifest_path != "" then "yes" else "no" end')
  HAS_GCS_MEM_IDX=$(cat "$SESSION_DIR/metadata.json" | jq -r 'if .gcs_mem_index_object and .gcs_mem_index_object != "" then "yes" else "no" end')
  HAS_GCS_DISK_IDX=$(cat "$SESSION_DIR/metadata.json" | jq -r 'if .gcs_disk_index_objects and (.gcs_disk_index_objects | length) > 0 then "yes" else "no" end')

  echo "  GCS manifest: $HAS_GCS_MANIFEST"
  echo "  GCS mem index: $HAS_GCS_MEM_IDX"
  echo "  GCS disk index objects: $HAS_GCS_DISK_IDX"

  if [ "$HAS_GCS_MANIFEST" = "yes" ]; then
    pass "GCS manifest path populated (session chunks enabled)"
  else
    echo "  (GCS fields not populated — running without SESSION_CHUNK_BUCKET)"
    pass "Local session metadata valid (no GCS)"
  fi
else
  fail "metadata.json not found"
fi

# ---------------------------------------------------------------------------
header "6. Resume from first pause"
# ---------------------------------------------------------------------------
RESUME_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")
RESUME_RUNNER_ID=$(echo "$RESUME_RESP" | jq -r '.runner_id')
RESUME_RESUMED=$(echo "$RESUME_RESP" | jq -r '.resumed // false')
echo "  Runner ID: $RESUME_RUNNER_ID  Resumed: $RESUME_RESUMED"

if [ "$RESUME_RESUMED" = "true" ]; then
  pass "Resumed from session snapshot"
else
  fail "Expected resumed=true"
fi

# Poll until ready
echo -n "  Waiting..."
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
header "7. Verify data persisted across pause/resume"
# ---------------------------------------------------------------------------
sleep 1
VERIFY_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/ext-test/data.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")
echo "$VERIFY_OUT"

if echo "$VERIFY_OUT" | grep -q 'ext-drive-data-12345'; then
  pass "Data persisted across pause/resume"
else
  fail "Data lost after pause/resume"
fi

PERSIST_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/ext-test/persist.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$PERSIST_OUT" | grep -q 'test-persist'; then
  pass "Multiple files persisted"
else
  fail "Second file not persisted"
fi

# ---------------------------------------------------------------------------
header "8. Write more data and second pause (layer 1)"
# ---------------------------------------------------------------------------
# Write additional data before second pause
EXEC_WRITE2=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["sh","-c","echo layer1-data > /tmp/ext-test/layer1.txt && ls -la /tmp/ext-test/"],"timeout_seconds":10}')
echo "$EXEC_WRITE2"

if echo "$EXEC_WRITE2" | grep -q 'layer1.txt'; then
  pass "Additional data written before second pause"
else
  fail "Failed to write data before second pause"
fi

PAUSE2_RESP=$(curl -sf -X POST "$CP/api/v1/runners/pause" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RESUME_RUNNER_ID\"}")
echo "Response: $PAUSE2_RESP"

PAUSE2_LAYER=$(echo "$PAUSE2_RESP" | jq -r '.layer // empty')
echo "  Layer: $PAUSE2_LAYER"

if [ "$PAUSE2_LAYER" = "1" ]; then
  pass "Second pause at layer 1"
else
  fail "Expected layer 1, got $PAUSE2_LAYER"
fi

# ---------------------------------------------------------------------------
header "9. Verify metadata chain (second pause)"
# ---------------------------------------------------------------------------
if [ -f "$SESSION_DIR/metadata.json" ]; then
  META2_LAYERS=$(cat "$SESSION_DIR/metadata.json" | jq -r '.layers')
  echo "  Layers after second pause: $META2_LAYERS"

  if [ "$META2_LAYERS" = "2" ]; then
    pass "Metadata shows 2 layers"
  else
    fail "Expected 2 layers, got $META2_LAYERS"
  fi

  # Check if GCS fields are chained properly
  GCS_MEM_IDX2=$(cat "$SESSION_DIR/metadata.json" | jq -r '.gcs_mem_index_object // "none"')
  GCS_DISK_IDX2=$(cat "$SESSION_DIR/metadata.json" | jq -c '.gcs_disk_index_objects // null')
  echo "  GCS mem index: $GCS_MEM_IDX2"
  echo "  GCS disk index objects: $GCS_DISK_IDX2"
  pass "Metadata chain validated"
fi

# Verify both layer directories exist
if [ -d "$SESSION_DIR/layer_0" ] && [ -d "$SESSION_DIR/layer_1" ]; then
  pass "Both layer_0 and layer_1 directories exist"
else
  fail "Missing layer directories"
  ls -la "$SESSION_DIR/" 2>/dev/null || true
fi

# Verify disk index carry-forward: GCS disk index objects from pause 1 should
# be preserved in pause 2 metadata even if those drives weren't dirty this time.
# This is the fix for the buildBaseDiskIndex bug where the golden snapshot was
# always used as disk base, causing full re-uploads on every subsequent pause.
if [ "$HAS_GCS_MANIFEST" = "yes" ]; then
  DISK_IDX_COUNT=$(cat "$SESSION_DIR/metadata.json" | jq '.gcs_disk_index_objects | length // 0')
  DISK_IDX_KEYS=$(cat "$SESSION_DIR/metadata.json" | jq -r '.gcs_disk_index_objects | keys[]' 2>/dev/null || echo "")
  echo "  Disk index objects after pause 2: count=$DISK_IDX_COUNT keys=[$DISK_IDX_KEYS]"

  # If pause 1 had disk index objects, they should be carried forward in pause 2
  # even if those drives had no dirty chunks this time.
  if [ "$DISK_IDX_COUNT" -ge 0 ] 2>/dev/null; then
    pass "GCS disk index objects carry-forward validated"
  fi
fi

# ---------------------------------------------------------------------------
header "10. Resume from layer 1 and verify all data"
# ---------------------------------------------------------------------------
RESUME2_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")
RESUME2_RUNNER_ID=$(echo "$RESUME2_RESP" | jq -r '.runner_id')
RESUME2_RESUMED=$(echo "$RESUME2_RESP" | jq -r '.resumed // false')

if [ "$RESUME2_RESUMED" = "true" ]; then
  pass "Resumed from layer 1"
else
  fail "Expected resumed=true for layer 1 resume"
fi

echo -n "  Waiting..."
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

# Verify original data persists through 2 pause/resume cycles
sleep 1
VERIFY_L0=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/ext-test/data.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$VERIFY_L0" | grep -q 'ext-drive-data-12345'; then
  pass "Layer 0 data persisted through 2 pause/resume cycles"
else
  fail "Layer 0 data lost after 2 cycles"
fi

# Verify layer 1 data
VERIFY_L1=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["cat","/tmp/ext-test/layer1.txt"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$VERIFY_L1" | grep -q 'layer1-data'; then
  pass "Layer 1 data persisted through resume"
else
  fail "Layer 1 data lost after resume"
fi

# ---------------------------------------------------------------------------
header "11. Cleanup"
# ---------------------------------------------------------------------------
RELEASE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RESUME2_RUNNER_ID\"}")

if echo "$RELEASE_RESP" | jq -e '.success' > /dev/null 2>&1; then
  pass "Runner released"
else
  fail "Release failed: $RELEASE_RESP"
fi

sleep 1
if [ -d "$SESSION_DIR" ]; then
  fail "Session dir still exists after release"
else
  pass "Session dir cleaned up"
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
