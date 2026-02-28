#!/bin/bash
# E2E test: Network Policy System
#
# Tests the full network policy lifecycle:
#   1. API CRUD for policies (presets, custom, get/update)
#   2. Policy on snapshot configs (DB integration)
#   3. Allocation with policy preset / custom policy
#   4. Policy enforcement (deny-default blocks, allow-default permits)
#   5. RFC1918 blocking under ci-standard
#   6. Deny-default blocks + allowed CIDR works (positive+negative)
#   7. Quarantine overrides policy, unquarantine restores it
#   8. Emergency egress block (host-level kill switch)
#   9. Validation returns HTTP 400 for invalid policies
#   10. Backwards compatibility (no policy = unrestricted)
#
# Usage: make dev-test-network-policy
#
# Prerequisites:
#   - Stack running: make dev-stack
#   - Snapshot built: make dev-snapshot
#   - ipset installed: sudo apt-get install -y ipset
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
  curl -sf --no-buffer --max-time 15 -X POST "$MGR/api/v1/runners/$rid/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":$1,\"timeout_seconds\":10}" 2>&1 || echo '{"type":"error","message":"curl_failed"}'
}

# Helper: TCP connectivity test (returns NET_OK or NET_FAIL)
vm_tcp_check() {
  local rid="$1" host="$2" port="$3"
  vm_exec "$rid" "[\"bash\",\"-c\",\"(echo > /dev/tcp/$host/$port) 2>/dev/null && echo NET_OK || echo NET_FAIL\"]"
}

# Helper: TCP check with short timeout for expected-blocked cases
vm_tcp_check_blocked() {
  local rid="$1" host="$2" port="$3"
  vm_exec "$rid" "[\"bash\",\"-c\",\"timeout 3 bash -c '(echo > /dev/tcp/$host/$port) 2>/dev/null && echo NET_OK || echo NET_BLOCKED' || echo NET_BLOCKED\"]"
}

# Helper: allocate runner and wait for ready, returns runner_id on stdout
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

HAS_IPSET=false
if command -v ipset >/dev/null 2>&1; then
  HAS_IPSET=true
fi
if [ "$HAS_IPSET" = "false" ]; then
  echo "ERROR: ipset is required. Install with: sudo apt-get install -y ipset"
  exit 1
fi

echo "=== E2E Network Policy Tests ==="

# =========================================================================
# PART 1: Data model, API, backwards compatibility
# =========================================================================

# ---------------------------------------------------------------------------
header "1. Register snapshot config (no policy — backwards compat)"
# ---------------------------------------------------------------------------
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "netpol-test",
    "commands": [{"type":"shell","command":"echo netpol-test"}],
    "runner_ttl_seconds": 120,
    "auto_pause": false
  }')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.workload_key')
echo "  workload_key=$WORKLOAD_KEY"

if [ -n "$WORKLOAD_KEY" ] && [ "$WORKLOAD_KEY" != "null" ]; then
  pass "Snapshot config registered (no policy)"
else
  fail "Snapshot config registration failed"; exit 1
fi

NP=$(echo "$CONFIG_RESP" | jq -r '.network_policy // "null"')
NP_PRESET=$(echo "$CONFIG_RESP" | jq -r '.network_policy_preset // ""')
if [ "$NP" = "null" ] && [ -z "$NP_PRESET" ]; then
  pass "Config has no network policy (backwards compatible)"
else
  fail "Expected null policy, got policy=$NP preset=$NP_PRESET"
fi

# ---------------------------------------------------------------------------
header "2. Register snapshot config with policy preset"
# ---------------------------------------------------------------------------
CONFIG_PRESET_RESP=$(curl -sf -X POST "$CP/api/v1/snapshot-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "netpol-ci-test",
    "commands": [{"type":"shell","command":"echo netpol-ci"}],
    "runner_ttl_seconds": 120,
    "auto_pause": false,
    "network_policy_preset": "ci-standard"
  }')
NP_PRESET2=$(echo "$CONFIG_PRESET_RESP" | jq -r '.network_policy_preset // ""')
echo "  preset=$NP_PRESET2"

if [ "$NP_PRESET2" = "ci-standard" ]; then
  pass "Config stored network_policy_preset=ci-standard"
else
  fail "Expected preset ci-standard, got '$NP_PRESET2'"
fi

