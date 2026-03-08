#!/bin/bash
# E2E test: Multi-pause session with chunk dedup verification
#
# Tests the full multi-pause flow and verifies chunk dedup across pauses:
#   allocate(session) → exec(create state) → pause1(→GCS)
#     → inspect GCS artifacts
#     → resume(←GCS) → exec(create more state) → pause2(→GCS)
#     → compare chunk indexes (verify dedup: pause2 only uploads new dirty chunks)
#     → resume(←GCS) → verify ALL state from both pauses
#
# Usage:
#   SESSION_CHUNK_BUCKET=rroy-gc-testing make dev-test-multi-pause-dedup
#
# Prerequisites:
#   - Golden chunked snapshot uploaded: GCS_BUCKET=<bucket> ENABLE_CHUNKED=true make dev-snapshot
#   - Stack running with GCS sessions:  SESSION_CHUNK_BUCKET=<bucket> make dev-stack
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="dedup-e2e-$(date +%s)"
GCS_BUCKET=${SESSION_CHUNK_BUCKET:-}
PASS=0
FAIL=0
VERBOSE=${VERBOSE:-0}

# Colors for terminal output
RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m' # No Color

pass() { echo -e "  ${GREEN}✓${NC} $1"; PASS=$((PASS + 1)); }
fail() { echo -e "  ${RED}✗${NC} $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo -e "${BOLD}=== $1 ===${NC}"; }
info() { echo -e "  ${CYAN}→${NC} $1"; }
inspect() { echo -e "  ${YELLOW}⊳${NC} $1"; }

# Dump JSON nicely, optionally filtering with jq expression
dump_json() {
  local label="$1"
  local data="$2"
  local filter="${3:-.}"
  echo -e "  ${YELLOW}⊳ ${label}:${NC}"
  echo "$data" | jq "$filter" 2>/dev/null | sed 's/^/    /'
}

# Helper: exec inside VM
vm_exec() {
  local runner="$1"
  local cmd="$2"
  curl -s --no-buffer --max-time 30 -X POST "$MGR/api/v1/runners/$runner/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":[\"sh\",\"-c\",\"$cmd\"],\"timeout_seconds\":20}"
}

# Helper: wait for runner ready
wait_ready() {
  local runner_id="$1"
  local label="${2:-Runner}"
  local max_wait="${3:-60}"
  echo -n "  Waiting for $label..."
  for i in $(seq 1 "$max_wait"); do
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
      "$CP/api/v1/runners/status?runner_id=$runner_id")
    if [ "$HTTP_CODE" = "200" ]; then
      echo " ready (${i}s)"
      return 0
    fi
    echo -n "."
    sleep 1
  done
  echo " TIMEOUT (${max_wait}s)"
  return 1
}

# Helper: wait for exec to become responsive after resume
wait_exec_ready() {
  local runner_id="$1"
  local max_wait="${2:-30}"
  echo -n "  Waiting for exec..."
  for i in $(seq 1 "$max_wait"); do
    PRECHECK=$(curl -s --no-buffer --max-time 5 -X POST "$MGR/api/v1/runners/$runner_id/exec" \
      -H 'Content-Type: application/json' \
      -d '{"command":["echo","exec-alive"],"timeout_seconds":3}' 2>&1 || echo "EXEC_TIMEOUT")
    if echo "$PRECHECK" | grep -q 'exec-alive'; then
      echo " responsive (${i}s)"
      return 0
    fi
    echo -n "."
    sleep 1
  done
  echo " TIMEOUT (${max_wait}s)"
  return 1
}

# Helper: resolve session directory
find_session_dir() {
  local sid="$1"
  local dir="/tmp/fc-dev/sessions/$sid"
  if [ -f "$dir/metadata.json" ]; then
    echo "$dir"
    return
  fi
  dir="$(dirname /tmp/fc-dev/snapshots)/sessions/$sid"
  if [ -f "$dir/metadata.json" ]; then
    echo "$dir"
    return
  fi
  echo ""
}

# =====================================================================
# Validation
# =====================================================================
if [ -z "$GCS_BUCKET" ]; then
  echo -e "${RED}FAIL: SESSION_CHUNK_BUCKET is required.${NC}"
  echo "Usage: SESSION_CHUNK_BUCKET=your-bucket make dev-test-multi-pause-dedup"
  exit 1
fi

echo -e "${BOLD}Multi-Pause Chunk Dedup E2E Test${NC}"
echo "  GCS bucket:  $GCS_BUCKET"
echo "  Session ID:  $SESSION_ID"

# =====================================================================
header "1. Register snapshot config"
# =====================================================================
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}
CONFIG_RESP=$(curl -s -X POST "$CP/api/v1/layered-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "multi-pause-dedup-test",
    "layers": [{"name":"base","init_commands":'"$SNAPSHOT_COMMANDS"'}],
    "config": {
      "ttl": 300,
      "auto_pause": true,
      "session_max_age_seconds": 3600
    }
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.leaf_workload_key')
info "workload_key=$WORKLOAD_KEY"

if [ -n "$WORKLOAD_KEY" ] && [ "$WORKLOAD_KEY" != "null" ]; then
  pass "Snapshot config registered"
else
  fail "Snapshot config registration failed: $CONFIG_RESP"
  exit 1
fi

# =====================================================================
header "2. Allocate runner with session_id"
# =====================================================================
ALLOC_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")

RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  fail "Allocate returned no runner_id"
  dump_json "Response" "$ALLOC_RESP"
  exit 1
fi
pass "Runner allocated: $RUNNER_ID"

if ! wait_ready "$RUNNER_ID" "initial boot"; then
  fail "Runner did not become ready"
  exit 1
fi
pass "Runner ready"

# =====================================================================
header "3. Create state BEFORE pause 1"
# =====================================================================

# 3a. Memory marker (tmpfs)
info "Writing memory marker to tmpfs..."
OUT=$(vm_exec "$RUNNER_ID" "echo PAUSE1-MEM-MARKER > /tmp/p1-mem.txt && cat /tmp/p1-mem.txt")
if echo "$OUT" | grep -q 'PAUSE1-MEM-MARKER'; then
  pass "Pause 1 memory marker written"
else
  fail "Pause 1 memory marker failed"
fi

# 3b. Disk marker (rootfs)
info "Writing disk marker to rootfs..."
OUT=$(vm_exec "$RUNNER_ID" "echo PAUSE1-DISK-MARKER > /var/tmp/p1-disk.txt && cat /var/tmp/p1-disk.txt")
if echo "$OUT" | grep -q 'PAUSE1-DISK-MARKER'; then
  pass "Pause 1 disk marker written"
else
  fail "Pause 1 disk marker failed"
fi

# 3c. Larger file for chunk coverage
info "Writing 2MB file to tmpfs (spans partial chunk)..."
OUT=$(vm_exec "$RUNNER_ID" "dd if=/dev/urandom of=/tmp/p1-bigfile.bin bs=1K count=2048 2>/dev/null && md5sum /tmp/p1-bigfile.bin")
P1_BIGFILE_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1)
if [ -n "$P1_BIGFILE_MD5" ]; then
  pass "2MB file written (md5=$P1_BIGFILE_MD5)"
