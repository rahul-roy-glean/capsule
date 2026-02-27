#!/bin/bash
# Stop the dev stack: kill processes, clean up VMs, sockets, and network.
# Usage: make dev-stop
set -euo pipefail

PID_DIR="/tmp/fc-dev/pids"

kill_pid_file() {
  local name="$1"
  local pidfile="$PID_DIR/$name.pid"
  if [ -f "$pidfile" ]; then
    local pid
    pid=$(cat "$pidfile")
    if sudo kill -0 "$pid" 2>/dev/null; then
      echo "Stopping $name (PID $pid)..."
      sudo kill "$pid" 2>/dev/null || true
      # Wait up to 5s for graceful shutdown
      for i in $(seq 1 10); do
        sudo kill -0 "$pid" 2>/dev/null || break
        sleep 0.5
      done
      # Force kill if still alive
      if sudo kill -0 "$pid" 2>/dev/null; then
        echo "  Force-killing $name..."
        sudo kill -9 "$pid" 2>/dev/null || true
      fi
    fi
    rm -f "$pidfile"
  fi
}

echo "=== Stopping dev stack ==="

# 1. Stop firecracker-manager first (triggers graceful VM shutdown)
kill_pid_file "firecracker-manager"

# 2. Stop control-plane
kill_pid_file "control-plane"

# 3. Kill any remaining firecracker processes
if pgrep -f "firecracker --id" > /dev/null 2>&1; then
  echo "Killing remaining firecracker VM processes..."
  sudo pkill -f "firecracker --id" || true
  sleep 1
  sudo pkill -9 -f "firecracker --id" 2>/dev/null || true
fi

# 4. Clean up TAP devices (snapshot-builder and manager create tap-slot-*)
for tap in $(ip -o link show 2>/dev/null | grep -oP 'tap-slot-\d+' || true); do
  echo "Removing TAP device: $tap"
  sudo ip link delete "$tap" 2>/dev/null || true
done

# 5. Clean up network namespaces (fc-dev related)
for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep -E '^fc-' || true); do
  echo "Removing network namespace: $ns"
  sudo ip netns delete "$ns" 2>/dev/null || true
done

# 6. Clean up Firecracker sockets
if ls /tmp/fc-dev/sockets/*.sock 2>/dev/null; then
  echo "Removing Firecracker sockets..."
  sudo rm -f /tmp/fc-dev/sockets/*.sock
fi

# 7. Remove remaining PID files
rm -f "$PID_DIR"/*.pid

echo ""
echo "=== Stack stopped ==="
echo "Logs preserved in /tmp/fc-dev/logs/"
echo "Snapshots preserved in /tmp/fc-dev/snapshots/"
