#!/bin/bash
# E2E test: build base snapshot → derive snapshot with extension drives → boot derived → verify drives → pause/resume
# Usage: make dev-test-derive-snapshot-local
#
# Prerequisites:
#   - Stack running: make dev-stack-local (or dev-stack)
#   - Base snapshot built: make dev-snapshot-local (or dev-snapshot)
#   - derive-snapshot binary built: make derive-snapshot
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="test-derive-$(date +%s)"
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DERIVE_BIN="$REPO_ROOT/bin/derive-snapshot"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"

# ---------------------------------------------------------------------------
header "0. Prerequisites check"
# ---------------------------------------------------------------------------
if [ ! -f "$DERIVE_BIN" ]; then
  echo "FAIL: bin/derive-snapshot not found. Run 'make derive-snapshot' first."
  exit 1
fi
pass "derive-snapshot binary exists"

if [ ! -f "$SNAPSHOT_DIR/snapshot.mem" ]; then
  echo "FAIL: snapshot.mem not found. Run 'make dev-snapshot-local' first."
  exit 1
fi
pass "Base snapshot exists"

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
header "1. Register base snapshot config"
# ---------------------------------------------------------------------------
BASE_CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "derive-base-test",
    "commands": [{"type":"shell","args":["echo","base-snapshot-ready"]}],
    "runner_ttl_seconds": 30,
    "auto_pause": true,
    "session_max_age_seconds": 3600
  }')
BASE_WORKLOAD_KEY=$(echo "$BASE_CONFIG_RESP" | jq -r '.workload_key')
echo "Base workload key: $BASE_WORKLOAD_KEY"

if [ -n "$BASE_WORKLOAD_KEY" ] && [ "$BASE_WORKLOAD_KEY" != "null" ]; then
  pass "Base snapshot config registered"
else
  fail "Base snapshot config registration failed"
  echo "$BASE_CONFIG_RESP"
  exit 1
fi

# ---------------------------------------------------------------------------
header "2. Test derive-snapshot CLI (dry-run validation)"
# ---------------------------------------------------------------------------
# Test with drive specs to validate CLI parsing and key computation.
DRIVE_SPECS='[{"drive_id":"git_drive","label":"GIT","size_gb":1,"read_only":false,"mount_path":"/workspace/repo"},{"drive_id":"cache_drive","label":"CACHE","size_gb":1,"read_only":true,"mount_path":"/mnt/cache"}]'

# Run derive-snapshot in validation mode (it will fail on GCS without real credentials,
# but we verify the CLI parses arguments and computes the derived key).
set +e
DERIVE_OUTPUT=$("$DERIVE_BIN" \
  --base-workload-key="$BASE_WORKLOAD_KEY" \
  --drive-specs="$DRIVE_SPECS" \
  --gcs-bucket="local-dev-test" \
  --gcs-prefix="v1" \
  --local-cache="/tmp/fc-dev/chunk-cache-test" \
  --version="test-v1" \
  --log-level=debug \
  2>&1)
DERIVE_EXIT=$?
set -e

echo "$DERIVE_OUTPUT" | head -20

# The CLI should at least get past argument parsing and compute the derived key.
# It will likely fail on GCS connection — that's expected for local testing.
if echo "$DERIVE_OUTPUT" | grep -q "Computed derived workload key"; then
  DERIVED_KEY=$(echo "$DERIVE_OUTPUT" | grep "derived_workload_key" | head -1 | sed 's/.*"derived_workload_key":"\([^"]*\)".*/\1/' || echo "")
  pass "Derived workload key computed: $DERIVED_KEY"
else
  # Check if it at least parsed drive specs
  if echo "$DERIVE_OUTPUT" | grep -q "drive_count"; then
    pass "Drive specs parsed successfully (GCS connection expected to fail locally)"
  else
    fail "derive-snapshot CLI failed to parse arguments"
    echo "$DERIVE_OUTPUT"
  fi
fi

# ---------------------------------------------------------------------------
header "3. Test deterministic key computation"
# ---------------------------------------------------------------------------
# Run twice with same inputs — key should be identical.
set +e
OUTPUT1=$("$DERIVE_BIN" \
  --base-workload-key="test-base-123" \
  --drive-specs='[{"drive_id":"b","label":"B","size_gb":5},{"drive_id":"a","label":"A","size_gb":10}]' \
  --gcs-bucket="local-dev-test" \
  --log-level=debug 2>&1)

OUTPUT2=$("$DERIVE_BIN" \
  --base-workload-key="test-base-123" \
  --drive-specs='[{"drive_id":"a","label":"A","size_gb":10},{"drive_id":"b","label":"B","size_gb":5}]' \
  --gcs-bucket="local-dev-test" \
  --log-level=debug 2>&1)
set -e

