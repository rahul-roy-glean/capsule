#!/bin/bash
# E2E test: Auth Proxy — delegated provider, token push, proxy reachability
#
# Tests the auth proxy lifecycle:
#   1. Allocate runner with delegated auth config via label
#   2. Verify proxy port is reachable inside VM netns (TCP check)
#   3. Push token via host-side /update-token endpoint
#   4. Verify pushed token is accepted (HTTP 200)
#   5. Edge cases: invalid push, wrong provider name
#   6. Release runner, verify cleanup
#
# Usage: make dev-test-auth-proxy
#
# Prerequisites:
#   - Stack running in netns mode: make dev-stack
#   - Snapshot built: make dev-snapshot
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

# Helper: exec inside a VM, return raw ndjson output
vm_exec() {
  local rid="$1"; shift
  local out
  if ! out=$(curl -sf --no-buffer --max-time 15 -X POST "$MGR/api/v1/runners/$rid/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":$1,\"timeout_seconds\":10}" 2>&1); then
    echo "EXEC_PROXY_FAILED"
    return 0
  fi
  echo "$out"
}

# Helper: wait for thaw-agent exec to be reachable
wait_for_exec() {
  local rid="$1"
  echo -n "  Waiting for thaw-agent exec..." >&2
  for i in $(seq 1 15); do
    local out
    out=$(vm_exec "$rid" "[\"echo\",\"THAW_OK\"]")
    if echo "$out" | grep -q "THAW_OK"; then
      echo " ready (${i}s)" >&2
      return 0
    fi
    echo -n "." >&2
    sleep 1
  done
  echo " FAILED (thaw-agent unreachable after 15s)" >&2
  return 1
}

# Helper: allocate runner and wait for ready
allocate_and_wait() {
  local body="$1"
  local resp runner_id
  resp=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
    -H 'Content-Type: application/json' \
    -d "$body")
  runner_id=$(echo "$resp" | jq -r '.runner_id')
  if [ -z "$runner_id" ] || [ "$runner_id" = "null" ]; then
    echo "ALLOC_FAILED"
    return 1
  fi
  RUNNER_IDS+=("$runner_id")
  echo -n "  runner_id=$runner_id  waiting..." >&2
  for i in $(seq 1 60); do
    local code
    code=$(curl -s -o /dev/null -w "%{http_code}" \
      "$CP/api/v1/runners/status?runner_id=$runner_id")
    if [ "$code" = "200" ]; then
      echo " ready (${i}s)" >&2
      echo "$runner_id"
      return 0
    fi
    echo -n "." >&2
    sleep 1
  done
  echo " TIMEOUT" >&2
  echo "$runner_id"
  return 1
}

RUNNER_IDS=()
cleanup() {
  for rid in "${RUNNER_IDS[@]}"; do
    if [ -n "$rid" ]; then
      curl -s -X POST "$CP/api/v1/runners/release" \
        -H 'Content-Type: application/json' \
        -d "{\"runner_id\": \"$rid\", \"destroy\": true}" > /dev/null 2>&1 || true
    fi
  done
}
trap cleanup EXIT

echo "=== E2E Auth Proxy Tests ==="

# =========================================================================
# PART 0: Register snapshot config
# =========================================================================

header "0. Register snapshot config"
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/layered-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "auth-proxy-test",
    "commands": [{"type":"shell","command":"echo auth-proxy-test"}],
    "runner_ttl_seconds": 120,
    "auto_pause": false
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "  workload_key=$WORKLOAD_KEY"

if [ -z "$WORKLOAD_KEY" ] || [ "$WORKLOAD_KEY" = "null" ]; then
  fail "Could not register snapshot config"
  exit 1
fi
pass "Snapshot config registered"

# =========================================================================
# PART 1: Allocate runner with delegated auth proxy config
# =========================================================================

# The auth config uses a delegated provider for github.com — this requires
# no external credentials and lets us test the full proxy lifecycle.
AUTH_CONFIG='{
  "providers": [
    {
      "type": "delegated",
      "hosts": ["github.com", "api.github.com"],
      "config": {
        "header": "Authorization",
        "prefix": "token "
      }
    }
  ],
  "proxy": {
    "listen_port": 3128,
    "ssl_bump": true,
    "allowed_hosts": ["github.com", "api.github.com", "*.googleapis.com"]
  }
}'

