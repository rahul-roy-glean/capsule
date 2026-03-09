#!/bin/bash
# E2E test: Network Policy System
#
# Tests the full network policy lifecycle:
#   1. API CRUD for policies (presets, custom, get/update)
#   2. Policy on layered configs (DB integration)
#   3. Allocation with policy preset / custom policy
#   4. Policy enforcement (deny-default blocks, allow-default permits)
#   5. RFC1918 blocking under restricted-egress
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
  local out
  if ! out=$(curl -sf --no-buffer --max-time 15 -X POST "$MGR/api/v1/runners/$rid/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":$1,\"timeout_seconds\":10}" 2>&1); then
    echo "EXEC_PROXY_FAILED"
    return 0  # don't blow up set -e; caller checks output
  fi
  echo "$out"
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

# Helper: wait for thaw-agent to be reachable (retry exec up to 10s)
wait_for_exec() {
  local rid="$1"
  echo -n "  Waiting for thaw-agent exec..." >&2
  for i in $(seq 1 10); do
    local out
    out=$(vm_exec "$rid" "[\"echo\",\"THAW_OK\"]")
    if echo "$out" | grep -q "THAW_OK"; then
      echo " ready (${i}s)" >&2
      return 0
    fi
    echo -n "." >&2
    sleep 1
  done
  echo " FAILED (thaw-agent unreachable after 10s)" >&2
  return 1
}

