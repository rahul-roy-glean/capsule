#!/bin/bash
# E2E test: snapshot template tagging (WS6)
# Usage: make dev-test-template-tags (or run directly)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$REPO_ROOT/dev/lib-workload-key.sh"

CP=http://localhost:8080

echo "=== E2E Template Tags Test ==="
echo ""

# --- 0. Register snapshot config ---
echo "=== 0. Register snapshot config ==="
require_workload_key
register_dev_config "tag-test" '{"ttl": 60, "auto_pause": false}'

# --- 1. Create tags ---
echo ""
echo "=== 1. Create tag 'stable' ==="
TAG_RESP=$(curl -sf -X POST "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags" \
  -H 'Content-Type: application/json' \
  -d '{"tag":"stable","version":"v1.0.0","description":"initial stable release"}')
echo "Response: $TAG_RESP"
TAG_VERSION=$(echo "$TAG_RESP" | jq -r '.version')
if [ "$TAG_VERSION" != "v1.0.0" ]; then
  echo "FAIL: expected version v1.0.0, got $TAG_VERSION"
  exit 1
fi
echo "OK: stable tag created with version v1.0.0"

echo ""
echo "=== 1b. Create tag 'canary' ==="
TAG_RESP2=$(curl -sf -X POST "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags" \
  -H 'Content-Type: application/json' \
  -d '{"tag":"canary","version":"v2.0.0-rc1","description":"canary release candidate"}')
echo "Response: $TAG_RESP2"
CANARY_VERSION=$(echo "$TAG_RESP2" | jq -r '.version')
if [ "$CANARY_VERSION" != "v2.0.0-rc1" ]; then
  echo "FAIL: expected version v2.0.0-rc1, got $CANARY_VERSION"
  exit 1
fi
echo "OK: canary tag created with version v2.0.0-rc1"

# --- 2. List tags ---
echo ""
echo "=== 2. List tags ==="
LIST_RESP=$(curl -sf "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags")
echo "Response: $LIST_RESP"
TAG_COUNT=$(echo "$LIST_RESP" | jq -r '.count')
if [ "$TAG_COUNT" != "2" ]; then
  echo "FAIL: expected 2 tags, got $TAG_COUNT"
  exit 1
fi
echo "OK: 2 tags listed"

# --- 3. Get specific tag ---
echo ""
echo "=== 3. Get tag 'stable' ==="
GET_RESP=$(curl -sf "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags/stable")
echo "Response: $GET_RESP"
GOT_VERSION=$(echo "$GET_RESP" | jq -r '.version')
if [ "$GOT_VERSION" != "v1.0.0" ]; then
  echo "FAIL: expected version v1.0.0, got $GOT_VERSION"
  exit 1
fi
echo "OK: got stable tag with version v1.0.0"

# --- 4. Update tag ---
echo ""
echo "=== 4. Update tag 'stable' to v1.1.0 ==="
UPDATE_RESP=$(curl -sf -X POST "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags" \
  -H 'Content-Type: application/json' \
  -d '{"tag":"stable","version":"v1.1.0","description":"updated stable release"}')
echo "Response: $UPDATE_RESP"
UPDATED_VERSION=$(echo "$UPDATE_RESP" | jq -r '.version')
if [ "$UPDATED_VERSION" != "v1.1.0" ]; then
  echo "FAIL: expected version v1.1.0, got $UPDATED_VERSION"
  exit 1
fi
echo "OK: stable tag updated to v1.1.0"

# --- 5. Promote tag ---
echo ""
echo "=== 5. Promote tag 'stable' to current_version ==="
PROMOTE_RESP=$(curl -sf -X POST "$CP/api/v1/layered-configs/$WORKLOAD_KEY/promote" \
  -H 'Content-Type: application/json' \
  -d '{"tag":"stable"}')
echo "Response: $PROMOTE_RESP"
PROMOTED_VERSION=$(echo "$PROMOTE_RESP" | jq -r '.version')
if [ "$PROMOTED_VERSION" != "v1.1.0" ]; then
  echo "FAIL: expected promoted version v1.1.0, got $PROMOTED_VERSION"
  exit 1
fi
echo "OK: promoted stable to current_version=v1.1.0"

# Verify current_version was updated
CONFIG_RESP2=$(curl -sf "$CP/api/v1/layered-configs/$WORKLOAD_KEY")
CURRENT_VERSION=$(echo "$CONFIG_RESP2" | jq -r '.current_version')
if [ "$CURRENT_VERSION" != "v1.1.0" ]; then
  echo "FAIL: expected current_version=v1.1.0, got $CURRENT_VERSION"
  exit 1
fi
echo "OK: current_version is now v1.1.0"

# --- 6. Delete tag ---
echo ""
echo "=== 6. Delete tag 'canary' ==="
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE \
  "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags/canary")
if [ "$HTTP_CODE" != "204" ]; then
  echo "FAIL: expected 204, got $HTTP_CODE"
  exit 1
fi
echo "OK: canary tag deleted (204)"

# Verify deletion
HTTP_CODE2=$(curl -s -o /dev/null -w "%{http_code}" \
  "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags/canary")
if [ "$HTTP_CODE2" != "404" ]; then
  echo "FAIL: expected 404 for deleted tag, got $HTTP_CODE2"
  exit 1
fi
echo "OK: canary tag confirmed deleted (404)"

# --- 7. Verify only 1 tag remains ---
echo ""
echo "=== 7. Verify remaining tags ==="
FINAL_LIST=$(curl -sf "$CP/api/v1/layered-configs/$WORKLOAD_KEY/tags")
FINAL_COUNT=$(echo "$FINAL_LIST" | jq -r '.count')
if [ "$FINAL_COUNT" != "1" ]; then
  echo "FAIL: expected 1 tag remaining, got $FINAL_COUNT"
  exit 1
fi
echo "OK: 1 tag remaining"

echo ""
echo "========================================="
echo "=== ALL TEMPLATE TAG TESTS PASSED ==="
echo "========================================="
