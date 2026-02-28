#!/bin/bash
# E2E test: GCS-backed rootfs durable snapshot (WS1)
#
# Tests that rootfs dirty chunks are uploaded to GCS during pause and restored
# on cross-host resume, preserving filesystem changes (files created, packages
# installed, etc.).
#
# Usage:
#   SESSION_CHUNK_BUCKET=rroy-gc-testing make dev-test-gcs-rootfs-durability
#
# Prerequisites:
#   - Golden chunked snapshot uploaded: GCS_BUCKET=<bucket> ENABLE_CHUNKED=true make dev-snapshot
#   - Stack running with GCS sessions:  SESSION_CHUNK_BUCKET=<bucket> make dev-stack
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
SESSION_ID="rootfs-e2e-$(date +%s)"
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

if [ -z "$GCS_BUCKET" ]; then
  echo "FAIL: SESSION_CHUNK_BUCKET is required."
  echo "Usage: SESSION_CHUNK_BUCKET=your-bucket make dev-test-gcs-rootfs-durability"
  exit 1
fi

echo "GCS bucket: $GCS_BUCKET"
echo "Session ID: $SESSION_ID"

# ---------------------------------------------------------------------------
header "1. Register snapshot config"
# ---------------------------------------------------------------------------
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}
CONFIG_RESP=$(curl -s -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "rootfs-durability-test",
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

HOST=${HOST_ADDR:-$MGR}
sleep 2  # wait for thaw-agent readiness

# ---------------------------------------------------------------------------
header "3. Write rootfs marker file"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "echo rootfs-marker > /var/tmp/rootfs-test.txt && echo ok"]}')
if echo "$EXEC_RESP" | grep -q "ok"; then
  pass "Wrote rootfs marker file"
else
  fail "Failed to write marker: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "4. Write 2MB file and capture md5"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "dd if=/dev/urandom of=/var/tmp/rootfs-big.bin bs=1M count=2 2>/dev/null && md5sum /var/tmp/rootfs-big.bin | cut -d\" \" -f1"]}')
ORIG_MD5=$(echo "$EXEC_RESP" | jq -rs '[.[] | select(.type=="stdout")] | last | .data' | tr -d '[:space:]')
echo "  original md5=$ORIG_MD5"
if [ -n "$ORIG_MD5" ] && [ ${#ORIG_MD5} -eq 32 ]; then
  pass "2MB file written with md5"
else
  fail "Failed to write 2MB file: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "5. Pause runner"
# ---------------------------------------------------------------------------
PAUSE_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/pause")
PAUSE_SESSION=$(echo "$PAUSE_RESP" | jq -r '.session_id')
echo "  pause response: $PAUSE_RESP"

if [ -n "$PAUSE_SESSION" ] && [ "$PAUSE_SESSION" != "null" ]; then
  pass "Runner paused"
else
  fail "Failed to pause: $PAUSE_RESP"
fi

sleep 2  # wait for GCS upload to complete

# ---------------------------------------------------------------------------
header "6. Verify rootfs disk index in GCS manifest"
# ---------------------------------------------------------------------------
# The manifest should have a non-empty Disk.ChunkIndexObject
SESSION_DIR=$(find /tmp/fc-dev/sessions/$SESSION_ID -name "metadata.json" 2>/dev/null | head -1 || true)
if [ -n "$SESSION_DIR" ]; then
  DISK_INDEX=$(jq -r '.gcs_disk_index_objects["__rootfs__"] // empty' "$SESSION_DIR" 2>/dev/null || true)
  if [ -n "$DISK_INDEX" ]; then
    pass "Rootfs disk index present in session metadata: $DISK_INDEX"
  else
    fail "No rootfs disk index (__rootfs__) in session metadata"
  fi
else
  echo "  (skipping local metadata check — cross-host mode)"
fi

# ---------------------------------------------------------------------------
header "7. Delete local session files (simulate cross-host)"
# ---------------------------------------------------------------------------
rm -rf /tmp/fc-dev/sessions/$SESSION_ID
pass "Deleted local session files"

# ---------------------------------------------------------------------------
header "8. Resume from GCS"
# ---------------------------------------------------------------------------
CONNECT_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/connect")
echo "  connect response: $CONNECT_RESP"
sleep 3  # wait for thaw-agent readiness

# ---------------------------------------------------------------------------
header "9. Verify rootfs marker survives"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["cat", "/var/tmp/rootfs-test.txt"]}')
if echo "$EXEC_RESP" | grep -q "rootfs-marker"; then
  pass "Rootfs marker survived cross-host resume"
else
  fail "Rootfs marker LOST: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "10. Verify 2MB file md5 matches"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "md5sum /var/tmp/rootfs-big.bin | cut -d\" \" -f1"]}')
RESUME_MD5=$(echo "$EXEC_RESP" | jq -rs '[.[] | select(.type=="stdout")] | last | .data' | tr -d '[:space:]')
echo "  resumed md5=$RESUME_MD5"
if [ "$ORIG_MD5" = "$RESUME_MD5" ]; then
  pass "2MB file md5 matches after resume"
else
  fail "md5 mismatch: original=$ORIG_MD5 resumed=$RESUME_MD5"
fi

# ---------------------------------------------------------------------------
header "11. Multi-pause chain: write more data → pause → delete → resume → verify"
# ---------------------------------------------------------------------------
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "echo chain-marker > /var/tmp/chain-test.txt && echo ok"]}')
if echo "$EXEC_RESP" | grep -q "ok"; then
  pass "Wrote chain marker"
else
  fail "Failed to write chain marker"
fi

# Second pause
PAUSE_RESP2=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/pause")
echo "  second pause: $PAUSE_RESP2"
sleep 2
rm -rf /tmp/fc-dev/sessions/$SESSION_ID

# Resume again
CONNECT_RESP2=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/connect")
sleep 3

# Verify ALL markers
EXEC_RESP=$(curl -s -X POST "$HOST/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command": ["bash", "-c", "cat /var/tmp/rootfs-test.txt && cat /var/tmp/chain-test.txt"]}')
if echo "$EXEC_RESP" | grep -q "rootfs-marker" && echo "$EXEC_RESP" | grep -q "chain-marker"; then
  pass "Both markers survived multi-pause chain"
else
  fail "Markers lost in chain: $EXEC_RESP"
fi

# ---------------------------------------------------------------------------
header "12. Release runner"
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