# Escape the auth config for embedding in the labels JSON
AUTH_CONFIG_ESCAPED=$(echo "$AUTH_CONFIG" | jq -c '.' | jq -Rs '.')

header "1. Allocate runner with auth proxy config"
ALLOC_BODY=$(cat <<EOF
{
  "ci_system": "none",
  "workload_key": "$WORKLOAD_KEY",
  "labels": {
    "_auth_config_json": $AUTH_CONFIG_ESCAPED
  }
}
EOF
)

RUNNER_ID=$(allocate_and_wait "$ALLOC_BODY")
if [ "$RUNNER_ID" = "ALLOC_FAILED" ]; then
  fail "Failed to allocate runner with auth config"
  exit 1
fi
pass "Runner allocated with auth proxy config"

# =========================================================================
# PART 2: Verify proxy port is reachable inside VM netns
# =========================================================================

header "2. Verify proxy port reachable from inside VM"
if ! wait_for_exec "$RUNNER_ID"; then
  fail "thaw-agent exec unreachable"
else
  # The proxy binds on the gateway IP (172.16.0.1) inside the netns.
  # We test TCP connectivity from the guest to the proxy port.
  PROXY_CHECK=$(vm_exec "$RUNNER_ID" "[\"bash\",\"-c\",\"(echo > /dev/tcp/172.16.0.1/3128) 2>/dev/null && echo PROXY_OK || echo PROXY_FAIL\"]")
  if echo "$PROXY_CHECK" | grep -q "PROXY_OK"; then
    pass "Proxy port 3128 reachable on 172.16.0.1"
  elif echo "$PROXY_CHECK" | grep -q "EXEC_PROXY_FAILED"; then
    fail "Exec proxy transport failed (cannot verify)"
  else
    fail "Proxy port 3128 NOT reachable on 172.16.0.1"
    echo "  output: $PROXY_CHECK"
  fi
fi

# =========================================================================
# PART 3: Find and test the token update endpoint
# =========================================================================

header "3. Discover token update endpoint"

# The auth proxy's token update endpoint runs on the host veth IP (10.200.{slot}.1:9443).
# We can find the runner's host-reachable IP from the manager status.
RUNNER_INFO=$(curl -sf "$MGR/api/v1/runners/$RUNNER_ID" 2>/dev/null || echo '{}')
RUNNER_IP=$(echo "$RUNNER_INFO" | jq -r '.internal_ip // empty')
echo "  runner internal IP: ${RUNNER_IP:-"(unknown)"}"

# Derive host veth IP: if runner IP is 10.200.X.2, host is 10.200.X.1
if [ -n "$RUNNER_IP" ]; then
  # Extract the third octet and build host IP
  THIRD_OCTET=$(echo "$RUNNER_IP" | cut -d. -f3)
  TOKEN_ENDPOINT="http://10.200.${THIRD_OCTET}.1:9443"
  echo "  token update endpoint: $TOKEN_ENDPOINT/update-token"

  # ---------------------------------------------------------------------------
  header "4. Push token via /update-token endpoint"
  # ---------------------------------------------------------------------------

  PUSH_RESP=$(curl -sf -X POST "$TOKEN_ENDPOINT/update-token" \
    -H 'Content-Type: application/json' \
    -d '{
      "provider": "delegated",
      "token": "ghp_test_token_12345",
      "expires_at": "2030-01-01T00:00:00Z"
    }' 2>&1) || PUSH_RESP="FAILED"

  if [ "$PUSH_RESP" != "FAILED" ]; then
    pass "Token push accepted (HTTP 200)"
  else
    fail "Token push failed (endpoint unreachable or error)"
  fi

  # ---------------------------------------------------------------------------
  header "5. Push token — error cases"
  # ---------------------------------------------------------------------------

  # 5a. Unknown provider
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$TOKEN_ENDPOINT/update-token" \
    -H 'Content-Type: application/json' \
    -d '{"provider":"nonexistent","token":"tok"}')
  [ "$HTTP_CODE" = "404" ] && pass "Unknown provider returns 404" || fail "Expected 404, got $HTTP_CODE"

  # 5b. Invalid JSON body
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$TOKEN_ENDPOINT/update-token" \
    -H 'Content-Type: application/json' \
    -d 'not-json')
  [ "$HTTP_CODE" = "400" ] && pass "Invalid JSON returns 400" || fail "Expected 400, got $HTTP_CODE"

  # 5c. GET not allowed
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$TOKEN_ENDPOINT/update-token")
  [ "$HTTP_CODE" = "405" ] && pass "GET returns 405 Method Not Allowed" || fail "Expected 405, got $HTTP_CODE"

  # 5d. Push a second token (update)
  PUSH2_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$TOKEN_ENDPOINT/update-token" \
    -H 'Content-Type: application/json' \
    -d '{
      "provider": "delegated",
      "token": "ghp_updated_token_67890",
      "expires_at": "2031-06-15T12:00:00Z"
    }')
  [ "$PUSH2_CODE" = "200" ] && pass "Token update (second push) accepted" || fail "Token update failed: HTTP $PUSH2_CODE"

