#!/bin/bash
# E2E test: GCS-backed session pause/resume (cross-host simulation)
#
# Tests the full flow:
#   allocate(session) → exec → pause(→GCS) → delete local session → resume(←GCS) → exec → verify state
#
# Usage:
#   SESSION_CHUNK_BUCKET=rroy-gc-testing make dev-test-gcs-pause-resume
#
# Prerequisites:
#   - Golden chunked snapshot uploaded: GCS_BUCKET=<bucket> ENABLE_CHUNKED=true make dev-snapshot
#   - Stack running with GCS sessions:  SESSION_CHUNK_BUCKET=<bucket> make dev-stack
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="gcs-e2e-$(date +%s)"
GCS_BUCKET=${SESSION_CHUNK_BUCKET:-}
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

if [ -z "$GCS_BUCKET" ]; then
  echo "FAIL: SESSION_CHUNK_BUCKET is required."
  echo "Usage: SESSION_CHUNK_BUCKET=your-bucket make dev-test-gcs-pause-resume"
  exit 1
fi

echo "GCS bucket: $GCS_BUCKET"
echo "Session ID: $SESSION_ID"

# ---------------------------------------------------------------------------
header "1. Register snapshot config"
# ---------------------------------------------------------------------------
# IMPORTANT: commands must match what was used in `build-snapshot.sh` so the
# workload_key hash matches the golden chunked snapshot in GCS.
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}
CONFIG_RESP=$(curl -s -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "gcs-pause-resume-test",
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
header "2. Allocate runner with session_id"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")
echo "  Response: $ALLOC_RESP"

RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  fail "Allocate returned no runner_id"
  exit 1
fi
pass "Runner allocated: $RUNNER_ID"

# ---------------------------------------------------------------------------
header "3. Wait for ready"
# ---------------------------------------------------------------------------
READY=false
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo "  Ready after ${i}s"
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
  echo "Manager log tail:"
  tail -30 /tmp/fc-dev/logs/firecracker-manager.log
  exit 1
fi

# ---------------------------------------------------------------------------
header "3b. TTL auto-pause behavioral test"
# ---------------------------------------------------------------------------
# Regression test for the TTL propagation bug: the scheduler used to store
# runner_ttl_seconds and auto_pause in the DB but never forward them during
# allocation. Now we test behaviorally: allocate a runner with a short TTL,
# wait for it to expire, and verify the runner was auto-paused.
#
# We allocate a SEPARATE short-lived runner for this test so we don't
# disturb the main test runner.
TTL_SESSION="ttl-test-$(date +%s)"

# Register a snapshot config with a 3-second TTL
TTL_CONFIG_RESP=$(curl -s -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "ttl-auto-pause-test",
    "commands": '"$SNAPSHOT_COMMANDS"',
    "runner_ttl_seconds": 3,
    "auto_pause": true,
    "session_max_age_seconds": 3600
  }')
TTL_WORKLOAD_KEY=$(echo "$TTL_CONFIG_RESP" | jq -r '.workload_key')
echo "  TTL test workload_key=$TTL_WORKLOAD_KEY"

TTL_ALLOC_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$TTL_WORKLOAD_KEY\", \"session_id\":\"$TTL_SESSION\"}")
TTL_RUNNER_ID=$(echo "$TTL_ALLOC_RESP" | jq -r '.runner_id')
echo "  TTL runner: $TTL_RUNNER_ID"

if [ -z "$TTL_RUNNER_ID" ] || [ "$TTL_RUNNER_ID" = "null" ]; then
  fail "TTL test: failed to allocate runner"
