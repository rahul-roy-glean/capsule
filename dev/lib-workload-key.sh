#!/bin/bash
# Shared helpers for discovering the workload key and snapshot commands
# from the most recent dev snapshot build.
#
# Usage: source dev/lib-workload-key.sh

DEV_SNAPSHOT_INFO="/tmp/fc-dev/snapshot-info.json"
DEV_CP="http://localhost:8080"

# discover_workload_key prints the leaf workload key from the last snapshot build.
# This is the key that aligns with both the GCS snapshot and the layered-config system.
# Requires snapshot-info.json written by build-snapshot.sh / provision-agent-snapshot.sh.
discover_workload_key() {
  if [ -n "${WORKLOAD_KEY:-}" ] && [ "${WORKLOAD_KEY}" != "null" ]; then
    echo "$WORKLOAD_KEY"
    return 0
  fi

  if [ ! -f "$DEV_SNAPSHOT_INFO" ]; then
    return 1
  fi

  local leaf_key
  leaf_key=$(jq -r '.leaf_workload_key // empty' "$DEV_SNAPSHOT_INFO" 2>/dev/null)
  if [ -n "$leaf_key" ]; then
    echo "$leaf_key"
    return 0
  fi

  return 1
}

# discover_snapshot_commands prints the snapshot commands JSON from the last build.
discover_snapshot_commands() {
  if [ -f "$DEV_SNAPSHOT_INFO" ]; then
    jq -c '.snapshot_commands // empty' "$DEV_SNAPSHOT_INFO" 2>/dev/null
    return $?
  fi
  return 1
}

# require_workload_key discovers the leaf workload key or exits with an error.
# Sets WORKLOAD_KEY as a side-effect.
require_workload_key() {
  WORKLOAD_KEY=$(discover_workload_key) || {
    echo "FAIL: could not discover workload key from $DEV_SNAPSHOT_INFO."
    echo "Run 'GCS_BUCKET=<bucket> make dev-snapshot' first."
    exit 1
  }
  echo "Using workload key: $WORKLOAD_KEY"
}

# register_dev_config registers a layered config with the control plane using
# the snapshot commands from the last build. Ensures exactly one config exists
# per leaf_workload_key by deleting any prior configs for the same key first.
# Asserts the returned leaf_workload_key matches WORKLOAD_KEY.
#
# Args: $1=display_name $2=config_json (e.g. '{"ttl":60,"auto_pause":false}')
# Sets CONFIG_RESP as a side-effect.
register_dev_config() {
  local display_name="${1:-dev-test}"
  local config_json="${2:-{}}"

  if [ -z "${WORKLOAD_KEY:-}" ]; then
    echo "FAIL: register_dev_config called before require_workload_key"
    exit 1
  fi

  local commands
  commands=$(discover_snapshot_commands) || {
    echo "FAIL: could not discover snapshot commands from $DEV_SNAPSHOT_INFO."
    echo "Run 'GCS_BUCKET=<bucket> make dev-snapshot' first."
    exit 1
  }

  CONFIG_RESP=$(curl -sf -X POST "$DEV_CP/api/v1/layered-configs" \
    -H 'Content-Type: application/json' \
    -d '{
      "display_name": "'"$display_name"'",
      "layers": [{"name":"base","init_commands":'"$commands"'}],
      "config": '"$config_json"'
    }')

  local leaf_key
  leaf_key=$(echo "$CONFIG_RESP" | jq -r '.leaf_workload_key')
  if [ -z "$leaf_key" ] || [ "$leaf_key" = "null" ]; then
    echo "FAIL: could not register layered config: $CONFIG_RESP"
    exit 1
  fi

  if [ "$leaf_key" != "$WORKLOAD_KEY" ]; then
    echo "FAIL: leaf_workload_key mismatch: config returned '$leaf_key' but WORKLOAD_KEY='$WORKLOAD_KEY'"
    echo "This means snapshot-info.json is stale. Re-run 'GCS_BUCKET=<bucket> make dev-snapshot'."
    exit 1
  fi

  echo "  Registered config: leaf_workload_key=$leaf_key"
}
