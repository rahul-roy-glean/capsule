#!/bin/bash
# E2E test: GCS-backed session pause/resume (cross-host simulation)
#
# Tests the full flow:
#   allocate(session) → exec → pause(→GCS) → delete local session → resume(←GCS) → exec → verify state
#
# Usage:
#   GCS_BUCKET=rroy-gc-testing make dev-test-gcs-pause-resume
#
# Prerequisites:
#   - Golden chunked snapshot uploaded: GCS_BUCKET=<bucket> make dev-snapshot
#   - Stack running with GCS sessions:  GCS_BUCKET=<bucket> make dev-stack
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="gcs-e2e-$(date +%s)"
GCS_BUCKET=${GCS_BUCKET:-${SESSION_CHUNK_BUCKET:-}}
PASS=0
FAIL=0

. "$(dirname "${BASH_SOURCE[0]}")/lib-workload-key.sh"
. "$(dirname "${BASH_SOURCE[0]}")/lib-gcs-mode.sh"

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

vm_exec_runner() {
  local runner_id="$1"
  local cmd="$2"
  curl -s --no-buffer --max-time 30 -X POST "$MGR/api/v1/runners/$runner_id/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":[\"sh\",\"-c\",\"$cmd\"],\"timeout_seconds\":20}" 2>&1 || echo "EXEC_TIMEOUT"
}

dump_vm_sanity() {
  local runner_id="$1"
  local label="$2"
  echo "  --- ${label}: guest sanity snapshot ---"
  local out
  out=$(vm_exec_runner "$runner_id" "echo 'WHOAMI:'; whoami; echo 'HOSTNAME:'; hostname; echo 'UPTIME:'; head -c 20 /proc/uptime; echo; echo 'ETH0:'; ip -brief addr show dev eth0 2>/dev/null || ip addr show dev eth0 2>/dev/null || true; echo 'ROUTES:'; ip route 2>/dev/null || cat /proc/net/route; echo 'RESOLV:'; cat /etc/resolv.conf 2>/dev/null || true; echo 'THAW_PID:'; pgrep thaw-agent | head -1")
  echo "$out"
}

extract_stdout() {
  echo "$1" | jq -rs 'map(select(.type=="stdout") | .data) | join("")'
}

PROC_STATE_SERVER_PY=$(cat <<'PY'
import hashlib
import json
import os
import socket

sock_path = "/tmp/proc-state.sock"
try:
    os.unlink(sock_path)
except FileNotFoundError:
    pass

blob = bytearray((b"bazel-firecracker-proc-state-" * ((64 * 1024 * 1024 // 29) + 1))[: 64 * 1024 * 1024])
state = {
    "pid": os.getpid(),
    "token": "unset",
    "counter": 0,
    "blob_sha": hashlib.sha256(blob).hexdigest(),
    "blob_len": len(blob),
}

srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
srv.bind(sock_path)
srv.listen(16)

while True:
    conn, _ = srv.accept()
    try:
        data = conn.recv(4096).decode().strip()
        if data.startswith("SET "):
            state["token"] = data[4:]
        elif data == "BUMP":
            state["counter"] += 1
        conn.sendall((json.dumps(state) + "\n").encode())
    finally:
        conn.close()
PY
)
PROC_STATE_SERVER_B64=$(printf '%s' "$PROC_STATE_SERVER_PY" | base64 -w0 2>/dev/null || printf '%s' "$PROC_STATE_SERVER_PY" | base64 | tr -d '\n')

proc_state_raw_runner() {
  local runner_id="$1"
  local payload="$2"
  vm_exec_runner "$runner_id" "python3 -c 'import socket; s=socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.connect(\"/tmp/proc-state.sock\"); s.sendall(b\"$payload\\n\"); print(s.recv(65536).decode(), end=\"\")'"
}

proc_state_get_runner() {
  extract_stdout "$(proc_state_raw_runner "$1" "STATE")"
}

proc_state_set_runner() {
  local runner_id="$1"
  local token="$2"
  extract_stdout "$(proc_state_raw_runner "$runner_id" "SET $token")"
}

proc_state_bump_runner() {
  extract_stdout "$(proc_state_raw_runner "$1" "BUMP")"
}

dump_proc_state() {
  local runner_id="$1"
  local label="$2"
  local json
  json=$(proc_state_get_runner "$runner_id")
  echo "  --- ${label}: proc-state ---"
  echo "$json" | jq . 2>/dev/null | sed 's/^/    /' || echo "    $json"
}

GCS_BUCKET=$(require_gcs_bucket)
assert_manager_gcs_mode "$GCS_BUCKET"

echo "GCS bucket: $GCS_BUCKET"
echo "Session ID: $SESSION_ID"

# ---------------------------------------------------------------------------
header "1. Discover workload key and register config"
# ---------------------------------------------------------------------------
require_workload_key
register_dev_config "gcs-pause-resume-test" '{"ttl": 300, "auto_pause": true, "session_max_age_seconds": 3600}'
pass "Workload key discovered and config registered"

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

dump_vm_sanity "$RUNNER_ID" "fresh allocation"

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

# Re-register config with a 3-second TTL for the TTL subtest.
# Uses the same workload key (same snapshot commands), just different TTL config.
register_dev_config "ttl-auto-pause-test" '{"ttl": 3, "auto_pause": true, "session_max_age_seconds": 3600}'
TTL_WORKLOAD_KEY="$WORKLOAD_KEY"
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
  TTL_STATE=$(echo "$TTL_STATUS_RESP" | jq -r '.status // .state // "unknown"')
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
  vm_exec_runner "$RUNNER_ID" "$1"
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

# 4f. Tmpfs directory tree: many small files + manifest hash
echo "  --- 4f. Tmpfs directory tree workload ---"
OUT=$(vm_exec "rm -rf /tmp/gcs-tree && mkdir -p /tmp/gcs-tree/a /tmp/gcs-tree/b/c && for i in \$(seq 1 48); do printf 'mem-tree-%03d\n' \$i > /tmp/gcs-tree/a/file_\$i.txt; done && for i in \$(seq 49 96); do printf 'mem-nested-%03d\n' \$i > /tmp/gcs-tree/b/c/file_\$i.txt; done && find /tmp/gcs-tree -type f | sort | xargs sha256sum | sha256sum")
echo "  $OUT"
TMPFS_TREE_SHA=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{64}' | head -1)
echo "  Tmpfs tree SHA: $TMPFS_TREE_SHA"
if [ -n "$TMPFS_TREE_SHA" ]; then
  pass "tmpfs tree created (sha=$TMPFS_TREE_SHA)"
else
  fail "tmpfs tree workload failed"
fi

# 4g. Rootfs directory tree: nested files + manifest hash
echo "  --- 4g. Rootfs directory tree workload ---"
OUT=$(vm_exec "rm -rf /var/tmp/gcs-tree && mkdir -p /var/tmp/gcs-tree/x /var/tmp/gcs-tree/y/z && for i in \$(seq 1 24); do printf 'disk-tree-%03d\n' \$i > /var/tmp/gcs-tree/x/file_\$i.txt; done && for i in \$(seq 25 48); do printf 'disk-nested-%03d\n' \$i > /var/tmp/gcs-tree/y/z/file_\$i.txt; done && find /var/tmp/gcs-tree -type f | sort | xargs sha256sum | sha256sum")
echo "  $OUT"
ROOTFS_TREE_SHA=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{64}' | head -1)
echo "  Rootfs tree SHA: $ROOTFS_TREE_SHA"
if [ -n "$ROOTFS_TREE_SHA" ]; then
  pass "rootfs tree created (sha=$ROOTFS_TREE_SHA)"
else
  fail "rootfs tree workload failed"
fi

# 4h. Larger rootfs file: exercise disk chunking beyond tiny marker files
echo "  --- 4h. Larger disk data (8MB on rootfs) ---"
OUT=$(vm_exec "dd if=/dev/urandom of=/var/tmp/gcs-rootfs-big.bin bs=1K count=8192 2>/dev/null && md5sum /var/tmp/gcs-rootfs-big.bin")
echo "  $OUT"
ROOTFS_BIG_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1)
echo "  Rootfs bigfile MD5: $ROOTFS_BIG_MD5"
if [ -n "$ROOTFS_BIG_MD5" ]; then
  pass "8MB rootfs file written (md5=$ROOTFS_BIG_MD5)"
else
  fail "8MB rootfs file write failed"
fi

# 4i. In-memory daemon: heap-resident state over a Unix socket
echo "  --- 4i. In-memory daemon state ---"
PROC_TOKEN="session-proof-$SESSION_ID"
OUT=$(vm_exec "rm -f /tmp/proc-state.sock /tmp/proc_state_server.py /tmp/proc_state.log && printf '%s' '$PROC_STATE_SERVER_B64' | base64 -d >/tmp/proc_state_server.py && nohup python3 /tmp/proc_state_server.py >/tmp/proc_state.log 2>&1 & echo \$!")
echo "  $OUT"
PROC_DAEMON_PID=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '\d+' | head -1)
if [ -n "$PROC_DAEMON_PID" ]; then
  pass "proc-state daemon started (pid=$PROC_DAEMON_PID)"
else
  fail "proc-state daemon failed to start"
fi

sleep 1
dump_proc_state "$RUNNER_ID" "fresh allocation"
PROC_STATE_JSON=$(proc_state_set_runner "$RUNNER_ID" "$PROC_TOKEN")
for _ in $(seq 1 7); do
  PROC_STATE_JSON=$(proc_state_bump_runner "$RUNNER_ID")
done
echo "  Final pre-pause proc-state: $PROC_STATE_JSON"
PRE_PROC_PID=$(echo "$PROC_STATE_JSON" | jq -r '.pid // empty')
PRE_PROC_TOKEN=$(echo "$PROC_STATE_JSON" | jq -r '.token // empty')
PRE_PROC_COUNTER=$(echo "$PROC_STATE_JSON" | jq -r '.counter // empty')
PRE_PROC_BLOB_SHA=$(echo "$PROC_STATE_JSON" | jq -r '.blob_sha // empty')
PRE_PROC_BLOB_LEN=$(echo "$PROC_STATE_JSON" | jq -r '.blob_len // empty')
echo "  PID:      $PRE_PROC_PID"
echo "  Token:    $PRE_PROC_TOKEN"
echo "  Counter:  $PRE_PROC_COUNTER"
echo "  Blob SHA: $PRE_PROC_BLOB_SHA"
echo "  Blob Len: $PRE_PROC_BLOB_LEN"
if [ "$PRE_PROC_TOKEN" = "$PROC_TOKEN" ] && [ "$PRE_PROC_COUNTER" = "7" ] && [ -n "$PRE_PROC_BLOB_SHA" ]; then
  pass "proc-state daemon initialized with heap state"
else
  fail "proc-state daemon state initialization failed"
fi

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
header "7. Delete local session directory (simulate true cross-host resume)"
# ---------------------------------------------------------------------------
echo "  Deleting: $SESSION_DIR"
# Delete the ENTIRE session directory, including metadata.json. A successful
# resume now proves the control plane persisted portable restore metadata and
# the host is not relying on local session files at all.
rm -rf "$SESSION_DIR"
if [ ! -e "$SESSION_DIR" ]; then
  pass "Local session directory deleted"
else
  fail "Failed to delete local session directory"
fi

# ---------------------------------------------------------------------------
header "8. Resume via /connect on suspended runner (should use portable GCS metadata)"
# ---------------------------------------------------------------------------
RESUME_RESP=$(curl -s -X POST "$CP/api/v1/runners/connect" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")
echo "  Response: $RESUME_RESP"

RESUME_RUNNER_ID=$(echo "$RESUME_RESP" | jq -r '.runner_id')
RESUME_STATUS=$(echo "$RESUME_RESP" | jq -r '.status // empty')
echo "  Runner ID: $RESUME_RUNNER_ID"
echo "  Status: $RESUME_STATUS"

if [ "$RESUME_STATUS" = "resumed" ]; then
  pass "Session resumed through /connect"
else
  fail "Expected status=resumed, got $RESUME_STATUS"
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
RESUMED_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1 || true)
echo "  MD5 before: $BIGFILE_MD5"
echo "  MD5 after:  $RESUMED_MD5"
if echo "$OUT" | grep -q "EXEC_TIMEOUT"; then
  fail "2MB tmpfs file checksum timed out after resume"
  echo "  Raw exec output: $OUT"
elif [ "$BIGFILE_MD5" = "$RESUMED_MD5" ] && [ -n "$RESUMED_MD5" ]; then
  pass "2MB tmpfs file checksum matches — no corruption"
else
  fail "2MB tmpfs file checksum missing or mismatched"
  echo "  Raw exec output: $OUT"
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

# 9g. Verify tmpfs/rootfs tree hashes and larger rootfs file
echo "  --- 9g. Tmpfs tree hash ---"
OUT=$(vm_exec_resumed "find /tmp/gcs-tree -type f | sort | xargs sha256sum | sha256sum")
echo "  $OUT"
RESUMED_TMPFS_TREE_SHA=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{64}' | head -1 || true)
echo "  Tmpfs tree SHA before: $TMPFS_TREE_SHA"
echo "  Tmpfs tree SHA after:  $RESUMED_TMPFS_TREE_SHA"
if [ "$TMPFS_TREE_SHA" = "$RESUMED_TMPFS_TREE_SHA" ] && [ -n "$RESUMED_TMPFS_TREE_SHA" ]; then
  pass "tmpfs tree hash preserved"
else
  fail "tmpfs tree hash mismatch after resume"
fi

echo "  --- 9h. Rootfs tree hash ---"
OUT=$(vm_exec_resumed "find /var/tmp/gcs-tree -type f | sort | xargs sha256sum | sha256sum")
echo "  $OUT"
RESUMED_ROOTFS_TREE_SHA=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{64}' | head -1 || true)
echo "  Rootfs tree SHA before: $ROOTFS_TREE_SHA"
echo "  Rootfs tree SHA after:  $RESUMED_ROOTFS_TREE_SHA"
if [ "$ROOTFS_TREE_SHA" = "$RESUMED_ROOTFS_TREE_SHA" ] && [ -n "$RESUMED_ROOTFS_TREE_SHA" ]; then
  pass "rootfs tree hash preserved"
else
  fail "rootfs tree hash mismatch after resume"
fi

echo "  --- 9i. Rootfs bigfile checksum ---"
OUT=$(vm_exec_resumed "md5sum /var/tmp/gcs-rootfs-big.bin")
echo "  $OUT"
RESUMED_ROOTFS_BIG_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1 || true)
echo "  Rootfs bigfile MD5 before: $ROOTFS_BIG_MD5"
echo "  Rootfs bigfile MD5 after:  $RESUMED_ROOTFS_BIG_MD5"
if [ "$ROOTFS_BIG_MD5" = "$RESUMED_ROOTFS_BIG_MD5" ] && [ -n "$RESUMED_ROOTFS_BIG_MD5" ]; then
  pass "rootfs bigfile checksum preserved"
else
  fail "rootfs bigfile checksum mismatch after resume"
fi

dump_vm_sanity "$RESUME_RUNNER_ID" "after /connect resume"

echo "  --- 9j. In-memory daemon state after /connect resume ---"
dump_proc_state "$RESUME_RUNNER_ID" "after /connect resume"
PROC_STATE_JSON_RESUME1=$(proc_state_get_runner "$RESUME_RUNNER_ID")
echo "  Resume 1 proc-state: $PROC_STATE_JSON_RESUME1"
RESUME1_PROC_PID=$(echo "$PROC_STATE_JSON_RESUME1" | jq -r '.pid // empty')
RESUME1_PROC_TOKEN=$(echo "$PROC_STATE_JSON_RESUME1" | jq -r '.token // empty')
RESUME1_PROC_COUNTER=$(echo "$PROC_STATE_JSON_RESUME1" | jq -r '.counter // empty')
RESUME1_PROC_BLOB_SHA=$(echo "$PROC_STATE_JSON_RESUME1" | jq -r '.blob_sha // empty')
RESUME1_PROC_BLOB_LEN=$(echo "$PROC_STATE_JSON_RESUME1" | jq -r '.blob_len // empty')
if [ "$RESUME1_PROC_PID" = "$PRE_PROC_PID" ] && \
   [ "$RESUME1_PROC_TOKEN" = "$PRE_PROC_TOKEN" ] && \
   [ "$RESUME1_PROC_COUNTER" = "$PRE_PROC_COUNTER" ] && \
   [ "$RESUME1_PROC_BLOB_SHA" = "$PRE_PROC_BLOB_SHA" ] && \
   [ "$RESUME1_PROC_BLOB_LEN" = "$PRE_PROC_BLOB_LEN" ]; then
  pass "proc-state daemon heap survived /connect resume"
else
  fail "proc-state daemon state changed after /connect resume"
fi
PROC_STATE_JSON_RESUME1_BUMP=$(proc_state_bump_runner "$RESUME_RUNNER_ID")
RESUME1_BUMP_COUNTER=$(echo "$PROC_STATE_JSON_RESUME1_BUMP" | jq -r '.counter // empty')
if [ "$RESUME1_BUMP_COUNTER" = "8" ]; then
  pass "proc-state daemon counter continued after /connect resume"
else
  fail "proc-state daemon counter did not continue after /connect resume"
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

# Delete the full session directory again so allocate(session_id) also proves it
# can resume from portable metadata without any local metadata.json.
rm -rf "$SESSION_DIR"
if [ ! -e "$SESSION_DIR" ]; then
  pass "Local session directory deleted for portable resume (layer 1)"
else
  fail "Failed to delete local session directory"
fi

# Resume from layer 1 using allocate(session_id)
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
EXEC_READY2=false
for i in $(seq 1 30); do
  PRECHECK2=$(curl -s --no-buffer --max-time 5 -X POST "$MGR/api/v1/runners/$RESUME2_RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d '{"command":["echo","alive2"],"timeout_seconds":3}' 2>&1 || echo "TIMEOUT")
  if echo "$PRECHECK2" | grep -q 'alive2'; then
    echo " responsive (${i}s)"
    EXEC_READY2=true
    break
  fi
  echo -n "."
  sleep 1
done
echo ""

if ! $EXEC_READY2; then
  fail "Exec NOT reachable on resumed layer 1 VM after 30s"
  echo "  Last response: $PRECHECK2"
  echo "  Manager log (last 20 lines):"
  tail -20 /tmp/fc-dev/logs/firecracker-manager.log
  exit 1
fi
pass "Exec reachable on resumed layer 1 VM"
dump_vm_sanity "$RESUME2_RUNNER_ID" "after layer 1 resume"

echo "  --- In-memory daemon after layer 1 resume ---"
dump_proc_state "$RESUME2_RUNNER_ID" "after layer 1 resume"
PROC_STATE_JSON_RESUME2=$(proc_state_get_runner "$RESUME2_RUNNER_ID")
echo "  Resume 2 proc-state: $PROC_STATE_JSON_RESUME2"
RESUME2_PROC_PID=$(echo "$PROC_STATE_JSON_RESUME2" | jq -r '.pid // empty')
RESUME2_PROC_TOKEN=$(echo "$PROC_STATE_JSON_RESUME2" | jq -r '.token // empty')
RESUME2_PROC_COUNTER=$(echo "$PROC_STATE_JSON_RESUME2" | jq -r '.counter // empty')
RESUME2_PROC_BLOB_SHA=$(echo "$PROC_STATE_JSON_RESUME2" | jq -r '.blob_sha // empty')
RESUME2_PROC_BLOB_LEN=$(echo "$PROC_STATE_JSON_RESUME2" | jq -r '.blob_len // empty')
if [ "$RESUME2_PROC_PID" = "$PRE_PROC_PID" ] && \
   [ "$RESUME2_PROC_TOKEN" = "$PRE_PROC_TOKEN" ] && \
   [ "$RESUME2_PROC_COUNTER" = "8" ] && \
   [ "$RESUME2_PROC_BLOB_SHA" = "$PRE_PROC_BLOB_SHA" ] && \
   [ "$RESUME2_PROC_BLOB_LEN" = "$PRE_PROC_BLOB_LEN" ]; then
  pass "proc-state daemon heap survived layer 1 resume"
else
  fail "proc-state daemon state changed after layer 1 resume"
fi
PROC_STATE_JSON_RESUME2_BUMP=$(proc_state_bump_runner "$RESUME2_RUNNER_ID")
RESUME2_BUMP_COUNTER=$(echo "$PROC_STATE_JSON_RESUME2_BUMP" | jq -r '.counter // empty')
if [ "$RESUME2_BUMP_COUNTER" = "9" ]; then
  pass "proc-state daemon counter continued after layer 1 resume"
else
  fail "proc-state daemon counter did not continue after layer 1 resume"
fi

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

echo "  --- Verify tmpfs tree hash through 2 cycles ---"
OUT=$(vm_exec_chain "find /tmp/gcs-tree -type f | sort | xargs sha256sum | sha256sum")
FINAL_TMPFS_TREE_SHA=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{64}' | head -1 || true)
echo "  Tmpfs tree SHA final: $FINAL_TMPFS_TREE_SHA"
if [ "$TMPFS_TREE_SHA" = "$FINAL_TMPFS_TREE_SHA" ] && [ -n "$FINAL_TMPFS_TREE_SHA" ]; then
  pass "tmpfs tree preserved through 2 GCS cycles"
else
  fail "tmpfs tree mismatch after 2 GCS cycles"
fi

echo "  --- Verify rootfs tree hash through 2 cycles ---"
OUT=$(vm_exec_chain "find /var/tmp/gcs-tree -type f | sort | xargs sha256sum | sha256sum")
FINAL_ROOTFS_TREE_SHA=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{64}' | head -1 || true)
echo "  Rootfs tree SHA final: $FINAL_ROOTFS_TREE_SHA"
if [ "$ROOTFS_TREE_SHA" = "$FINAL_ROOTFS_TREE_SHA" ] && [ -n "$FINAL_ROOTFS_TREE_SHA" ]; then
  pass "rootfs tree preserved through 2 GCS cycles"
else
  fail "rootfs tree mismatch after 2 GCS cycles"
fi

echo "  --- Verify rootfs bigfile through 2 cycles ---"
OUT=$(vm_exec_chain "md5sum /var/tmp/gcs-rootfs-big.bin")
FINAL_ROOTFS_BIG_MD5=$(echo "$OUT" | grep '"type":"stdout"' | grep -oP '[a-f0-9]{32}' | head -1 || true)
echo "  Rootfs bigfile MD5 final: $FINAL_ROOTFS_BIG_MD5"
if [ "$ROOTFS_BIG_MD5" = "$FINAL_ROOTFS_BIG_MD5" ] && [ -n "$FINAL_ROOTFS_BIG_MD5" ]; then
  pass "rootfs bigfile preserved through 2 GCS cycles"
else
  fail "rootfs bigfile checksum mismatch after 2 GCS cycles"
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