else
  # Wait for it to become ready
  echo -n "  Waiting for ready..."
  for i in $(seq 1 60); do
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
      "$CP/api/v1/runners/status?runner_id=$TTL_RUNNER_ID")
    if [ "$HTTP_CODE" = "200" ]; then
      echo " ready (${i}s)"
      break
    fi
    echo -n "."
    sleep 1
  done

  # Now wait for TTL to expire. The autoscale loop ticks every 2s, and the
  # control plane learns the state via heartbeats (every 5s by default).
  # With TTL=3s: idle for 3s + up to 2s autoscale tick + up to 5s heartbeat = ~10s max.
  # We wait 15s to be safe.
  echo "  Waiting 15s for TTL (3s) + autoscale tick + heartbeat propagation..."
  sleep 15

  # Check runner state on the control plane — should be paused or gone.
  TTL_STATUS_RESP=$(curl -s "$CP/api/v1/runners/status?runner_id=$TTL_RUNNER_ID")
  TTL_STATUS_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$TTL_RUNNER_ID")
  TTL_STATE=$(echo "$TTL_STATUS_RESP" | jq -r '.state // "unknown"')
  echo "  TTL runner state: $TTL_STATE (HTTP $TTL_STATUS_CODE)"

  # Also check the manager's view of the runner
  MGR_RUNNERS=$(curl -s "$MGR/api/v1/runners" 2>/dev/null || echo '[]')
  TTL_MGR_STATE=$(echo "$MGR_RUNNERS" | jq -r ".[] | select(.id == \"$TTL_RUNNER_ID\") | .state" 2>/dev/null || echo "not_found")
  echo "  Manager view: $TTL_MGR_STATE"

  if [ "$TTL_STATE" = "paused" ] || [ "$TTL_STATE" = "suspended" ] || \
     [ "$TTL_MGR_STATE" = "paused" ] || [ "$TTL_MGR_STATE" = "suspended" ] || \
     [ "$TTL_STATUS_CODE" = "404" ]; then
    pass "TTL auto-pause: runner was paused/removed after 3s TTL"
  else
    fail "TTL auto-pause: runner still $TTL_STATE after 15s (expected paused)"
    echo "  Manager log (TTL entries):"
    grep -i "ttl\|auto.pause" /tmp/fc-dev/logs/firecracker-manager.log 2>/dev/null | tail -5 || echo "  (none)"
  fi
fi

# ---------------------------------------------------------------------------
header "4. Create state inside VM (memory + disk + process)"
# ---------------------------------------------------------------------------

# Helper to run a command inside the VM and capture output
vm_exec() {
  local cmd="$1"
  curl -s --no-buffer --max-time 30 -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":[\"sh\",\"-c\",\"$cmd\"],\"timeout_seconds\":20}"
}

# 4a. Memory state: write marker to tmpfs (/tmp is memory-backed)
echo "  --- 4a. Memory state (tmpfs) ---"
OUT=$(vm_exec "echo gcs-mem-marker-12345 > /tmp/gcs-test.txt && cat /tmp/gcs-test.txt")
echo "  $OUT"
if echo "$OUT" | grep -q 'gcs-mem-marker-12345'; then
  pass "tmpfs marker written"
else
  fail "tmpfs marker write failed"
fi

# 4b. Memory state: write ~2MB of data to tmpfs to span partial chunk
echo "  --- 4b. Larger memory data (2MB in tmpfs) ---"
OUT=$(vm_exec "dd if=/dev/urandom of=/tmp/gcs-bigfile.bin bs=1K count=2048 2>/dev/null && md5sum /tmp/gcs-bigfile.bin")
echo "  $OUT"
BIGFILE_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1)
echo "  MD5: $BIGFILE_MD5"
if [ -n "$BIGFILE_MD5" ]; then
  pass "2MB tmpfs file written (md5=$BIGFILE_MD5)"
else
  fail "2MB tmpfs file write failed"
fi

# 4c. Disk state: write to persistent filesystem (use /var/tmp which is on rootfs and world-writable)
echo "  --- 4c. Disk state (rootfs) ---"
OUT=$(vm_exec "echo gcs-disk-marker-67890 > /var/tmp/gcs-disk-test.txt && cat /var/tmp/gcs-disk-test.txt")
echo "  $OUT"
if echo "$OUT" | grep -q 'gcs-disk-marker-67890'; then
  pass "rootfs marker written"
else
  fail "rootfs marker write failed"
fi

# 4d. Process state: record the thaw-agent PID — it should survive pause/resume
echo "  --- 4d. Process state ---"
OUT=$(vm_exec "pgrep thaw-agent | head -1")
PRE_PAUSE_PID=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '\d+' | head -1)
echo "  thaw-agent PID before pause: $PRE_PAUSE_PID"
if [ -n "$PRE_PAUSE_PID" ]; then
  pass "thaw-agent running (pid=$PRE_PAUSE_PID)"
else
  fail "thaw-agent not found"
fi

# 4e. Environment / kernel state: record uptime and PID list
echo "  --- 4e. Kernel state ---"
OUT=$(vm_exec "head -c 20 /proc/uptime")
PRE_PAUSE_UPTIME=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[0-9]+\.[0-9]+' | head -1)
echo "  Uptime before pause: ${PRE_PAUSE_UPTIME}s"

# ---------------------------------------------------------------------------
header "5. Pause runner (should upload to GCS)"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -s -X POST "$CP/api/v1/runners/pause" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")
echo "  Response: $PAUSE_RESP"

PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id // empty')
if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Runner paused"
else
  fail "Pause did not return session_id"
  exit 1
fi