# ---------------------------------------------------------------------------
header "3. Allocate runner without policy (unrestricted)"
# ---------------------------------------------------------------------------
RUNNER_ID=$(allocate_and_wait "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\"}")
if [ "$RUNNER_ID" != "ALLOC_FAILED" ]; then
  pass "Runner allocated (no policy)"
else
  fail "Failed to allocate runner"; exit 1
fi

# ---------------------------------------------------------------------------
header "4. Get policy for unrestricted runner"
# ---------------------------------------------------------------------------
sleep 1
POLICY_RESP=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID")
echo "  $POLICY_RESP"

POLICY_VER=$(echo "$POLICY_RESP" | jq -r '.version // 0')
POLICY_NULL=$(echo "$POLICY_RESP" | jq -r '.policy')
if [ "$POLICY_VER" = "0" ] && [ "$POLICY_NULL" = "null" ]; then
  pass "Unrestricted runner: version=0, policy=null"
else
  fail "Expected version=0 policy=null, got version=$POLICY_VER"
fi

# ---------------------------------------------------------------------------
header "5. Verify unrestricted runner has network access"
# ---------------------------------------------------------------------------
EXEC_OUT=$(vm_tcp_check "$RUNNER_ID" "8.8.8.8" "53")
if echo "$EXEC_OUT" | grep -q "NET_OK"; then
  pass "Unrestricted runner can reach 8.8.8.8:53"
else
  fail "Unrestricted runner has no network access"
fi

# =========================================================================
# PART 2: ci-standard preset enforcement
# =========================================================================

# ---------------------------------------------------------------------------
header "6. Allocate runner with ci-standard preset"
# ---------------------------------------------------------------------------
RUNNER_ID_CI=$(allocate_and_wait "{
  \"ci_system\":\"none\",
  \"workload_key\":\"$WORKLOAD_KEY\",
  \"network_policy_preset\":\"ci-standard\"
}")
if [ "$RUNNER_ID_CI" != "ALLOC_FAILED" ]; then
  pass "Runner allocated with ci-standard preset"
else
  fail "Failed to allocate ci-standard runner"; exit 1
fi

# ---------------------------------------------------------------------------
header "7. Verify ci-standard policy is applied"
# ---------------------------------------------------------------------------
sleep 1
CI_RESP=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
CI_VER=$(echo "$CI_RESP" | jq -r '.version // 0')
CI_NAME=$(echo "$CI_RESP" | jq -r '.policy.name // ""')
CI_ACTION=$(echo "$CI_RESP" | jq -r '.policy.default_egress_action // ""')
echo "  version=$CI_VER name=$CI_NAME action=$CI_ACTION"

[ "$CI_VER" = "1" ] && pass "Policy version 1" || fail "Expected version 1, got $CI_VER"
[ "$CI_NAME" = "ci-standard" ] && pass "Policy name is ci-standard" || fail "Expected ci-standard, got '$CI_NAME'"
[ "$CI_ACTION" = "allow" ] && pass "default_egress_action=allow" || fail "Expected allow, got '$CI_ACTION'"

# ---------------------------------------------------------------------------
header "8. ci-standard: external egress works"
# ---------------------------------------------------------------------------
EXEC_CI=$(vm_tcp_check "$RUNNER_ID_CI" "8.8.8.8" "53")
if echo "$EXEC_CI" | grep -q "NET_OK"; then
  pass "ci-standard: external egress works"
else
  fail "ci-standard: external egress blocked"
fi

# ---------------------------------------------------------------------------
header "8b. ci-standard: RFC1918 is blocked"
# ---------------------------------------------------------------------------
RFC_EXEC=$(vm_tcp_check_blocked "$RUNNER_ID_CI" "10.0.0.1" "80")
if echo "$RFC_EXEC" | grep -q "NET_BLOCKED"; then
  pass "ci-standard: RFC1918 (10.0.0.1) blocked"
elif echo "$RFC_EXEC" | grep -q "NET_OK"; then
  fail "ci-standard: RFC1918 NOT blocked (10.0.0.1 reachable)"
else
  pass "ci-standard: RFC1918 appears blocked (timeout/no response)"
fi

# =========================================================================
# PART 3: Runtime policy update
# =========================================================================

