#!/bin/bash
# E2E smoke test: host-side garbage collection
#
# This script restarts the local dev stack with aggressive GC settings,
# creates a mix of stale and active host-local artifacts, waits for the
# janitor loop to run, and verifies:
#   - stale sessions/session-state/chunk-cache/logs/quarantine are reclaimed
#   - active runner/session artifacts are preserved
#
# Usage:
#   make dev-test-host-gc
#
# Prerequisites:
#   - A dev snapshot exists: make dev-snapshot
#   - The host can run the dev stack locally (Linux + KVM)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CP=http://localhost:8080
MGR=http://localhost:9080
BASE=/tmp/fc-dev
SESSIONS_DIR="$BASE/sessions"
SOCKET_DIR="$BASE/sockets"
STATE_DIR="$SOCKET_DIR/session-state"
SNAPSHOT_DIR="$BASE/snapshots"
CHUNK_DIR="$SNAPSHOT_DIR/chunks"
LOG_DIR="$BASE/logs"
QUAR_DIR="$BASE/quarantine"

PASS=0
FAIL=0
RUNNER_ID=""
ACTIVE_SESSION=""

. "$REPO_ROOT/dev/lib-workload-key.sh"

pass() { echo "  [PASS] $1"; PASS=$((PASS + 1)); }
fail() { echo "  [FAIL] $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

cleanup() {
  if [ -n "$RUNNER_ID" ]; then
    curl -s -X POST "$CP/api/v1/runners/release" \
      -H 'Content-Type: application/json' \
      -d "{\"runner_id\":\"$RUNNER_ID\", \"destroy\": true}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

wait_http_200() {
  local url="$1"
  local label="$2"
  local max_wait="${3:-60}"
  echo -n "  Waiting for $label"
  for i in $(seq 1 "$max_wait"); do
    if curl -sf "$url" >/dev/null 2>&1; then
      echo " ready (${i}s)"
      return 0
    fi
    echo -n "."
    sleep 1
  done
  echo " FAILED"
  return 1
}

wait_runner_ready() {
  local runner_id="$1"
  echo -n "  Waiting for runner $runner_id"
  for i in $(seq 1 60); do
    local code
    code=$(curl -s -o /dev/null -w "%{http_code}" "$CP/api/v1/runners/status?runner_id=$runner_id")
    if [ "$code" = "200" ]; then
      echo " ready (${i}s)"
      return 0
    fi
    echo -n "."
    sleep 1
  done
  echo " FAILED"
  return 1
}

write_file_of_size() {
  local path="$1"
  local bytes="$2"
  mkdir -p "$(dirname "$path")"
  python3 - "$path" "$bytes" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
size = int(sys.argv[2])
path.write_bytes(b"x" * size)
PY
}

set_old_mtime() {
  local path="$1"
  touch -d '2 minutes ago' "$path"
}

write_session_metadata() {
  local session_id="$1"
  local runner_id="$2"
  local path="$3"
  cat > "$path" <<EOF
{
  "session_id": "$session_id",
  "runner_id": "$runner_id",
  "workload_key": "dev-workload",
  "layers": 1,
  "created_at": "2024-01-01T00:00:00Z",
  "paused_at": "2024-01-01T00:00:00Z",
  "session_max_age_seconds": 10
}
EOF
}

header "1. Restart stack with aggressive GC settings"
CAPSULE_MANAGER_EXTRA_FLAGS="--gc-session-sweep-interval=5s --gc-session-default-max-age=10s --gc-session-state-max-age=10s --gc-chunk-cache-max-bytes=1048576 --gc-chunk-cache-low-watermark=0.5 --gc-log-max-age=10s --gc-quarantine-max-age=10s" \
  bash "$REPO_ROOT/dev/run-stack.sh"
wait_http_200 "$CP/health" "control-plane health" 10 || { fail "control-plane did not become healthy"; exit 1; }
wait_http_200 "$MGR/health" "capsule-manager health" 10 || { fail "capsule-manager did not become healthy"; exit 1; }
pass "Stack restarted with short GC intervals"

header "2. Discover workload key and allocate active runner"
require_workload_key
register_dev_config "host-gc-smoke" '{"ttl": 300, "auto_pause": false, "session_max_age_seconds": 3600}'
ACTIVE_SESSION="gc-active-$(date +%s)"
ALLOC_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$ACTIVE_SESSION\"}")
RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  fail "failed to allocate active runner"
  echo "$ALLOC_RESP"
  exit 1
fi
wait_runner_ready "$RUNNER_ID" || { fail "active runner did not become ready"; exit 1; }
pass "Allocated active runner $RUNNER_ID with session $ACTIVE_SESSION"

header "3. Seed stale and active host artifacts"
mkdir -p "$SESSIONS_DIR" "$STATE_DIR" "$CHUNK_DIR" "$LOG_DIR" "$QUAR_DIR"

STALE_SESSION="gc-stale-session"
mkdir -p "$SESSIONS_DIR/$STALE_SESSION/layer_0"
write_session_metadata "$STALE_SESSION" "runner-stale" "$SESSIONS_DIR/$STALE_SESSION/metadata.json"
set_old_mtime "$SESSIONS_DIR/$STALE_SESSION/metadata.json"

mkdir -p "$SESSIONS_DIR/$ACTIVE_SESSION/layer_0"
write_session_metadata "$ACTIVE_SESSION" "$RUNNER_ID" "$SESSIONS_DIR/$ACTIVE_SESSION/metadata.json"
set_old_mtime "$SESSIONS_DIR/$ACTIVE_SESSION/metadata.json"

write_file_of_size "$STATE_DIR/runner-stale.state" 4096
set_old_mtime "$STATE_DIR/runner-stale.state"
write_file_of_size "$STATE_DIR/$RUNNER_ID.state" 4096
set_old_mtime "$STATE_DIR/$RUNNER_ID.state"

write_file_of_size "$CHUNK_DIR/aa/chunk-oldest" 450000
set_old_mtime "$CHUNK_DIR/aa/chunk-oldest"
write_file_of_size "$CHUNK_DIR/bb/chunk-older" 450000
set_old_mtime "$CHUNK_DIR/bb/chunk-older"
write_file_of_size "$CHUNK_DIR/cc/chunk-newest" 450000

write_file_of_size "$LOG_DIR/runner-stale.log" 128
set_old_mtime "$LOG_DIR/runner-stale.log"
write_file_of_size "$LOG_DIR/runner-stale.console.log" 128
set_old_mtime "$LOG_DIR/runner-stale.console.log"
write_file_of_size "$LOG_DIR/runner-stale.metrics" 128
set_old_mtime "$LOG_DIR/runner-stale.metrics"
write_file_of_size "$LOG_DIR/$RUNNER_ID.log" 128
set_old_mtime "$LOG_DIR/$RUNNER_ID.log"

mkdir -p "$QUAR_DIR/runner-stale"
write_file_of_size "$QUAR_DIR/runner-stale/manifest.json" 128
set_old_mtime "$QUAR_DIR/runner-stale/manifest.json"
touch -d '2 minutes ago' "$QUAR_DIR/runner-stale"
mkdir -p "$QUAR_DIR/$RUNNER_ID"
write_file_of_size "$QUAR_DIR/$RUNNER_ID/manifest.json" 128
set_old_mtime "$QUAR_DIR/$RUNNER_ID/manifest.json"
touch -d '2 minutes ago' "$QUAR_DIR/$RUNNER_ID"

pass "Created stale and active artifact fixtures"

header "4. Wait for janitor and verify cleanup"
GC_DONE=false
for i in $(seq 1 20); do
  if [ ! -e "$SESSIONS_DIR/$STALE_SESSION" ] && \
     [ ! -e "$STATE_DIR/runner-stale.state" ] && \
     [ ! -e "$LOG_DIR/runner-stale.log" ] && \
     [ ! -e "$QUAR_DIR/runner-stale" ] && \
     [ ! -e "$CHUNK_DIR/aa/chunk-oldest" ] && \
     [ ! -e "$CHUNK_DIR/bb/chunk-older" ]; then
    GC_DONE=true
    echo "  Janitor completed after ${i}s"
    break
  fi
  sleep 2
done

if ! $GC_DONE; then
  fail "janitor did not clean stale artifacts within timeout"
  echo "  Manager log tail:"
  tail -40 "$LOG_DIR/capsule-manager.log" || true
fi

if [ ! -e "$SESSIONS_DIR/$STALE_SESSION" ]; then pass "stale session dir removed"; else fail "stale session dir still present"; fi
if [ -e "$SESSIONS_DIR/$ACTIVE_SESSION" ]; then pass "active session dir preserved"; else fail "active session dir was incorrectly removed"; fi

if [ ! -e "$STATE_DIR/runner-stale.state" ]; then pass "stale session-state file removed"; else fail "stale session-state file still present"; fi
if [ -e "$STATE_DIR/$RUNNER_ID.state" ]; then pass "active session-state file preserved"; else fail "active session-state file was incorrectly removed"; fi

if [ ! -e "$LOG_DIR/runner-stale.log" ] && [ ! -e "$LOG_DIR/runner-stale.console.log" ] && [ ! -e "$LOG_DIR/runner-stale.metrics" ]; then
  pass "stale runner logs removed"
else
  fail "stale runner logs still present"
fi
if [ -e "$LOG_DIR/$RUNNER_ID.log" ]; then pass "active runner log preserved"; else fail "active runner log was incorrectly removed"; fi

if [ ! -e "$QUAR_DIR/runner-stale" ]; then pass "stale quarantine dir removed"; else fail "stale quarantine dir still present"; fi
if [ -e "$QUAR_DIR/$RUNNER_ID" ]; then pass "active quarantine dir preserved"; else fail "active quarantine dir was incorrectly removed"; fi

if [ ! -e "$CHUNK_DIR/aa/chunk-oldest" ] && [ ! -e "$CHUNK_DIR/bb/chunk-older" ] && [ -e "$CHUNK_DIR/cc/chunk-newest" ]; then
  pass "chunk cache pruned oldest files to low watermark"
else
  fail "chunk cache pruning did not match expected oldest-first behavior"
fi

header "5. Summary"
echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -ne 0 ]; then
  echo "Host GC smoke test FAILED"
  exit 1
fi
echo "Host GC smoke test PASSED"
echo ""
echo "Note: the stack is still running with aggressive GC flags."
echo "Re-run 'make dev-stack' afterwards if you want to restore the default local settings."