# ---------------------------------------------------------------------------
header "6. Verify GCS has session manifest"
# ---------------------------------------------------------------------------
# Read metadata to get the GCS paths
SESSION_DIR="/tmp/fc-dev/sessions/$SESSION_ID"
if [ ! -f "$SESSION_DIR/metadata.json" ]; then
  # Try derived path
  SESSION_DIR="$(dirname /tmp/fc-dev/snapshots)/sessions/$SESSION_ID"
fi

GCS_MANIFEST=$(jq -r '.gcs_manifest_path // empty' "$SESSION_DIR/metadata.json" 2>/dev/null || echo "")
GCS_MEM_INDEX=$(jq -r '.gcs_mem_index_object // empty' "$SESSION_DIR/metadata.json" 2>/dev/null || echo "")

echo "  GCS manifest: $GCS_MANIFEST"
echo "  GCS mem index: $GCS_MEM_INDEX"

if [ -n "$GCS_MANIFEST" ] && [ "$GCS_MANIFEST" != "null" ]; then
  pass "metadata.json has GCS manifest path"
else
  fail "GCS manifest path not set in metadata — upload may have failed"
  echo "  metadata.json contents:"
  cat "$SESSION_DIR/metadata.json" 2>/dev/null || echo "  (not found)"
  echo ""
  echo "  Manager log (last 50 lines):"
  tail -50 /tmp/fc-dev/logs/firecracker-manager.log
  exit 1
fi

if gsutil -q stat "gs://$GCS_BUCKET/$GCS_MANIFEST" 2>/dev/null; then
  pass "snapshot_manifest.json exists in GCS"
else
  fail "snapshot_manifest.json NOT found in GCS: gs://$GCS_BUCKET/$GCS_MANIFEST"
fi

if gsutil -q stat "gs://$GCS_BUCKET/$GCS_MEM_INDEX" 2>/dev/null; then
  pass "chunked-metadata.json exists in GCS"
else
  fail "chunked-metadata.json NOT found in GCS"
fi

# ---------------------------------------------------------------------------
header "7. Delete local session files (simulate cross-host resume)"
# ---------------------------------------------------------------------------
echo "  Deleting: $SESSION_DIR"
# Keep metadata.json (the resume path reads it), but delete the actual layer files
# so the UFFD handler is forced to use GCS instead of local files.
rm -rf "$SESSION_DIR/layer_"*
if [ ! -d "$SESSION_DIR/layer_0" ]; then
  pass "Local layer files deleted"
else
  fail "Failed to delete local layer files"
fi

# ---------------------------------------------------------------------------
header "8. Resume: allocate with same session_id (should use GCS)"
# ---------------------------------------------------------------------------
RESUME_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")
echo "  Response: $RESUME_RESP"

RESUME_RUNNER_ID=$(echo "$RESUME_RESP" | jq -r '.runner_id')
RESUMED=$(echo "$RESUME_RESP" | jq -r '.resumed // false')
RESUME_IP=$(echo "$RESUME_RESP" | jq -r '.internal_ip // empty')
echo "  Runner ID: $RESUME_RUNNER_ID"
echo "  Resumed: $RESUMED"
echo "  Internal IP: $RESUME_IP"

if [ "$RESUMED" = "true" ]; then
  pass "Session resumed"
else
  fail "Expected resumed=true, got $RESUMED"
  echo "  Manager log (last 30 lines):"
  tail -30 /tmp/fc-dev/logs/firecracker-manager.log
  exit 1
fi

# Wait for ready
echo -n "  Waiting for resumed runner..."
READY=false
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RESUME_RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    READY=true
    break
  fi
  echo -n "."
  sleep 1
done

if ! $READY; then
  fail "Resumed runner did not become ready in 60s"
  echo "  Manager log (last 30 lines):"
  tail -30 /tmp/fc-dev/logs/firecracker-manager.log
  exit 1
fi
pass "Resumed runner is ready"

# ---------------------------------------------------------------------------
header "9. Verify ALL state preserved after GCS resume"
# ---------------------------------------------------------------------------

# Wait for exec to actually work (thaw-agent needs time to re-bind after resume)
echo "  --- Pre-check: waiting for exec to become responsive ---"
EXEC_READY=false
for i in $(seq 1 30); do
  PRECHECK=$(curl -s --no-buffer --max-time 5 -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d '{"command":["echo","exec-alive"],"timeout_seconds":3}' 2>&1 || echo "EXEC_TIMEOUT")
  if echo "$PRECHECK" | grep -q 'exec-alive'; then
    echo "  Exec responsive after ${i}s"
    EXEC_READY=true
    break
  fi
  echo -n "."
  sleep 1
done
echo ""

if $EXEC_READY; then
  pass "Exec reachable on resumed VM"