else
  fail "2MB file write failed"
fi

# =====================================================================
header "4. PAUSE 1 → GCS"
# =====================================================================
PAUSE1_RESP=$(curl -s -X POST "$CP/api/v1/runners/pause" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")

PAUSE1_SESSION=$(echo "$PAUSE1_RESP" | jq -r '.session_id // empty')
PAUSE1_LAYER=$(echo "$PAUSE1_RESP" | jq -r '.layer // empty')
PAUSE1_SIZE=$(echo "$PAUSE1_RESP" | jq -r '.snapshot_size_bytes // 0')

dump_json "Pause 1 response" "$PAUSE1_RESP"

if [ -n "$PAUSE1_SESSION" ] && [ "$PAUSE1_SESSION" != "null" ]; then
  pass "Pause 1 succeeded (layer=$PAUSE1_LAYER, size=$PAUSE1_SIZE)"
else
  fail "Pause 1 failed"
  exit 1
fi

# =====================================================================
header "5. INSPECT: Pause 1 artifacts"
# =====================================================================
SESSION_DIR=$(find_session_dir "$SESSION_ID")

if [ -z "$SESSION_DIR" ]; then
  fail "Session directory not found"
  exit 1
fi
pass "Session dir: $SESSION_DIR"

# 5a. metadata.json
info "Reading metadata.json..."
P1_METADATA=$(cat "$SESSION_DIR/metadata.json")
dump_json "metadata.json" "$P1_METADATA" '{session_id, workload_key, runner_id, layers, gcs_manifest_path, gcs_mem_index_object, gcs_disk_index_objects}'

P1_GCS_MANIFEST=$(echo "$P1_METADATA" | jq -r '.gcs_manifest_path // empty')
P1_GCS_MEM_IDX=$(echo "$P1_METADATA" | jq -r '.gcs_mem_index_object // empty')

if [ -n "$P1_GCS_MANIFEST" ] && [ "$P1_GCS_MANIFEST" != "null" ]; then
  pass "Pause 1 has GCS manifest: $P1_GCS_MANIFEST"
else
  fail "No GCS manifest after pause 1"
  exit 1
fi

# 5b. Download and inspect the GCS manifest
info "Downloading GCS manifest..."
P1_MANIFEST_JSON=$(gsutil cat "gs://$GCS_BUCKET/$P1_GCS_MANIFEST" 2>/dev/null || echo "DOWNLOAD_FAILED")
if [ "$P1_MANIFEST_JSON" = "DOWNLOAD_FAILED" ]; then
  fail "Could not download GCS manifest"
else
  pass "GCS manifest downloaded"
  dump_json "snapshot_manifest.json" "$P1_MANIFEST_JSON" \
    '{version, snapshot_id, workload_key, memory: {mode, total_size_bytes, chunk_index_object}, extension_disks}'
fi

# 5c. Download and inspect the mem chunk index
info "Downloading mem chunk index..."
P1_MEM_IDX_JSON=$(gsutil cat "gs://$GCS_BUCKET/$P1_GCS_MEM_IDX" 2>/dev/null || echo "DOWNLOAD_FAILED")
if [ "$P1_MEM_IDX_JSON" = "DOWNLOAD_FAILED" ]; then
  fail "Could not download mem chunk index"
else
  P1_MEM_EXTENTS=$(echo "$P1_MEM_IDX_JSON" | jq '.region.extents | length')
  P1_CHUNK_SIZE=$(echo "$P1_MEM_IDX_JSON" | jq '.chunk_size_bytes')
  P1_LOGICAL_SIZE=$(echo "$P1_MEM_IDX_JSON" | jq '.region.logical_size_bytes')
  pass "Pause 1 mem index: $P1_MEM_EXTENTS extents, chunk_size=$P1_CHUNK_SIZE, logical=$P1_LOGICAL_SIZE"

  # Save the set of chunk hashes for dedup comparison later
  P1_MEM_HASHES=$(echo "$P1_MEM_IDX_JSON" | jq -r '.region.extents[].hash' | sort)
  P1_MEM_UNIQUE_HASHES=$(echo "$P1_MEM_HASHES" | sort -u | wc -l | tr -d ' ')
  P1_MEM_TOTAL_HASHES=$(echo "$P1_MEM_HASHES" | wc -l | tr -d ' ')
  inspect "Pause 1 mem: $P1_MEM_TOTAL_HASHES total extents, $P1_MEM_UNIQUE_HASHES unique hashes"
fi

# 5d. Inspect extension disk indexes if present
P1_EXT_DISKS=$(echo "$P1_METADATA" | jq -r '.gcs_disk_index_objects // {} | keys[]' 2>/dev/null || true)
if [ -n "$P1_EXT_DISKS" ]; then
  for DRIVE_ID in $P1_EXT_DISKS; do
    DISK_PATH=$(echo "$P1_METADATA" | jq -r ".gcs_disk_index_objects[\"$DRIVE_ID\"]")
    info "Downloading disk index for drive '$DRIVE_ID'..."
    DISK_IDX_JSON=$(gsutil cat "gs://$GCS_BUCKET/$DISK_PATH" 2>/dev/null || echo "DOWNLOAD_FAILED")
    if [ "$DISK_IDX_JSON" != "DOWNLOAD_FAILED" ]; then
      DISK_EXTENTS=$(echo "$DISK_IDX_JSON" | jq '.region.extents | length')
      pass "Pause 1 disk '$DRIVE_ID': $DISK_EXTENTS extents"
    fi
  done
fi

# =====================================================================
header "6. RESUME from pause 1"
# =====================================================================
RESUME1_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")

RUNNER_ID_2=$(echo "$RESUME1_RESP" | jq -r '.runner_id')
RESUMED1=$(echo "$RESUME1_RESP" | jq -r '.resumed // false')
dump_json "Resume 1 response" "$RESUME1_RESP"

if [ "$RESUMED1" = "true" ]; then
  pass "Resume 1 succeeded: $RUNNER_ID_2"
else
  fail "Resume 1 failed (expected resumed=true)"
  tail -30 /tmp/fc-dev/logs/firecracker-manager.log
  exit 1
fi

if ! wait_ready "$RUNNER_ID_2" "resume 1"; then
  fail "Resumed runner not ready"
  exit 1
fi
wait_exec_ready "$RUNNER_ID_2"
pass "Resumed runner exec-ready"

# =====================================================================
header "7. Verify pause 1 state AND create new state for pause 2"
# =====================================================================

# 7a. Verify pause 1 markers
info "Verifying pause 1 memory marker..."
OUT=$(vm_exec "$RUNNER_ID_2" "cat /tmp/p1-mem.txt")
if echo "$OUT" | grep -q 'PAUSE1-MEM-MARKER'; then
  pass "Pause 1 memory marker preserved"
else
  fail "Pause 1 memory marker LOST"
fi

info "Verifying pause 1 disk marker..."
OUT=$(vm_exec "$RUNNER_ID_2" "cat /var/tmp/p1-disk.txt")
if echo "$OUT" | grep -q 'PAUSE1-DISK-MARKER'; then
  pass "Pause 1 disk marker preserved"
else
  fail "Pause 1 disk marker LOST"
fi

info "Verifying 2MB file checksum..."
OUT=$(vm_exec "$RUNNER_ID_2" "md5sum /tmp/p1-bigfile.bin")
RESUMED_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1)
if [ "$P1_BIGFILE_MD5" = "$RESUMED_MD5" ] && [ -n "$RESUMED_MD5" ]; then
  pass "2MB file checksum matches (no corruption)"
