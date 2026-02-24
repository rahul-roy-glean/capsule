#!/bin/bash
# Start the control-plane + firecracker-manager stack.
# Run inside the Lima VM: lima bash dev/run-stack.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_DIR="/tmp/fc-dev/pids"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
LOG_DIR="/tmp/fc-dev/logs"

cd "$REPO_ROOT"

mkdir -p "$PID_DIR" "$LOG_DIR"

# --- Prerequisites ---
echo "=== Checking prerequisites ==="

if [ ! -e /dev/kvm ]; then
  echo "FAIL: /dev/kvm not found. KVM is required."
  exit 1
fi

if ! systemctl is-active --quiet postgresql; then
  echo "FAIL: PostgreSQL is not running. Start the Lima VM with 'make dev-up'."
  exit 1
fi

if [ ! -f "$SNAPSHOT_DIR/kernel.bin" ]; then
  echo "FAIL: kernel.bin not found. Run 'make dev-build' first."
  exit 1
fi

if [ ! -f "$REPO_ROOT/bin/control-plane" ]; then
  echo "FAIL: bin/control-plane not found. Run 'lima make build' first."
  exit 1
fi

if [ ! -f "$REPO_ROOT/bin/firecracker-manager" ]; then
  echo "FAIL: bin/firecracker-manager not found. Run 'lima make build' first."
  exit 1
fi

# --- Stop existing processes if running ---
if [ -f "$PID_DIR/control-plane.pid" ] || [ -f "$PID_DIR/firecracker-manager.pid" ]; then
  echo "Stopping existing stack..."
  bash "$(dirname "$0")/stop-stack.sh" 2>/dev/null || true
fi

# --- Start control-plane ---
echo ""
echo "=== Starting control-plane ==="
nohup ./bin/control-plane \
  --db-host=localhost \
  --db-user=postgres \
  --db-password="" \
  --db-name=firecracker_runner \
  --db-ssl-mode=disable \
  --http-port=8080 \
  --grpc-port=50051 \
  --telemetry-enabled=false \
  > "$LOG_DIR/control-plane.log" 2>&1 &
CP_PID=$!
echo "$CP_PID" > "$PID_DIR/control-plane.pid"
echo "control-plane PID: $CP_PID (log: $LOG_DIR/control-plane.log)"

# Wait for control-plane health
echo -n "Waiting for control-plane..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:8080/health > /dev/null 2>&1; then
    echo " ready (${i}s)"
    break
  fi
  if [ "$i" = "30" ]; then
    echo " FAIL: control-plane did not become healthy in 30s"
    echo "Last log lines:"
    tail -20 "$LOG_DIR/control-plane.log"
    exit 1
  fi
  echo -n "."
  sleep 1
done

# --- Start firecracker-manager ---
echo ""
echo "=== Starting firecracker-manager ==="
sudo -b sh -c 'nohup '"$REPO_ROOT"'/bin/firecracker-manager \
  --http-port=9080 \
  --grpc-port=50052 \
  --use-netns \
  --ci-system=none \
  --snapshot-bucket=local-dev \
  --snapshot-cache='"$SNAPSHOT_DIR"' \
  --socket-dir=/tmp/fc-dev/sockets \
  --workspace-dir=/tmp/fc-dev/workspaces \
  --log-dir='"$LOG_DIR"' \
  --control-plane=http://localhost:8080 \
  --telemetry-enabled=false \
  --max-runners=4 \
  --idle-target=0 \
  --log-level=debug \
  > '"$LOG_DIR"'/firecracker-manager.log 2>&1 &
echo $! > /tmp/fc-dev/pids/firecracker-manager.pid'
# Give sudo a moment to fork
sleep 1
MGR_PID=$(cat "$PID_DIR/firecracker-manager.pid" 2>/dev/null || echo "unknown")
echo "firecracker-manager PID: $MGR_PID (log: $LOG_DIR/firecracker-manager.log)"

# Wait for firecracker-manager health
echo -n "Waiting for firecracker-manager..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:9080/health > /dev/null 2>&1; then
    echo " ready (${i}s)"
    break
  fi
  if [ "$i" = "30" ]; then
    echo " FAIL: firecracker-manager did not become healthy in 30s"
    echo "Last log lines:"
    tail -20 "$LOG_DIR/firecracker-manager.log"
    exit 1
  fi
  echo -n "."
  sleep 1
done

echo ""
echo "=== Stack ready ==="
echo "  Control Plane:       http://localhost:8080"
echo "  Firecracker Manager: http://localhost:9080"
echo "  Logs:                $LOG_DIR/"
echo ""
echo "Run 'make dev-test-exec' to test, 'make dev-stop' to stop."
