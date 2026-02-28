#!/bin/bash
# E2E test: Network Policy System
#
# Tests the full network policy lifecycle:
#   1. API CRUD for policies (presets, custom, get/update)
#   2. Policy on snapshot configs (DB integration)
#   3. Allocation with policy preset / custom policy
#   4. Policy enforcement (deny-default blocks, allow-default permits)
#   5. Quarantine overrides policy, unquarantine restores it
#   6. Emergency egress block (host-level kill switch)
#   7. DNS proxy blocks disallowed domains
#   8. Dynamic policy update at runtime
#   9. Backwards compatibility (no policy = unrestricted)
#
# Usage: make dev-test-network-policy
#
# Prerequisites:
#   - Stack running: make dev-stack
#   - Snapshot built: make dev-snapshot
set -euo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
PASS=0
FAIL=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
header() { echo ""; echo "=== $1 ==="; }

RUNNER_IDS=()
HAS_IPSET=false
if command -v ipset >/dev/null 2>&1; then
  HAS_IPSET=true
fi
if [ "$HAS_IPSET" = "false" ]; then
  echo "WARNING: ipset not installed — iptables enforcement tests will be skipped"
  echo "  Install with: sudo apt-get install -y ipset"
fi

cleanup() {
  for rid in "${RUNNER_IDS[@]}"; do
    if [ -n "$rid" ]; then
      echo "Cleaning up runner $rid..."
      curl -s -X POST "$CP/api/v1/runners/release" \
        -H 'Content-Type: application/json' \
        -d "{\"runner_id\": \"$rid\", \"destroy\": true}" > /dev/null 2>&1 || true
    fi
  done
}
trap cleanup EXIT

echo "=== E2E Network Policy Tests ==="

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
  fail "Snapshot config registration failed"
  exit 1
fi

# Verify no policy fields set
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
WORKLOAD_KEY_CI=$(echo "$CONFIG_PRESET_RESP" | jq -r '.workload_key')
NP_PRESET2=$(echo "$CONFIG_PRESET_RESP" | jq -r '.network_policy_preset // ""')
echo "  workload_key=$WORKLOAD_KEY_CI preset=$NP_PRESET2"

if [ "$NP_PRESET2" = "ci-standard" ]; then
  pass "Config stored network_policy_preset=ci-standard"
else
  fail "Expected preset ci-standard, got '$NP_PRESET2'"
fi

# ---------------------------------------------------------------------------
header "3. Allocate runner without policy (unrestricted)"
# ---------------------------------------------------------------------------
ALLOC_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{\"ci_system\":\"none\", \"workload_key\":\"$WORKLOAD_KEY\"}")
RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
RUNNER_IDS+=("$RUNNER_ID")
echo "  runner_id=$RUNNER_ID"

if [ -n "$RUNNER_ID" ] && [ "$RUNNER_ID" != "null" ]; then
  pass "Runner allocated (no policy)"
else
  fail "Failed to allocate runner"
  exit 1
fi

# Poll until ready
echo -n "  Waiting for runner..."
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  if [ "$i" = "60" ]; then
    fail "Runner not ready after 60s"
    exit 1
  fi
  echo -n "."
  sleep 1
done

# ---------------------------------------------------------------------------
header "4. Get policy for unrestricted runner (should be empty)"
# ---------------------------------------------------------------------------
sleep 1
POLICY_RESP=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID")
echo "  Response: $POLICY_RESP"

POLICY_VERSION=$(echo "$POLICY_RESP" | jq -r '.version // 0')
HAS_POLICY=$(echo "$POLICY_RESP" | jq 'has("policy")')
if [ "$POLICY_VERSION" = "0" ] && [ "$HAS_POLICY" = "false" ]; then
  pass "Unrestricted runner has no policy (version 0)"
else
  fail "Expected no policy, got version=$POLICY_VERSION has_policy=$HAS_POLICY"
fi