else
  fail "2MB file checksum mismatch: before=$P1_BIGFILE_MD5, after=$RESUMED_MD5"
fi

# 7b. Create NEW state for pause 2
info "Writing NEW memory marker for pause 2..."
OUT=$(vm_exec "$RUNNER_ID_2" "echo PAUSE2-MEM-MARKER > /tmp/p2-mem.txt && cat /tmp/p2-mem.txt")
if echo "$OUT" | grep -q 'PAUSE2-MEM-MARKER'; then
  pass "Pause 2 memory marker written"
else
  fail "Pause 2 memory marker failed"
fi

info "Writing NEW disk marker for pause 2..."
OUT=$(vm_exec "$RUNNER_ID_2" "echo PAUSE2-DISK-MARKER > /var/tmp/p2-disk.txt && cat /var/tmp/p2-disk.txt")
if echo "$OUT" | grep -q 'PAUSE2-DISK-MARKER'; then
  pass "Pause 2 disk marker written"
else
  fail "Pause 2 disk marker failed"
fi

# =====================================================================
header "8. PAUSE 2 → GCS"
# =====================================================================
PAUSE2_RESP=$(curl -s -X POST "$CP/api/v1/runners/pause" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID_2\"}")

PAUSE2_SESSION=$(echo "$PAUSE2_RESP" | jq -r '.session_id // empty')
PAUSE2_LAYER=$(echo "$PAUSE2_RESP" | jq -r '.layer // empty')
PAUSE2_SIZE=$(echo "$PAUSE2_RESP" | jq -r '.snapshot_size_bytes // 0')