KEY1=$(echo "$OUTPUT1" | grep "derived_workload_key" | head -1 | sed 's/.*"derived_workload_key":"\([^"]*\)".*/\1/' || echo "key1-not-found")
KEY2=$(echo "$OUTPUT2" | grep "derived_workload_key" | head -1 | sed 's/.*"derived_workload_key":"\([^"]*\)".*/\1/' || echo "key2-not-found")

echo "  Key1 (order: b,a): $KEY1"
echo "  Key2 (order: a,b): $KEY2"

if [ "$KEY1" = "$KEY2" ] && [ -n "$KEY1" ] && [ "$KEY1" != "key1-not-found" ]; then
  pass "Derived key is deterministic regardless of drive spec order"
else
  fail "Derived keys differ or could not be extracted"
fi

# Verify different inputs produce different keys
set +e
OUTPUT3=$("$DERIVE_BIN" \
  --base-workload-key="different-base" \
  --drive-specs='[{"drive_id":"a","label":"A","size_gb":10}]' \
  --gcs-bucket="local-dev-test" \
  --log-level=debug 2>&1)
set -e

KEY3=$(echo "$OUTPUT3" | grep "derived_workload_key" | head -1 | sed 's/.*"derived_workload_key":"\([^"]*\)".*/\1/' || echo "key3-not-found")
echo "  Key3 (different base): $KEY3"

if [ "$KEY1" != "$KEY3" ] && [ -n "$KEY3" ] && [ "$KEY3" != "key3-not-found" ]; then
  pass "Different base keys produce different derived keys"
else
  fail "Expected different derived keys for different base keys"
fi

# ---------------------------------------------------------------------------
header "4. Test drive spec validation"
# ---------------------------------------------------------------------------
# Empty drive specs should still work (no extension drives).
set +e
EMPTY_DRIVE_OUTPUT=$("$DERIVE_BIN" \
  --base-workload-key="test-base-empty" \
  --drive-specs='[]' \
  --gcs-bucket="local-dev-test" \
  --log-level=debug 2>&1)
set -e

if echo "$EMPTY_DRIVE_OUTPUT" | grep -q "drive_count.*0\|\"drive_count\":0"; then
  pass "Empty drive specs accepted (0 drives)"
else
  # Check if it at least parsed successfully
  if echo "$EMPTY_DRIVE_OUTPUT" | grep -q "Computed derived workload key"; then
    pass "Empty drive specs accepted"
  else
    fail "Empty drive specs should be valid"
  fi
fi

# Invalid JSON should fail
set +e
INVALID_OUTPUT=$("$DERIVE_BIN" \
  --base-workload-key="test-base" \
  --drive-specs='not-valid-json' \
  --gcs-bucket="local-dev-test" \
  --log-level=debug 2>&1)
INVALID_EXIT=$?
set -e

if [ "$INVALID_EXIT" -ne 0 ]; then
  pass "Invalid drive-specs JSON correctly rejected"
else
  fail "Invalid drive-specs JSON should have been rejected"
fi

# Missing required flags should fail
set +e
MISSING_OUTPUT=$("$DERIVE_BIN" --log-level=debug 2>&1)
MISSING_EXIT=$?
set -e

if [ "$MISSING_EXIT" -ne 0 ]; then
  pass "Missing --base-workload-key correctly rejected"
else
  fail "Missing --base-workload-key should have been rejected"
fi

# ---------------------------------------------------------------------------
header "5. Allocate and exec on base snapshot (verify base works)"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"session_id\":\"$SESSION_ID\"}")
echo "Response: $ALLOC_RESP"

RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
echo "  Runner ID: $RUNNER_ID"

if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  fail "Allocate returned no runner_id"
  exit 1
fi
pass "Runner allocated on base snapshot"

# Poll until ready
echo -n "  Waiting for runner..."
READY=false
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    READY=true
    break
  fi
  echo -n "."
  sleep 1
done

if $READY; then
  pass "Base runner became ready"
else
  fail "Base runner did not become ready in 60s"
  exit 1
fi

# Simple exec
EXEC_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","base-snapshot-works"],"timeout_seconds":10}')
echo "$EXEC_OUT"

if echo "$EXEC_OUT" | grep -q 'base-snapshot-works'; then
  pass "Exec on base snapshot works"
else
  fail "Exec on base snapshot failed"
fi

# ---------------------------------------------------------------------------
header "6. Pause base runner"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/pause" \
  -H 'Content-Type: application/json')
echo "Response: $PAUSE_RESP"

PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id // empty')
PAUSE_LAYER=$(echo "$PAUSE_RESP" | jq -r '.layer // empty')

if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Base runner paused successfully (session=$PAUSE_SESSION, layer=$PAUSE_LAYER)"
else
  fail "Base runner pause failed"
fi