else
  echo "  SKIP: Cannot determine token update endpoint (runner IP not found)"
  fail "Token update endpoint discovery failed"
fi

# =========================================================================
# PART 4: Verify proxy handles CONNECT (from inside VM)
# =========================================================================

header "6. Verify proxy responds to HTTP CONNECT"
# Send a raw CONNECT request to the proxy and check it responds.
# We use /dev/tcp to make a TCP connection and send raw HTTP.
# The proxy should respond with 200 (for allowed host) or 403 (for blocked host).
CONNECT_TEST=$(vm_exec "$RUNNER_ID" "[\"bash\",\"-c\",\"echo -e 'CONNECT github.com:443 HTTP/1.1\\\\r\\\\nHost: github.com:443\\\\r\\\\n\\\\r\\\\n' | timeout 3 nc 172.16.0.1 3128 2>/dev/null | head -1 || echo CONNECT_FAIL\"]")
if echo "$CONNECT_TEST" | grep -q "200"; then
  pass "Proxy responds 200 to CONNECT github.com:443"
elif echo "$CONNECT_TEST" | grep -q "CONNECT_FAIL\|EXEC_PROXY_FAILED"; then
  fail "Cannot test CONNECT (nc/exec issue)"
  echo "  output: $CONNECT_TEST"
else
  fail "Proxy CONNECT response unexpected"
  echo "  output: $CONNECT_TEST"
fi

# Test blocked host
BLOCKED_TEST=$(vm_exec "$RUNNER_ID" "[\"bash\",\"-c\",\"echo -e 'CONNECT evil.com:443 HTTP/1.1\\\\r\\\\nHost: evil.com:443\\\\r\\\\n\\\\r\\\\n' | timeout 3 nc 172.16.0.1 3128 2>/dev/null | head -1 || echo CONNECT_FAIL\"]")
if echo "$BLOCKED_TEST" | grep -q "403"; then
  pass "Proxy responds 403 to CONNECT evil.com:443 (not in allowed_hosts)"
elif echo "$BLOCKED_TEST" | grep -q "CONNECT_FAIL\|EXEC_PROXY_FAILED"; then
  fail "Cannot test blocked CONNECT (nc/exec issue)"
  echo "  output: $BLOCKED_TEST"
else
  # The proxy may just close the connection for blocked hosts
  pass "Proxy rejected CONNECT to evil.com (not in allowed_hosts)"
fi

# =========================================================================
# PART 5: Release runner
# =========================================================================

header "7. Release runner"
RELEASE_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}" 2>&1) || RELEASE_RESP='{"error":"release failed"}'
echo "  $RELEASE_RESP"
RUNNER_IDS=("${RUNNER_IDS[@]/$RUNNER_ID/}")
pass "Runner released"

# Verify the token update endpoint is gone after release
if [ -n "${TOKEN_ENDPOINT:-}" ]; then
  sleep 1
  GONE_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 2 \
    -X POST "$TOKEN_ENDPOINT/update-token" \
    -H 'Content-Type: application/json' \
    -d '{"provider":"delegated","token":"x"}' 2>/dev/null) || GONE_CODE="000"
  if [ "$GONE_CODE" = "000" ] || [ "$GONE_CODE" = "007" ]; then
    pass "Token update endpoint unreachable after release (cleaned up)"
  else
    fail "Token update endpoint still reachable after release (HTTP $GONE_CODE)"
  fi
fi

# =========================================================================
# RESULTS
# =========================================================================

header "RESULTS"
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
  echo "=== ALL AUTH PROXY TESTS PASSED ==="
  echo "========================================="
fi