dump_json "Pause 2 response" "$PAUSE2_RESP"

if [ -n "$PAUSE2_SESSION" ] && [ "$PAUSE2_SESSION" != "null" ]; then
  pass "Pause 2 succeeded (layer=$PAUSE2_LAYER, size=$PAUSE2_SIZE)"
else
  fail "Pause 2 failed"
  exit 1
fi

if [ "$PAUSE2_LAYER" = "1" ]; then
  pass "Pause 2 is layer 1 (correct chaining)"
else
  fail "Expected layer 1, got $PAUSE2_LAYER"
fi

# =====================================================================
header "9. INSPECT: Pause 2 artifacts & DEDUP comparison"
# =====================================================================

# Re-read metadata
SESSION_DIR=$(find_session_dir "$SESSION_ID")
P2_METADATA=$(cat "$SESSION_DIR/metadata.json")
dump_json "metadata.json (after pause 2)" "$P2_METADATA" \
  '{session_id, workload_key, runner_id, layers, gcs_manifest_path, gcs_mem_index_object, gcs_disk_index_objects}'

P2_GCS_MANIFEST=$(echo "$P2_METADATA" | jq -r '.gcs_manifest_path // empty')
P2_GCS_MEM_IDX=$(echo "$P2_METADATA" | jq -r '.gcs_mem_index_object // empty')

if [ "$P1_GCS_MANIFEST" != "$P2_GCS_MANIFEST" ]; then
  pass "Pause 2 has different GCS manifest (new snapshot): $P2_GCS_MANIFEST"