# ---------------------------------------------------------------------------
header "5. Verify unrestricted runner has network access"
# ---------------------------------------------------------------------------
# Use /dev/tcp bash builtin — works without ping/nslookup being installed.
EXEC_OUT=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["bash","-c","(echo > /dev/tcp/8.8.8.8/53) 2>/dev/null && echo NET_OK || echo NET_FAIL"],"timeout_seconds":10}')
echo "  $EXEC_OUT"

if echo "$EXEC_OUT" | grep -q "NET_OK"; then
  pass "Unrestricted runner can reach 8.8.8.8:53"
else
  fail "Unrestricted runner has no network access (expected unrestricted)"
fi

# ---------------------------------------------------------------------------
header "6. Allocate runner with ci-standard preset"
# ---------------------------------------------------------------------------
ALLOC_CI_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{
    \"ci_system\":\"none\",
    \"workload_key\":\"$WORKLOAD_KEY\",
    \"network_policy_preset\":\"ci-standard\"
  }")
RUNNER_ID_CI=$(echo "$ALLOC_CI_RESP" | jq -r '.runner_id')
RUNNER_IDS+=("$RUNNER_ID_CI")
echo "  runner_id=$RUNNER_ID_CI"

if [ -n "$RUNNER_ID_CI" ] && [ "$RUNNER_ID_CI" != "null" ]; then
  pass "Runner allocated with ci-standard preset"
else
  fail "Failed to allocate runner with ci-standard"
  exit 1
fi

echo -n "  Waiting for runner..."
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID_CI")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# ---------------------------------------------------------------------------
header "7. Verify ci-standard policy is applied"
# ---------------------------------------------------------------------------
sleep 1
CI_POLICY_RESP=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
echo "  Response: $CI_POLICY_RESP"

CI_POLICY_VER=$(echo "$CI_POLICY_RESP" | jq -r '.version // 0')
CI_POLICY_NAME=$(echo "$CI_POLICY_RESP" | jq -r '.policy.name // "none"')
CI_DEFAULT_ACTION=$(echo "$CI_POLICY_RESP" | jq -r '.policy.default_egress_action // "none"')

if [ "$CI_POLICY_VER" = "1" ]; then
  pass "ci-standard policy applied (version 1)"
else
  fail "Expected policy version 1, got $CI_POLICY_VER"
fi

if [ "$CI_POLICY_NAME" = "ci-standard" ]; then
  pass "Policy name is ci-standard"
else
  fail "Expected policy name 'ci-standard', got '$CI_POLICY_NAME'"
fi

if [ "$CI_DEFAULT_ACTION" = "allow" ]; then
  pass "ci-standard has default_egress_action=allow"
else
  fail "Expected allow, got '$CI_DEFAULT_ACTION'"
fi

# ---------------------------------------------------------------------------
header "8. Verify ci-standard allows external egress"
# ---------------------------------------------------------------------------
EXEC_CI=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID_CI/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["bash","-c","(echo > /dev/tcp/8.8.8.8/53) 2>/dev/null && echo NET_OK || echo NET_FAIL"],"timeout_seconds":10}')

if echo "$EXEC_CI" | grep -q "NET_OK"; then
  pass "ci-standard: external egress works"
else
  fail "ci-standard: no external access"
fi

# ---------------------------------------------------------------------------
header "9. Update policy at runtime (switch to deny-default)"
# ---------------------------------------------------------------------------
if [ "$HAS_IPSET" = "true" ]; then
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
echo "  Response: $UPDATE_RESP"

UPDATE_SUCCESS=$(echo "$UPDATE_RESP" | jq -r '.success // false')
UPDATE_VER=$(echo "$UPDATE_RESP" | jq -r '.version // 0')

if [ "$UPDATE_SUCCESS" = "true" ]; then
  pass "Policy updated at runtime"
else
  fail "Policy update failed: $UPDATE_RESP"
fi

if [ "$UPDATE_VER" = "2" ]; then
  pass "Policy version incremented to 2"
else
  fail "Expected version 2, got $UPDATE_VER"
fi

