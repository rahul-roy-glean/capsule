#!/bin/bash
# E2E test: allocate -> poll -> file ops -> release
# Usage: make dev-test-file-ops
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$REPO_ROOT/dev/lib-workload-key.sh"

CP=http://localhost:8080
MGR=http://localhost:9080

echo "=== E2E File Operations Test ==="
echo ""

# --- 0. Discover workload key from built snapshot ---
echo "=== 0. Discover workload key ==="
require_workload_key

# Register config so control plane knows TTL settings
register_dev_config "file-ops-test" '{"ttl": 60, "auto_pause": false}'

# --- 1. Allocate runner ---
echo "=== 1. Allocate runner ==="
RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\"}")
echo "Response: $RESP"

RUNNER_ID=$(echo "$RESP" | jq -r '.runner_id')
echo "Runner ID: $RUNNER_ID"

if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
  echo "FAIL: no runner_id in response"
  exit 1
fi

# --- 2. Poll until capsule-thaw-agent is ready ---
echo ""
echo "=== 2. Poll until ready ==="
for i in $(seq 1 60); do
  # Poll capsule-thaw-agent health endpoint via exec (most reliable)
  EXEC_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    "$MGR/api/v1/runners/$RUNNER_ID/exec" \
    -H 'Content-Type: application/json' \
    -d '{"command":["echo","ready"],"timeout_seconds":2}')
  if [ "$EXEC_CODE" = "200" ]; then
    echo "Runner ready after ${i}s"
    break
  fi
  if [ "$i" = "60" ]; then
    echo "FAIL: runner not ready after 60s (last HTTP status: $EXEC_CODE)"
    exit 1
  fi
  sleep 1
done
echo ""

# --- 3. Test /files/write ---
echo "=== 3. Test /files/write ==="
WRITE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/write" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-file.txt","content":"hello file ops\n"}')
echo "$WRITE_RESP"

BYTES=$(echo "$WRITE_RESP" | jq -r '.bytes_written')
if [ "$BYTES" = "null" ] || [ "$BYTES" -lt 1 ]; then
  echo "FAIL: write did not return bytes_written"
  exit 1
fi
echo "OK: wrote $BYTES bytes"

# --- 4. Test /files/read ---
echo ""
echo "=== 4. Test /files/read ==="
READ_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/read" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-file.txt"}')
echo "$READ_RESP"

CONTENT=$(echo "$READ_RESP" | jq -r '.content')
if [ "$CONTENT" = "hello file ops" ]; then
  echo "OK: content='$CONTENT'"
else
  echo "FAIL: expected content 'hello file ops', got '$CONTENT'"
  exit 1
fi

# --- 5. Test /files/stat ---
echo ""
echo "=== 5. Test /files/stat ==="
STAT_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/stat" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-file.txt"}')
echo "$STAT_RESP"

EXISTS=$(echo "$STAT_RESP" | jq -r '.exists')
if [ "$EXISTS" != "true" ]; then
  echo "FAIL: stat says file does not exist"
  exit 1
fi
echo "OK: file exists"

# --- 6. Test /files/mkdir ---
echo ""
echo "=== 6. Test /files/mkdir ==="
MKDIR_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/mkdir" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-subdir/nested"}')
echo "$MKDIR_RESP"

CREATED=$(echo "$MKDIR_RESP" | jq -r '.created')
if [ "$CREATED" != "true" ]; then
  echo "FAIL: mkdir did not return created=true"
  exit 1
fi
echo "OK: directory created"

# --- 7. Test /files/list ---
echo ""
echo "=== 7. Test /files/list ==="
LIST_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/list" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace"}')
echo "$LIST_RESP"

ENTRY_COUNT=$(echo "$LIST_RESP" | jq '.entries | length')
if [ "$ENTRY_COUNT" -lt 1 ]; then
  echo "FAIL: list returned no entries"
  exit 1
fi
echo "OK: listed $ENTRY_COUNT entries"

# --- 8. Test /files/remove ---
echo ""
echo "=== 8. Test /files/remove ==="
REMOVE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/remove" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-file.txt"}')
echo "$REMOVE_RESP"

REMOVED=$(echo "$REMOVE_RESP" | jq -r '.removed')
if [ "$REMOVED" != "true" ]; then
  echo "FAIL: remove did not return removed=true"
  exit 1
fi
echo "OK: file removed"

# Verify removal via stat
STAT2_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/stat" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-file.txt"}')
EXISTS2=$(echo "$STAT2_RESP" | jq -r '.exists')
if [ "$EXISTS2" != "false" ]; then
  echo "FAIL: file still exists after removal"
  exit 1
fi
echo "OK: confirmed file no longer exists"

# --- 9. Test /files/remove recursive ---
echo ""
echo "=== 9. Test /files/remove (recursive) ==="
RM_DIR_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/$RUNNER_ID/files/remove" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/workspace/test-subdir","recursive":true}')
echo "$RM_DIR_RESP"
echo "OK: recursive remove"

# --- 10. Test path validation (should get 403) ---
echo ""
echo "=== 10. Test path validation ==="
FORBIDDEN_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/$RUNNER_ID/files/read" \
  -H 'Content-Type: application/json' \
  -d '{"path":"/etc/passwd"}')
if [ "$FORBIDDEN_CODE" = "403" ]; then
  echo "OK: /etc/passwd correctly rejected with 403"
else
  echo "FAIL: expected 403 for /etc/passwd, got $FORBIDDEN_CODE"
  exit 1
fi

# --- 11. Release runner ---
echo ""
echo "=== 11. Release runner ==="
RELEASE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}")
echo "Response: $RELEASE_RESP"

echo ""
echo "========================================="
echo "=== ALL FILE OPS TESTS PASSED ==="
echo "========================================="