else
  fail "Pause 2 manifest same as pause 1 — not updated?"
fi

# 9a. Download pause 2 mem chunk index
info "Downloading pause 2 mem chunk index..."
P2_MEM_IDX_JSON=$(gsutil cat "gs://$GCS_BUCKET/$P2_GCS_MEM_IDX" 2>/dev/null || echo "DOWNLOAD_FAILED")
if [ "$P2_MEM_IDX_JSON" = "DOWNLOAD_FAILED" ]; then
  fail "Could not download pause 2 mem chunk index"
else
  P2_MEM_EXTENTS=$(echo "$P2_MEM_IDX_JSON" | jq '.region.extents | length')
  P2_CHUNK_SIZE=$(echo "$P2_MEM_IDX_JSON" | jq '.chunk_size_bytes')
  P2_LOGICAL_SIZE=$(echo "$P2_MEM_IDX_JSON" | jq '.region.logical_size_bytes')
  pass "Pause 2 mem index: $P2_MEM_EXTENTS extents, chunk_size=$P2_CHUNK_SIZE, logical=$P2_LOGICAL_SIZE"

  P2_MEM_HASHES=$(echo "$P2_MEM_IDX_JSON" | jq -r '.region.extents[].hash' | sort)
  P2_MEM_UNIQUE_HASHES=$(echo "$P2_MEM_HASHES" | sort -u | wc -l | tr -d ' ')
  P2_MEM_TOTAL_HASHES=$(echo "$P2_MEM_HASHES" | wc -l | tr -d ' ')
  inspect "Pause 2 mem: $P2_MEM_TOTAL_HASHES total extents, $P2_MEM_UNIQUE_HASHES unique hashes"

  # 9b. DEDUP ANALYSIS: Compare chunk hash sets
  echo ""
  info "--- CHUNK DEDUP ANALYSIS ---"

  # Hashes in both pause 1 and pause 2 (reused chunks = dedup working)
  COMMON_HASHES=$(comm -12 <(echo "$P1_MEM_HASHES") <(echo "$P2_MEM_HASHES") | wc -l | tr -d ' ')
  # New hashes only in pause 2
  NEW_IN_P2=$(comm -13 <(echo "$P1_MEM_HASHES") <(echo "$P2_MEM_HASHES") | wc -l | tr -d ' ')
  # Hashes removed (in p1 but not p2 — should be rare, only if dirty chunk merged differently)
  REMOVED=$(comm -23 <(echo "$P1_MEM_HASHES") <(echo "$P2_MEM_HASHES") | wc -l | tr -d ' ')

  inspect "Chunks reused from pause 1:     $COMMON_HASHES"
  inspect "New chunks only in pause 2:      $NEW_IN_P2"
  inspect "Chunks replaced (dirty in p2):   $REMOVED"

  if [ "$COMMON_HASHES" -gt 0 ]; then
    # Calculate dedup ratio
    if [ "$P2_MEM_TOTAL_HASHES" -gt 0 ]; then
      DEDUP_PCT=$(( (COMMON_HASHES * 100) / P2_MEM_TOTAL_HASHES ))
      pass "Chunk dedup working: ${DEDUP_PCT}% of pause 2 chunks reused from pause 1 ($COMMON_HASHES/$P2_MEM_TOTAL_HASHES)"
    fi
  else
    fail "No chunk reuse between pause 1 and pause 2 — dedup broken?"
  fi

  # We expect NEW_IN_P2 to be much smaller than COMMON_HASHES
  # (only dirty pages should produce new chunks)
  if [ "$NEW_IN_P2" -lt "$COMMON_HASHES" ]; then
    pass "New chunks ($NEW_IN_P2) < reused ($COMMON_HASHES) — incremental pause is efficient"
  else
    inspect "Warning: more new chunks than reused — VM dirtied a lot of memory between pauses"
  fi
fi

# 9c. Extension disk dedup comparison
P2_EXT_DISKS=$(echo "$P2_METADATA" | jq -r '.gcs_disk_index_objects // {} | keys[]' 2>/dev/null || true)
if [ -n "$P2_EXT_DISKS" ]; then
  echo ""
  info "--- EXTENSION DISK DEDUP ---"
  for DRIVE_ID in $P2_EXT_DISKS; do
    DISK_PATH=$(echo "$P2_METADATA" | jq -r ".gcs_disk_index_objects[\"$DRIVE_ID\"]")
    P2_DISK_JSON=$(gsutil cat "gs://$GCS_BUCKET/$DISK_PATH" 2>/dev/null || echo "DOWNLOAD_FAILED")
    if [ "$P2_DISK_JSON" != "DOWNLOAD_FAILED" ]; then
      P2_DISK_EXTENTS=$(echo "$P2_DISK_JSON" | jq '.region.extents | length')
      inspect "Pause 2 disk '$DRIVE_ID': $P2_DISK_EXTENTS extents"
    fi
  done