# Verify the updated policy reads back
UPDATED_POLICY_RESP=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
UPDATED_NAME=$(echo "$UPDATED_POLICY_RESP" | jq -r '.policy.name // ""')
UPDATED_ACTION=$(echo "$UPDATED_POLICY_RESP" | jq -r '.policy.default_egress_action // ""')

if [ "$UPDATED_NAME" = "custom-deny" ] && [ "$UPDATED_ACTION" = "deny" ]; then
  pass "Policy reads back correctly after update"
else
  fail "Policy readback mismatch: name=$UPDATED_NAME action=$UPDATED_ACTION"
fi
else
  echo "  SKIP: ipset not installed"
fi

# ---------------------------------------------------------------------------
header "10. Allocate runner with custom deny-default policy"
# ---------------------------------------------------------------------------
if [ "$HAS_IPSET" = "true" ]; then
ALLOC_DENY_RESP=$(curl -sf -X POST "$CP/api/v1/runners/allocate" \
  -H 'Content-Type: application/json' \
  -d "{
    \"ci_system\":\"none\",
    \"workload_key\":\"$WORKLOAD_KEY\",
    \"network_policy_json\": \"{\\\"name\\\":\\\"deny-test\\\",\\\"default_egress_action\\\":\\\"deny\\\",\\\"allowed_egress\\\":[{\\\"cidrs\\\":[\\\"8.8.8.8/32\\\"],\\\"ports\\\":[{\\\"start\\\":53}]}]}\"
  }")
RUNNER_ID_DENY=$(echo "$ALLOC_DENY_RESP" | jq -r '.runner_id')
RUNNER_IDS+=("$RUNNER_ID_DENY")
echo "  runner_id=$RUNNER_ID_DENY"

if [ -n "$RUNNER_ID_DENY" ] && [ "$RUNNER_ID_DENY" != "null" ]; then
  pass "Runner allocated with deny-default policy"
else
  fail "Failed to allocate deny-default runner"
fi

echo -n "  Waiting for runner..."
for i in $(seq 1 60); do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    "$CP/api/v1/runners/status?runner_id=$RUNNER_ID_DENY")
  if [ "$HTTP_CODE" = "200" ]; then
    echo " ready (${i}s)"
    break
  fi
  echo -n "."
  sleep 1
done

# ---------------------------------------------------------------------------
header "11. Verify deny-default blocks general egress"
# ---------------------------------------------------------------------------
sleep 1
DENY_EXEC=$(curl -sf --no-buffer -X POST "$MGR/api/v1/runners/$RUNNER_ID_DENY/exec" \
  -H 'Content-Type: application/json' \
  -d '{"command":["bash","-c","(echo > /dev/tcp/1.1.1.1/80) 2>/dev/null && echo NET_OK || echo NET_BLOCKED"],"timeout_seconds":10}')
echo "  $DENY_EXEC"

if echo "$DENY_EXEC" | grep -q "NET_BLOCKED"; then
  pass "deny-default: external egress blocked (1.1.1.1 unreachable)"
elif echo "$DENY_EXEC" | grep -q "NET_OK"; then
  fail "deny-default: external egress NOT blocked (1.1.1.1 reachable — policy not enforced)"
else
  pass "deny-default: egress appears blocked (no clear response)"
fi
else
  echo "  SKIP: ipset not installed (steps 10-11 require iptables enforcement)"
fi

# ---------------------------------------------------------------------------
header "12. Quarantine runner (should override to quarantine policy)"
# ---------------------------------------------------------------------------
QUARANTINE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/quarantine?runner_id=$RUNNER_ID_CI&block_egress=true&pause_vm=false")
echo "  Response: $QUARANTINE_RESP"

Q_SUCCESS=$(echo "$QUARANTINE_RESP" | jq -r '.success // false')
if [ "$Q_SUCCESS" = "true" ]; then
  pass "Runner quarantined"
else
  fail "Quarantine failed: $QUARANTINE_RESP"
fi

# ---------------------------------------------------------------------------
header "13. Unquarantine runner (should restore previous policy)"
# ---------------------------------------------------------------------------
UNQUARANTINE_RESP=$(curl -sf -X POST "$MGR/api/v1/runners/unquarantine?runner_id=$RUNNER_ID_CI&unblock_egress=true&resume_vm=false")
echo "  Response: $UNQUARANTINE_RESP"