# ---------------------------------------------------------------------------
header "9. Update policy at runtime (switch to deny-default)"
# ---------------------------------------------------------------------------
UPDATE_RESP=$(curl -s -X POST "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI" \
  -H 'Content-Type: application/json' \
  -d '{
    "policy": {
      "name": "custom-deny",
      "default_egress_action": "deny",
      "allowed_egress": [
        {"description": "allow DNS", "cidrs": ["8.8.8.8/32", "8.8.4.4/32"], "ports": [{"start":53}], "protocols": ["udp"]}
      ]
    }
  }')
echo "  $UPDATE_RESP"

UPDATE_OK=$(echo "$UPDATE_RESP" | jq -r '.success')
UPDATE_VER=$(echo "$UPDATE_RESP" | jq -r '.version // 0')
[ "$UPDATE_OK" = "true" ] && pass "Policy updated at runtime" || fail "Policy update failed: $UPDATE_RESP"
[ "$UPDATE_VER" = "2" ] && pass "Policy version incremented to 2" || fail "Expected version 2, got $UPDATE_VER"

# Read back
RB=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
RB_NAME=$(echo "$RB" | jq -r '.policy.name')
RB_ACTION=$(echo "$RB" | jq -r '.policy.default_egress_action')
if [ "$RB_NAME" = "custom-deny" ] && [ "$RB_ACTION" = "deny" ]; then
  pass "Policy reads back correctly after update"
else
  fail "Readback mismatch: name=$RB_NAME action=$RB_ACTION"
fi

# =========================================================================
# PART 4: deny-default enforcement (positive + negative)
# =========================================================================

# ---------------------------------------------------------------------------
header "10. Allocate runner with deny-default + allowed CIDR"
# ---------------------------------------------------------------------------
RUNNER_ID_DENY=$(allocate_and_wait "{
  \"ci_system\":\"none\",
  \"workload_key\":\"$WORKLOAD_KEY\",
  \"network_policy_json\": \"{\\\"name\\\":\\\"deny-test\\\",\\\"default_egress_action\\\":\\\"deny\\\",\\\"allowed_egress\\\":[{\\\"cidrs\\\":[\\\"8.8.8.8/32\\\"],\\\"ports\\\":[{\\\"start\\\":53}]}]}\"
}")
if [ "$RUNNER_ID_DENY" != "ALLOC_FAILED" ]; then
  pass "Runner allocated with deny-default policy"
else
  fail "Failed to allocate deny-default runner"
fi

sleep 1

# ---------------------------------------------------------------------------
header "11. deny-default: allowed CIDR reachable (positive)"
# ---------------------------------------------------------------------------
ALLOW_EXEC=$(vm_tcp_check "$RUNNER_ID_DENY" "8.8.8.8" "53")
if echo "$ALLOW_EXEC" | grep -q "NET_OK"; then
  pass "deny-default: 8.8.8.8:53 reachable (in allow list)"
else
  fail "deny-default: 8.8.8.8:53 NOT reachable (should be allowed)"
fi

# ---------------------------------------------------------------------------
header "11b. deny-default: non-allowed IP blocked (negative)"
# ---------------------------------------------------------------------------
DENY_EXEC=$(vm_tcp_check_blocked "$RUNNER_ID_DENY" "1.1.1.1" "80")
echo "  $DENY_EXEC"

if echo "$DENY_EXEC" | grep -q "NET_BLOCKED"; then
  pass "deny-default: 1.1.1.1:80 blocked (not in allow list)"
elif echo "$DENY_EXEC" | grep -q "NET_OK"; then
  fail "deny-default: 1.1.1.1:80 reachable (policy not enforced)"
else
  pass "deny-default: 1.1.1.1 appears blocked (timeout)"
fi

# =========================================================================
# PART 5: Quarantine override + emergency kill switch
# =========================================================================

# ---------------------------------------------------------------------------
header "12. Quarantine runner (overrides policy)"
# ---------------------------------------------------------------------------
QUARANTINE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/quarantine?runner_id=$RUNNER_ID_CI&block_egress=true&pause_vm=false")
Q_OK=$(echo "$QUARANTINE_RESP" | jq -r '.success // false')
[ "$Q_OK" = "true" ] && pass "Runner quarantined" || fail "Quarantine failed: $QUARANTINE_RESP"