fi

# =====================================================================
header "10. RESUME from pause 2 → final state verification"
# =====================================================================
RESUME2_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")

RUNNER_ID_3=$(echo "$RESUME2_RESP" | jq -r '.runner_id')
RESUMED2=$(echo "$RESUME2_RESP" | jq -r '.resumed // false')

if [ "$RESUMED2" = "true" ]; then
  pass "Resume 2 succeeded: $RUNNER_ID_3"
else
  fail "Resume 2 failed"
  dump_json "Response" "$RESUME2_RESP"
  exit 1
fi

if ! wait_ready "$RUNNER_ID_3" "resume 2"; then
  fail "Resume 2 runner not ready"
  exit 1
fi
wait_exec_ready "$RUNNER_ID_3"
pass "Resume 2 runner exec-ready"

# =====================================================================
header "11. Verify ALL state from BOTH pauses"
# =====================================================================

echo -e "  ${CYAN}--- State from pause 1 ---${NC}"

info "Checking pause 1 memory marker..."
OUT=$(vm_exec "$RUNNER_ID_3" "cat /tmp/p1-mem.txt")
if echo "$OUT" | grep -q 'PAUSE1-MEM-MARKER'; then
  pass "Pause 1 memory marker preserved across 2 pauses"
else
  fail "Pause 1 memory marker LOST after 2 pauses"
fi

info "Checking pause 1 disk marker..."
OUT=$(vm_exec "$RUNNER_ID_3" "cat /var/tmp/p1-disk.txt")
if echo "$OUT" | grep -q 'PAUSE1-DISK-MARKER'; then
  pass "Pause 1 disk marker preserved across 2 pauses"
else
  fail "Pause 1 disk marker LOST after 2 pauses"
fi

info "Checking 2MB file integrity..."
OUT=$(vm_exec "$RUNNER_ID_3" "md5sum /tmp/p1-bigfile.bin")
FINAL_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1)
if [ "$P1_BIGFILE_MD5" = "$FINAL_MD5" ] && [ -n "$FINAL_MD5" ]; then
  pass "2MB file integrity preserved across 2 pauses (md5=$FINAL_MD5)"
else
  fail "2MB file corrupted after 2 pauses: original=$P1_BIGFILE_MD5, final=$FINAL_MD5"
fi

echo -e "  ${CYAN}--- State from pause 2 ---${NC}"

info "Checking pause 2 memory marker..."
OUT=$(vm_exec "$RUNNER_ID_3" "cat /tmp/p2-mem.txt")
if echo "$OUT" | grep -q 'PAUSE2-MEM-MARKER'; then
  pass "Pause 2 memory marker preserved"
else
  fail "Pause 2 memory marker LOST"
fi

info "Checking pause 2 disk marker..."
OUT=$(vm_exec "$RUNNER_ID_3" "cat /var/tmp/p2-disk.txt")
if echo "$OUT" | grep -q 'PAUSE2-DISK-MARKER'; then
  pass "Pause 2 disk marker preserved"
else
  fail "Pause 2 disk marker LOST"
fi

# =====================================================================
header "12. Cleanup"
# =====================================================================
RELEASE_RESP=$(curl -s -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID_3\"}")

if echo "$RELEASE_RESP" | jq -e '.success' > /dev/null 2>&1; then
  pass "Runner released"
else
  fail "Release failed: $RELEASE_RESP"
fi

# =====================================================================
header "RESULTS"
# =====================================================================
echo ""
echo -e "  Passed: ${GREEN}$PASS${NC}"
echo -e "  Failed: ${RED}$FAIL${NC}"
echo ""

if [ "$FAIL" -gt 0 ]; then
  echo -e "${RED}=========================================${NC}"
  echo -e "${RED}=== SOME TESTS FAILED ===${NC}"
  echo -e "${RED}=========================================${NC}"
  exit 1
else
  echo -e "${GREEN}=========================================${NC}"
  echo -e "${GREEN}=== ALL TESTS PASSED ===${NC}"
  echo -e "${GREEN}=========================================${NC}"
fi