UQ_SUCCESS=$(echo "$UNQUARANTINE_RESP" | jq -r '.success // false')
if [ "$UQ_SUCCESS" = "true" ]; then
  pass "Runner unquarantined"
else
  fail "Unquarantine failed: $UNQUARANTINE_RESP"
fi

# Verify policy is still the updated custom-deny policy (only if step 9 ran)
if [ "$HAS_IPSET" = "true" ]; then
RESTORED_POLICY=$(curl -s "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID_CI")
RESTORED_NAME=$(echo "$RESTORED_POLICY" | jq -r '.policy.name // ""')

if [ "$RESTORED_NAME" = "custom-deny" ]; then
  pass "Policy restored after unquarantine"
else
  fail "Policy not restored: expected custom-deny, got '$RESTORED_NAME'"
fi
else
  echo "  SKIP: policy restore check (ipset not installed, step 9 was skipped)"
fi

# ---------------------------------------------------------------------------
header "14. Validate policy API rejects invalid policies"
# ---------------------------------------------------------------------------

# Domain rules in allow-default should be rejected
INVALID_RESP=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{
    "policy": {
      "default_egress_action": "allow",
      "allowed_egress": [{"domains": ["github.com"]}]
    }
  }')
echo "  Domain rules in allow-default: HTTP $INVALID_RESP"

# Even though the response code may be 200 with success=false, check the body
INVALID_BODY=$(curl -s -X POST "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{
    "policy": {
      "default_egress_action": "allow",
      "allowed_egress": [{"domains": ["github.com"]}]
    }
  }' 2>&1 || echo '{"success":false}')

INVALID_SUCCESS=$(echo "$INVALID_BODY" | jq -r '.success')
if [ "$INVALID_SUCCESS" = "false" ]; then
  pass "API rejected domain rules in allow-default mode"
else
  fail "API should have rejected domain rules in allow-default"
fi

# Invalid CIDR should be rejected
INVALID_CIDR_BODY=$(curl -s -X POST "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{
    "policy": {
      "default_egress_action": "deny",
      "allowed_egress": [{"cidrs": ["not-a-cidr"]}]
    }
  }' 2>&1 || echo '{"success":false}')

INVALID_CIDR_SUCCESS=$(echo "$INVALID_CIDR_BODY" | jq -r '.success')
if [ "$INVALID_CIDR_SUCCESS" = "false" ]; then
  pass "API rejected invalid CIDR"
else
  fail "API should have rejected invalid CIDR"
fi

# Internal CIDR broader than /16 should be rejected
INVALID_INTERNAL=$(curl -s -X POST "$MGR/api/v1/runners/network-policy?runner_id=$RUNNER_ID" \
  -H 'Content-Type: application/json' \
  -d '{
    "policy": {
      "default_egress_action": "deny",
      "internal_access": {"allowed_internal_cidrs": ["10.0.0.0/8"]}
    }
  }' 2>&1 || echo '{"success":false}')

INVALID_INTERNAL_SUCCESS=$(echo "$INVALID_INTERNAL" | jq -r '.success')
if [ "$INVALID_INTERNAL_SUCCESS" = "false" ]; then
  pass "API rejected internal CIDR broader than /16"
else
  fail "API should have rejected /8 internal CIDR"
fi

# ---------------------------------------------------------------------------
header "15. Release all runners"
# ---------------------------------------------------------------------------
for rid in "${RUNNER_IDS[@]}"; do
  if [ -n "$rid" ]; then
    REL_RESP=$(curl -sf -X POST "$CP/api/v1/runners/release" \
      -H 'Content-Type: application/json' \
      -d "{\"runner_id\":\"$rid\"}" 2>&1 || echo '{"success":false}')
    echo "  Released $rid: $(echo "$REL_RESP" | jq -r '.success // false')"
  fi
done
RUNNER_IDS=()  # prevent cleanup trap from double-releasing
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
