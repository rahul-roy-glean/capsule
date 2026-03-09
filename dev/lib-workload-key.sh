#!/bin/bash

discover_workload_key() {
  if [ -n "${WORKLOAD_KEY:-}" ] && [ "${WORKLOAD_KEY}" != "null" ]; then
    echo "$WORKLOAD_KEY"
    return 0
  fi

  local candidate=""
  for log_file in /tmp/fc-dev/logs/snapshot-builder.log /tmp/fc-dev/logs/agent-snapshot-builder.log; do
    if [ -f "$log_file" ]; then
      candidate=$(grep -o '"workload_key":"[^"]*"' "$log_file" | tail -1 | cut -d'"' -f4)
      if [ -n "$candidate" ]; then
        echo "$candidate"
        return 0
      fi
    fi
  done

  return 1
}