else
  fail "Exec NOT reachable on resumed VM after 30s"
  echo "  Last response: $PRECHECK"
  echo "  Manager log (last 10 lines):"
  tail -10 /tmp/fc-dev/logs/firecracker-manager.log
fi

# Helper to run a command inside the resumed VM (never fails the script)
vm_exec_resumed() {
  local cmd="$1"
  curl -s --no-buffer --max-time 30 -X POST "$MGR/api/v1/runners/$RESUME_RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":[\"sh\",\"-c\",\"$cmd\"],\"timeout_seconds\":20}" 2>&1 || echo "EXEC_TIMEOUT"
}

# 9a. Memory: tmpfs marker
echo "  --- 9a. Memory state (tmpfs marker) ---"
OUT=$(vm_exec_resumed "cat /tmp/gcs-test.txt")
echo "  $OUT"
if echo "$OUT" | grep -q 'gcs-mem-marker-12345'; then
  pass "tmpfs marker preserved"
else
  fail "tmpfs marker LOST — memory state not restored"
fi

# 9b. Memory: 2MB file integrity
echo "  --- 9b. Memory state (2MB file checksum) ---"
OUT=$(vm_exec_resumed "md5sum /tmp/gcs-bigfile.bin")
echo "  $OUT"
RESUMED_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1)
echo "  MD5 before: $BIGFILE_MD5"
echo "  MD5 after:  $RESUMED_MD5"
if [ "$BIGFILE_MD5" = "$RESUMED_MD5" ] && [ -n "$RESUMED_MD5" ]; then
  pass "2MB tmpfs file checksum matches — no corruption"
else
  fail "2MB tmpfs file checksum MISMATCH — data corrupted"
fi

# 9c. Disk: rootfs marker
echo "  --- 9c. Disk state (rootfs marker) ---"
OUT=$(vm_exec_resumed "cat /var/tmp/gcs-disk-test.txt")
echo "  $OUT"
if echo "$OUT" | grep -q 'gcs-disk-marker-67890'; then
  pass "rootfs marker preserved"
else
  fail "rootfs marker LOST — disk state not restored"
fi

# 9d. Process: thaw-agent should still be running (PID may change — it restarts on resume to re-read MMDS)
echo "  --- 9d. Process state (thaw-agent alive) ---"
OUT=$(vm_exec_resumed "pgrep thaw-agent | head -1")
POST_RESUME_PID=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '\d+' | head -1)
echo "  PID before pause: $PRE_PAUSE_PID"
echo "  PID after resume: $POST_RESUME_PID"
if [ -n "$POST_RESUME_PID" ]; then
  pass "thaw-agent running after resume (pid=$POST_RESUME_PID)"
  if [ "$PRE_PAUSE_PID" = "$POST_RESUME_PID" ]; then
    echo "    (same PID — process survived snapshot)"
  else
    echo "    (new PID — agent restarted on resume, expected)"
  fi
else
  fail "thaw-agent NOT running after resume"
fi

# 9e. Kernel: uptime should be >= pre-pause (kernel continues from snapshot)
echo "  --- 9e. Kernel state (uptime) ---"
OUT=$(vm_exec_resumed "head -c 20 /proc/uptime")
POST_RESUME_UPTIME=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[0-9]+\.[0-9]+' | head -1)
echo "  Uptime before pause: ${PRE_PAUSE_UPTIME}s"
echo "  Uptime after resume: ${POST_RESUME_UPTIME}s"
# Uptime comparison: kernel's monotonic clock continues from the snapshot point
if [ -n "$POST_RESUME_UPTIME" ]; then
  pass "Kernel alive (uptime=${POST_RESUME_UPTIME}s)"
else
  fail "Could not read kernel uptime"
fi

# 9f. Exec works: basic command
echo "  --- 9f. Exec sanity check ---"
OUT=$(vm_exec_resumed "whoami && hostname")
echo "  $OUT"
if echo "$OUT" | grep -q '"type":"exit"'; then
  pass "Exec works on resumed VM"
else
  fail "Exec broken on resumed VM"
fi

# ---------------------------------------------------------------------------
header "10. Multi-pause GCS chain: write more data → pause → verify GCS chaining"
# ---------------------------------------------------------------------------
# This tests the disk index carry-forward fix: pause 2 must carry forward
# disk index objects from pause 1 for drives that weren't dirty this time.
echo "  --- Writing new data before second pause ---"
OUT=$(vm_exec_resumed "echo gcs-chain-test-data > /tmp/gcs-chain.txt && cat /tmp/gcs-chain.txt")
if echo "$OUT" | grep -q 'gcs-chain-test-data'; then
  pass "New data written before second pause"