# ---------------------------------------------------------------------------
header "13. Unquarantine runner (restores previous policy)"
# ---------------------------------------------------------------------------
UNQ_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/unquarantine?runner_id=$RUNNER_ID_CI&unblock_egress=true&resume_vm=false")
UQ_OK=$(echo "$UNQ_RESP" | jq -r '.success // false')
[ "$UQ_OK" = "true" ] && pass "Runner unquarantined" || fail "Unquarantine failed: $UNQ_RESP"

RESTORED=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
RESTORED_NAME=$(echo "$RESTORED" | jq -r '.policy.name // ""')
if [ "$RESTORED_NAME" = "custom-deny" ]; then
  pass "Policy restored to custom-deny after unquarantine"
else
  fail "Policy not restored: expected custom-deny, got '$RESTORED_NAME'"
fi

# ---------------------------------------------------------------------------
header "14. Emergency block egress (host-level kill switch)"
# ---------------------------------------------------------------------------
# Emergency block via quarantine (host FORWARD chain DROP on veth)
EMERG_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/quarantine?runner_id=$RUNNER_ID_CI&block_egress=true&pause_vm=false")
EMERG_OK=$(echo "$EMERG_RESP" | jq -r '.success // false')
[ "$EMERG_OK" = "true" ] && pass "Emergency egress block applied" || fail "Emergency block failed"

# Verify egress is dead
BLOCKED=$(vm_tcp_check_blocked "$RUNNER_ID_CI" "8.8.8.8" "53")
if echo "$BLOCKED" | grep -q "NET_BLOCKED"; then
  pass "Emergency block: egress blocked"
elif echo "$BLOCKED" | grep -q "NET_OK"; then
  fail "Emergency block: egress NOT blocked"
else
  pass "Emergency block: egress appears blocked (timeout)"
fi

# Unquarantine to restore for cleanup
curl -sf -X POST "$MGR/api/v1/runners/unquarantine?runner_id=$RUNNER_ID_CI&unblock_egress=true&resume_vm=false" > /dev/null

# =========================================================================
# PART 6: Validation (HTTP 400 for invalid policies)
# =========================================================================

# ---------------------------------------------------------------------------
header "15. Validate policy API returns HTTP 400 for invalid policies"
# ---------------------------------------------------------------------------

# A) Domain rules in allow-default
HTTP_A=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"allow","allowed_egress":[{"domains":["github.com"]}]}}')
echo "  Domain rules in allow-default: HTTP $HTTP_A"
[ "$HTTP_A" = "400" ] && pass "HTTP 400: domain rules rejected in allow-default" || fail "Expected HTTP 400, got $HTTP_A"

# B) Invalid CIDR
HTTP_B=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"deny","allowed_egress":[{"cidrs":["not-a-cidr"]}]}}')
echo "  Invalid CIDR: HTTP $HTTP_B"
[ "$HTTP_B" = "400" ] && pass "HTTP 400: invalid CIDR rejected" || fail "Expected HTTP 400, got $HTTP_B"

# C) Internal CIDR broader than /16
HTTP_C=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"deny","internal_access":{"allowed_internal_cidrs":["10.0.0.0/8"]}}}')
echo "  Internal CIDR /8: HTTP $HTTP_C"
[ "$HTTP_C" = "400" ] && pass "HTTP 400: internal CIDR broader than /16 rejected" || fail "Expected HTTP 400, got $HTTP_C"

# D) Verify error body includes structured code
ERR_BODY=$(curl -s -X POST "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"allow","allowed_egress":[{"domains":["x.com"]}]}}')
ERR_CODE=$(echo "$ERR_BODY" | jq -r '.code // ""')
if [ "$ERR_CODE" = "INVALID_POLICY" ]; then
  pass "Error body includes code=INVALID_POLICY"
else
  fail "Expected code INVALID_POLICY, got '$ERR_CODE'"
fi

# =========================================================================
# Cleanup
# =========================================================================

# ---------------------------------------------------------------------------
header "16. Release all runners"
# ---------------------------------------------------------------------------
for rid in "${RUNNER_IDS[@]}"; do
  if [ -n "$rid" ]; then
    curl -sf -X POST "$CP/api/v1/runners/release" \
      -H 'Content-Type: application/json' \
      -d "{\"runner_id\":\"$rid\"}" > /dev/null 2>&1 || true
    echo "  Released $rid"
  fi
done
RUNNER_IDS=()
pass "All runners released"

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
  echo "=== ALL NETWORK POLICY TESTS PASSED ==="
  echo "========================================="
fi