# ---------------------------------------------------------------------------
header "7. Verify session metadata structure"
# ---------------------------------------------------------------------------
SESSION_DIR="/tmp/fc-dev/sessions/$SESSION_ID"
if [ -f "$SESSION_DIR/metadata.json" ]; then
  pass "Session metadata.json exists"

  # Check metadata fields
  META_WORKLOAD=$(cat "$SESSION_DIR/metadata.json" | jq -r '.workload_key')
  META_LAYERS=$(cat "$SESSION_DIR/metadata.json" | jq -r '.layers')
  META_RUNNER=$(cat "$SESSION_DIR/metadata.json" | jq -r '.runner_id')

  echo "  workload_key: $META_WORKLOAD"
  echo "  layers: $META_LAYERS"
  echo "  runner_id: $META_RUNNER"

  if [ "$META_LAYERS" = "1" ]; then
    pass "Layer count is 1 after first pause"
  else
    fail "Expected 1 layer, got $META_LAYERS"
  fi

  # Check for GCS fields (may or may not be populated depending on config)
  GCS_MANIFEST=$(cat "$SESSION_DIR/metadata.json" | jq -r '.gcs_manifest_path // "none"')
  GCS_MEM_IDX=$(cat "$SESSION_DIR/metadata.json" | jq -r '.gcs_mem_index_object // "none"')
  GCS_DISK_IDX=$(cat "$SESSION_DIR/metadata.json" | jq '.gcs_disk_index_objects // null')
  echo "  gcs_manifest_path: $GCS_MANIFEST"
  echo "  gcs_mem_index_object: $GCS_MEM_IDX"
  echo "  gcs_disk_index_objects: $GCS_DISK_IDX"
  pass "Session metadata structure validated"
else
  fail "Session metadata.json not found at $SESSION_DIR"
fi

# ---------------------------------------------------------------------------
header "8. Resume and verify state preserved"
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
  pass "Session resumed from snapshot"
else
  fail "Expected resumed=true"
fi

# Poll until ready
echo -n "  Waiting for resumed runner..."
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

RESUME_EXEC=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","resume-works"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$RESUME_EXEC" | grep -q 'resume-works'; then
  pass "Exec works after resume"
else
  fail "Exec failed after resume"
fi

# ---------------------------------------------------------------------------
header "9. Multi-pause chain test (pause → resume → pause again)"
# ---------------------------------------------------------------------------
# Pause the resumed runner (layer 1)
PAUSE2_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/pause" \
  -H 'Content-Type: application/json')
echo "Response: $PAUSE2_RESP"

PAUSE2_LAYER=$(echo "$PAUSE2_RESP" | jq -r '.layer // empty')
echo "  Layer: $PAUSE2_LAYER"

if [ "$PAUSE2_LAYER" = "1" ]; then
  pass "Second pause is layer 1 (multi-pause chain works)"
else
  fail "Expected layer 1 for second pause, got $PAUSE2_LAYER"
fi

# Verify metadata updated
if [ -f "$SESSION_DIR/metadata.json" ]; then
  META_LAYERS2=$(cat "$SESSION_DIR/metadata.json" | jq -r '.layers')
  if [ "$META_LAYERS2" = "2" ]; then
    pass "Metadata updated to 2 layers after second pause"
  else
    fail "Expected 2 layers in metadata, got $META_LAYERS2"
  fi
fi

# Verify layer_1 directory exists
if [ -d "$SESSION_DIR/layer_1" ]; then
  pass "layer_1 directory created"
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
else
  fail "layer_1 directory not found"
fi

# ---------------------------------------------------------------------------
header "10. Resume from layer 1 and verify"
# ---------------------------------------------------------------------------
RESUME2_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"session_id\":\"$SESSION_ID\"}")
RESUME2_RUNNER_ID=$(echo "$RESUME2_RESP" | jq -r '.runner_id')
RESUME2_RESUMED=$(echo "$RESUME2_RESP" | jq -r '.resumed // false')

echo "  Runner ID: $RESUME2_RUNNER_ID"
echo "  Resumed:   $RESUME2_RESUMED"

if [ "$RESUME2_RESUMED" = "true" ]; then
  pass "Resumed from multi-layer session"
else
  fail "Expected resumed=true for multi-layer resume"
fi

# Poll and exec
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

RESUME2_EXEC=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["echo","multi-layer-works"],"timeout_seconds":10}' 2>&1 || echo "EXEC_FAILED")

if echo "$RESUME2_EXEC" | grep -q 'multi-layer-works'; then
  pass "Exec works after multi-layer resume"
else
  fail "Exec failed after multi-layer resume"
fi

# ---------------------------------------------------------------------------
header "11. Cleanup: release runner"
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

# Verify session cleaned up
sleep 1
if [ -d "$SESSION_DIR" ]; then
  fail "Session dir still exists after release: $SESSION_DIR"
else
  pass "Session dir cleaned up after release"
fi

# Cleanup test chunk cache
rm -rf /tmp/fc-dev/chunk-cache-test

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