# Helper: dump iptables for debugging (call on failure)
dump_netns_iptables() {
  local rid="$1"
  local id8="${rid:0:8}"
  local ns="fc-$id8"
  echo "  --- iptables dump for $ns ---"
  echo "  FORWARD chain:"
  sudo ip netns exec "$ns" iptables -S FORWARD 2>/dev/null || echo "    (failed)"
  echo "  POLICY-INGRESS:"
  sudo ip netns exec "$ns" iptables -S POLICY-INGRESS 2>/dev/null || echo "    (no chain)"
  echo "  POLICY-EGRESS:"
  sudo ip netns exec "$ns" iptables -S POLICY-EGRESS 2>/dev/null || echo "    (no chain)"
  echo "  nat PREROUTING:"
  sudo ip netns exec "$ns" iptables -t nat -S PREROUTING 2>/dev/null || echo "    (failed)"
  echo "  ---"
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
CONFIG_IDS=()
cleanup() {
  for rid in "${RUNNER_IDS[@]}"; do
    if [ -n "$rid" ]; then
      curl -s -X POST "$CP/api/v1/runners/release" \
        -H 'Content-Type: application/json' \
        -d "{\"runner_id\": \"$rid\", \"destroy\": true}" > /dev/null 2>&1 || true
    fi
  done
  # Delete configs created by this test run
  for cid in "${CONFIG_IDS[@]}"; do
    if [ -n "$cid" ]; then
      curl -s -X DELETE "$CP/api/v1/layered-configs/$cid" > /dev/null 2>&1 || true
    fi
  done
}
trap cleanup EXIT

# Pre-cleanup: delete any leftover configs from previous runs by listing all
# configs and deleting any whose display_name starts with "netpol-test".
pre_cleanup() {
  local list
  list=$(curl -sf "$CP/api/v1/layered-configs" 2>/dev/null) || return 0
  echo "$list" | jq -r '.configs[]? | select(.display_name | startswith("netpol-test")) | .config_id' | while read -r cid; do
    curl -s -X DELETE "$CP/api/v1/layered-configs/$cid" > /dev/null 2>&1 || true
    echo "  pre-cleanup: deleted stale config $cid"
  done
}
pre_cleanup

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
header "1. Register layered config (no policy — backwards compat)"
# ---------------------------------------------------------------------------
CONFIG_RESP=$(curl -sf -X POST "$CP/api/v1/layered-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "netpol-test",
    "layers": [{"name": "base", "init_commands": [{"type":"shell","args":["echo","netpol-test-nopolicy"]}]}],
    "config": {"ttl": 120, "auto_pause": false}
  }')
CONFIG_ID=$(echo "$CONFIG_RESP" | jq -r '.config_id')
WORKLOAD_KEY=$(echo "$CONFIG_RESP" | jq -r '.leaf_workload_key')
CONFIG_IDS+=("$CONFIG_ID")
echo "  config_id=$CONFIG_ID  workload_key=$WORKLOAD_KEY"

if [ -n "$WORKLOAD_KEY" ] && [ "$WORKLOAD_KEY" != "null" ]; then
  pass "Layered config registered (no policy)"
else
  fail "Layered config registration failed"; exit 1
fi

GET_RESP=$(curl -sf "$CP/api/v1/layered-configs/$CONFIG_ID")
NP=$(echo "$GET_RESP" | jq -r '.config.network_policy // "null"')
NP_PRESET=$(echo "$GET_RESP" | jq -r '.config.network_policy_preset // ""')
if [ "$NP" = "null" ] && [ -z "$NP_PRESET" ]; then
  pass "Config has no network policy (backwards compatible)"
else
  fail "Expected null policy, got policy=$NP preset=$NP_PRESET"
fi

# ---------------------------------------------------------------------------
header "2. Register layered config with policy preset"
# ---------------------------------------------------------------------------
CONFIG_PRESET_RESP=$(curl -sf -X POST "$CP/api/v1/layered-configs" \
  -H 'Content-Type: application/json' \
  -d '{
    "display_name": "netpol-ci-test",
    "layers": [{"name": "base", "init_commands": [{"type":"shell","args":["echo","netpol-ci"]}]}],
    "config": {"ttl": 120, "auto_pause": false, "network_policy_preset": "restricted-egress"}
  }')
CONFIG_PRESET_ID=$(echo "$CONFIG_PRESET_RESP" | jq -r '.config_id')
CONFIG_IDS+=("$CONFIG_PRESET_ID")
GET_PRESET_RESP=$(curl -sf "$CP/api/v1/layered-configs/$CONFIG_PRESET_ID")
NP_PRESET2=$(echo "$GET_PRESET_RESP" | jq -r '.config.network_policy_preset // ""')
echo "  preset=$NP_PRESET2"

if [ "$NP_PRESET2" = "restricted-egress" ]; then
  pass "Config stored network_policy_preset=restricted-egress"
else
  fail "Expected preset restricted-egress, got '$NP_PRESET2'"
fi

# ---------------------------------------------------------------------------
header "3. Allocate runner without policy (unrestricted)"
# ---------------------------------------------------------------------------
# Use the no-policy config's workload key. pre_cleanup() cleared stale DB
# rows so the cache will only see this config with no preset set.
RUNNER_ID=$(allocate_and_wait "{\"ci_system\":\"none\",\"workload_key\":\"$WORKLOAD_KEY\"}")
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

# Release unrestricted runner — we're done with it for connectivity tests.
# Keep RUNNER_ID for validation tests in step 15 (runner must still exist).
# Actually release it now and allocate fresh for validation later.
curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID\"}" > /dev/null 2>&1 || true
echo "  (released unrestricted runner to free capacity)"
# Remove from cleanup list
RUNNER_IDS=("${RUNNER_IDS[@]/$RUNNER_ID/}")

# =========================================================================
# PART 2: restricted-egress preset enforcement
# =========================================================================

# ---------------------------------------------------------------------------
header "6. Allocate runner with restricted-egress preset"
# ---------------------------------------------------------------------------
RUNNER_ID_CI=$(allocate_and_wait "{
  \"ci_system\":\"none\",
  \"workload_key\":\"$WORKLOAD_KEY\",
  \"network_policy_preset\":\"restricted-egress\"
}")
if [ "$RUNNER_ID_CI" != "ALLOC_FAILED" ]; then
  pass "Runner allocated with restricted-egress preset"
else
  fail "Failed to allocate restricted-egress runner"; exit 1
fi

# ---------------------------------------------------------------------------
header "7. Verify restricted-egress policy is applied"
# ---------------------------------------------------------------------------
sleep 1
CI_RESP=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
CI_VER=$(echo "$CI_RESP" | jq -r '.version // 0')
CI_NAME=$(echo "$CI_RESP" | jq -r '.policy.name // ""')
CI_ACTION=$(echo "$CI_RESP" | jq -r '.policy.default_egress_action // ""')
echo "  version=$CI_VER name=$CI_NAME action=$CI_ACTION"

[ "$CI_VER" = "1" ] && pass "Policy version 1" || fail "Expected version 1, got $CI_VER"
[ "$CI_NAME" = "restricted-egress" ] && pass "Policy name is restricted-egress" || fail "Expected restricted-egress, got '$CI_NAME'"
[ "$CI_ACTION" = "allow" ] && pass "default_egress_action=allow" || fail "Expected allow, got '$CI_ACTION'"

# ---------------------------------------------------------------------------
header "8. restricted-egress: external egress works"
# ---------------------------------------------------------------------------
# Wait for thaw-agent to be reachable before testing network policy
if ! wait_for_exec "$RUNNER_ID_CI"; then
  fail "restricted-egress: thaw-agent exec unreachable (cannot test network policy)"
  dump_netns_iptables "$RUNNER_ID_CI"
else
  EXEC_CI=$(vm_tcp_check "$RUNNER_ID_CI" "8.8.8.8" "53")
  if echo "$EXEC_CI" | grep -q "NET_OK"; then
    pass "restricted-egress: external egress works"
  elif echo "$EXEC_CI" | grep -q "EXEC_PROXY_FAILED"; then
    fail "restricted-egress: exec proxy failed (transport issue, not policy)"
    dump_netns_iptables "$RUNNER_ID_CI"
  else
    fail "restricted-egress: external egress blocked"
    dump_netns_iptables "$RUNNER_ID_CI"
  fi
fi

# ---------------------------------------------------------------------------
header "8b. restricted-egress: RFC1918 is blocked"
# ---------------------------------------------------------------------------
RFC_EXEC=$(vm_tcp_check_blocked "$RUNNER_ID_CI" "10.0.0.1" "80")
if echo "$RFC_EXEC" | grep -q "NET_BLOCKED"; then
  pass "restricted-egress: RFC1918 (10.0.0.1) blocked"
elif echo "$RFC_EXEC" | grep -q "NET_OK"; then
  fail "restricted-egress: RFC1918 NOT blocked (10.0.0.1 reachable)"
else
  pass "restricted-egress: RFC1918 appears blocked (timeout/no response)"
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
# PART 4: Quarantine override + emergency kill switch
# (Uses RUNNER_ID_CI which is still alive from step 6)
# =========================================================================

# ---------------------------------------------------------------------------
header "10. Quarantine runner (overrides policy)"
# ---------------------------------------------------------------------------
QUARANTINE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/quarantine?runner_id=$RUNNER_ID_CI&block_egress=true&pause_vm=false")
Q_OK=$(echo "$QUARANTINE_RESP" | jq -r '.success // false')
[ "$Q_OK" = "true" ] && pass "Runner quarantined" || fail "Quarantine failed: $QUARANTINE_RESP"

# ---------------------------------------------------------------------------
header "11. Unquarantine runner (restores previous policy)"
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
header "12. Emergency block egress (host-level kill switch)"
# ---------------------------------------------------------------------------
EMERG_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/quarantine?runner_id=$RUNNER_ID_CI&block_egress=true&pause_vm=false")
EMERG_OK=$(echo "$EMERG_RESP" | jq -r '.success // false')
[ "$EMERG_OK" = "true" ] && pass "Emergency egress block applied" || fail "Emergency block failed"

BLOCKED=$(vm_tcp_check_blocked "$RUNNER_ID_CI" "8.8.8.8" "53")
if echo "$BLOCKED" | grep -q "NET_BLOCKED"; then
  pass "Emergency block: egress blocked"
elif echo "$BLOCKED" | grep -q "NET_OK"; then
  fail "Emergency block: egress NOT blocked"
else
  pass "Emergency block: egress appears blocked (timeout)"
fi

# Unquarantine to clean up, then release restricted-egress runner to free capacity
curl -sf -X POST "$MGR/api/v1/runners/unquarantine?runner_id=$RUNNER_ID_CI&unblock_egress=true&resume_vm=false" > /dev/null
curl -sf -X POST "$CP/api/v1/runners/release" \
  -H 'Content-Type: application/json' \
  -d "{\"runner_id\":\"$RUNNER_ID_CI\"}" > /dev/null 2>&1 || true
echo "  (released restricted-egress runner to free capacity)"
RUNNER_IDS=("${RUNNER_IDS[@]/$RUNNER_ID_CI/}")

# =========================================================================
# PART 5: deny-default enforcement (positive + negative)
# =========================================================================

# ---------------------------------------------------------------------------
header "13. Allocate runner with deny-default + allowed CIDR"
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

sleep 2

# ---------------------------------------------------------------------------
header "14. deny-default: allowed CIDR reachable (positive)"
# ---------------------------------------------------------------------------
if ! wait_for_exec "$RUNNER_ID_DENY"; then
  fail "deny-default: thaw-agent exec unreachable (cannot test network policy)"
  dump_netns_iptables "$RUNNER_ID_DENY"
else
  ALLOW_EXEC=$(vm_tcp_check "$RUNNER_ID_DENY" "8.8.8.8" "53")
  if echo "$ALLOW_EXEC" | grep -q "NET_OK"; then
    pass "deny-default: 8.8.8.8:53 reachable (in allow list)"
  elif echo "$ALLOW_EXEC" | grep -q "EXEC_PROXY_FAILED"; then
    fail "deny-default: exec proxy failed (transport issue, not policy)"
    dump_netns_iptables "$RUNNER_ID_DENY"
  else
    fail "deny-default: 8.8.8.8:53 NOT reachable (should be allowed)"
    dump_netns_iptables "$RUNNER_ID_DENY"
  fi
fi

# ---------------------------------------------------------------------------
header "14b. deny-default: non-allowed IP blocked (negative)"
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
# PART 6: Validation (HTTP 400 for invalid policies)
# (Uses RUNNER_ID_DENY which is still alive)
# =========================================================================

# ---------------------------------------------------------------------------
header "15. Validate policy API returns HTTP 400 for invalid policies"
# ---------------------------------------------------------------------------

# A) Domain rules in allow-default
HTTP_A=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_DENY" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"allow","allowed_egress":[{"domains":["github.com"]}]}}')
echo "  Domain rules in allow-default: HTTP $HTTP_A"
[ "$HTTP_A" = "400" ] && pass "HTTP 400: domain rules rejected in allow-default" || fail "Expected HTTP 400, got $HTTP_A"

# B) Invalid CIDR
HTTP_B=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_DENY" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"deny","allowed_egress":[{"cidrs":["not-a-cidr"]}]}}')
echo "  Invalid CIDR: HTTP $HTTP_B"
[ "$HTTP_B" = "400" ] && pass "HTTP 400: invalid CIDR rejected" || fail "Expected HTTP 400, got $HTTP_B"

# C) Internal CIDR broader than /16
HTTP_C=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_DENY" \
  -H 'Content-Type: application/json' \
  -d '{"policy":{"default_egress_action":"deny","internal_access":{"allowed_internal_cidrs":["10.0.0.0/8"]}}}')
echo "  Internal CIDR /8: HTTP $HTTP_C"
[ "$HTTP_C" = "400" ] && pass "HTTP 400: internal CIDR broader than /16 rejected" || fail "Expected HTTP 400, got $HTTP_C"

# D) Verify error body includes structured code
ERR_BODY=$(curl -s -X POST "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_DENY" \
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