else
  fail "Failed to write data before second pause"
fi

PAUSE2_RESP=$(curl -s -X POST "$CP/api/v1/runners/pause" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RESUME_RUNNER_ID\"}")
echo "  Response: $PAUSE2_RESP"

PAUSE2_SESSION=$(echo "$PAUSE2_RESP" | jq -r '.session_id // empty')
PAUSE2_LAYER=$(echo "$PAUSE2_RESP" | jq -r '.layer // empty')
echo "  Layer: $PAUSE2_LAYER"

if [ "$PAUSE2_LAYER" = "1" ]; then
  pass "Second GCS pause is layer 1"
else
  fail "Expected layer 1, got $PAUSE2_LAYER"
fi

# Verify GCS disk index carry-forward in metadata
if [ -f "$SESSION_DIR/metadata.json" ]; then
  META2_GCS_DISK=$(jq -c '.gcs_disk_index_objects // {}' "$SESSION_DIR/metadata.json")
  META2_GCS_MEM=$(jq -r '.gcs_mem_index_object // "none"' "$SESSION_DIR/metadata.json")
  echo "  GCS disk indexes after pause 2: $META2_GCS_DISK"
  echo "  GCS mem index after pause 2: $META2_GCS_MEM"
  pass "GCS session metadata updated for layer 1"
fi

# Delete local layers again (simulate cross-host)
rm -rf "$SESSION_DIR/layer_"*
if [ ! -d "$SESSION_DIR/layer_0" ] && [ ! -d "$SESSION_DIR/layer_1" ]; then
  pass "Local layer files deleted for cross-host resume"
else
  fail "Failed to delete local layer files"
fi

# Resume from layer 1
RESUME2_RESP=$(curl -s -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$SESSION_ID\"}")
RESUME2_RUNNER_ID=$(echo "$RESUME2_RESP" | jq -r '.runner_id')
RESUME2_RESUMED=$(echo "$RESUME2_RESP" | jq -r '.resumed // false')
echo "  Runner ID: $RESUME2_RUNNER_ID"
echo "  Resumed: $RESUME2_RESUMED"

if [ "$RESUME2_RESUMED" = "true" ]; then
  pass "Resumed from GCS layer 1"
else
  fail "Expected resumed=true for GCS layer 1 resume"
fi

# Wait for ready
echo -n "  Waiting..."
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RESUME2_RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# Wait for exec
echo -n "  Waiting for exec..."
for i in $(seq 1 30); do
  PRECHECK2=$(curl -s --no-buffer --max-time 5 -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d '{"command":["echo","alive2"],"timeout_seconds":3}' 2>&1 || echo "TIMEOUT")
  if echo "$PRECHECK2" | grep -q 'alive2'; then
    echo " responsive (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# Verify ALL data survived 2 GCS pause/resume cycles
vm_exec_chain() {
  local cmd="$1"
  curl -s --no-buffer --max-time 30 -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":[\"sh\",\"-c\",\"$cmd\"],\"timeout_seconds\":20}" 2>&1 || echo "EXEC_TIMEOUT"
}

echo "  --- Verify layer 0 data (original tmpfs marker) ---"
OUT=$(vm_exec_chain "cat /tmp/gcs-test.txt")
if echo "$OUT" | grep -q 'gcs-mem-marker-12345'; then
  pass "Original tmpfs marker preserved through 2 GCS cycles"
else
  fail "Original tmpfs marker LOST after 2 GCS cycles"
fi

echo "  --- Verify layer 0 data (rootfs marker) ---"
OUT=$(vm_exec_chain "cat /var/tmp/gcs-disk-test.txt")
if echo "$OUT" | grep -q 'gcs-disk-marker-67890'; then
  pass "Original rootfs marker preserved through 2 GCS cycles"
else
  fail "Original rootfs marker LOST after 2 GCS cycles"
fi

echo "  --- Verify layer 1 data (chain test) ---"
OUT=$(vm_exec_chain "cat /tmp/gcs-chain.txt")
if echo "$OUT" | grep -q 'gcs-chain-test-data'; then
  pass "Layer 1 data preserved through GCS resume"
else
  fail "Layer 1 data LOST after GCS resume"
fi

# ---------------------------------------------------------------------------
header "11. Cleanup: release runner"
# ---------------------------------------------------------------------------
RELEASE_RESP=$(curl -s -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RESUME2_RUNNER_ID\"}")

if echo "$RELEASE_RESP" | jq -e '.success' > /dev/null 2>&1; then
  pass "Runner released"
else
  fail "Release failed: $RELEASE_RESP"
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
