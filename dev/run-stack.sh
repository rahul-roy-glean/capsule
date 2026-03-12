#!/bin/bash
# Start the control-plane + capsule-manager stack.
# Usage: make dev-stack
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
  echo "FAIL: PostgreSQL is not running. Run 'sudo systemctl start postgresql'."
  exit 1
fi

if [ ! -f "$SNAPSHOT_DIR/kernel.bin" ]; then
  echo "FAIL: kernel.bin not found. Run 'make dev-build' first."
  exit 1
fi

if [ ! -f "$REPO_ROOT/bin/capsule-control-plane" ]; then
  echo "FAIL: bin/capsule-control-plane not found. Run 'make dev-build' first."
  exit 1
fi

if [ ! -f "$REPO_ROOT/bin/capsule-manager" ]; then
  echo "FAIL: bin/capsule-manager not found. Run 'make dev-build' first."
  exit 1
fi

# --- Stop existing processes if running ---
if [ -f "$PID_DIR/control-plane.pid" ] || [ -f "$PID_DIR/capsule-manager.pid" ]; then
  echo "Stopping existing stack..."
  bash "$(dirname "$0")/stop-stack.sh" 2>/dev/null || true
fi

# --- Reset stale host status ---
# After a manager crash or unclean shutdown the control-plane may have hosts
# stuck as 'unhealthy' or 'draining'. Reset so the restarted manager can
# re-register as 'ready'.
sudo -u postgres psql -d capsule -c \
  "UPDATE hosts SET status = 'ready' WHERE status IN ('unhealthy', 'draining');" \
  > /dev/null 2>&1 || true

# --- OpenTelemetry ---
# Set OTEL_EXPORTER_OTLP_ENDPOINT to enable tracing/metrics.
# When empty, OTel is no-op (zero overhead).
# To test locally: run the collector first, then export the endpoint:
#   docker run --rm -p 4317:4317 \
#     -v $(pwd)/deploy/otel-collector:/etc/otelcol-contrib \
#     otel/opentelemetry-collector-contrib:latest \
#     --config /etc/otelcol-contrib/config-local.yaml
#   export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317
OTEL_ENDPOINT="${OTEL_EXPORTER_OTLP_ENDPOINT:-}"

# --- Start control-plane ---
echo ""
echo "=== Starting control-plane ==="
OTEL_EXPORTER_OTLP_ENDPOINT="$OTEL_ENDPOINT" \
ENVIRONMENT=dev \
nohup ./bin/capsule-control-plane \
  --db-host=localhost \
  --db-user=postgres \
  --db-password="${DB_PASSWORD:-postgres}" \
  --db-name=capsule \
  --db-ssl-mode=disable \
  --http-port=8080 \
  --grpc-port=50051 \
  > "$LOG_DIR/capsule-control-plane.log" 2>&1 &
CP_PID=$!
echo "$CP_PID" > "$PID_DIR/control-plane.pid"
echo "control-plane PID: $CP_PID (log: $LOG_DIR/capsule-control-plane.log)"

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
    tail -20 "$LOG_DIR/capsule-control-plane.log"
    exit 1
  fi
  echo -n "."
  sleep 1
done

# --- Start capsule-manager ---
echo ""
echo "=== Starting capsule-manager ==="

# In local dev /tmp/fc-dev is not a real mount point; bind-mount it so the
# manager's data-mount readiness check passes.
if ! grep -q "/tmp/fc-dev" /proc/mounts 2>/dev/null; then
  sudo mount --bind /tmp/fc-dev /tmp/fc-dev
fi

# Build the full command as an array, then pass to sudo.
GCS_BUCKET=${GCS_BUCKET:-${SESSION_CHUNK_BUCKET:-}}

MGR_CMD="$REPO_ROOT/bin/capsule-manager \
  --http-port=9080 \
  --grpc-port=50052 \
  --snapshot-cache=$SNAPSHOT_DIR \
  --socket-dir=/tmp/fc-dev/sockets \
  --workspace-dir=/tmp/fc-dev/workspaces \
  --log-dir=$LOG_DIR \
  --control-plane=http://localhost:8080 \
  --max-runners=8 \
  --idle-target=0 \
  --log-level=debug"

if [ -n "$GCS_BUCKET" ]; then
  MGR_CMD="$MGR_CMD --snapshot-bucket=$GCS_BUCKET"
  echo "  GCS session chunks: enabled (bucket: $GCS_BUCKET)"
else
  MGR_CMD="$MGR_CMD --snapshot-bucket=local-dev"
fi

sudo bash -c "export OTEL_EXPORTER_OTLP_ENDPOINT='$OTEL_ENDPOINT' ENVIRONMENT=dev; nohup $MGR_CMD > $LOG_DIR/capsule-manager.log 2>&1 &
echo \$! > /tmp/fc-dev/pids/capsule-manager.pid"
# Give sudo a moment to fork
sleep 1
MGR_PID=$(cat "$PID_DIR/capsule-manager.pid" 2>/dev/null || echo "unknown")
echo "capsule-manager PID: $MGR_PID (log: $LOG_DIR/capsule-manager.log)"

# Wait for capsule-manager health
echo -n "Waiting for capsule-manager..."
for i in $(seq 1 30); do
  if curl -sf http://localhost:9080/health > /dev/null 2>&1; then
    echo " ready (${i}s)"
    break
  fi
  if [ "$i" = "30" ]; then
    echo " FAIL: capsule-manager did not become healthy in 30s"
    echo "Last log lines:"
    tail -20 "$LOG_DIR/capsule-manager.log"
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
echo "Run 'make dev-test-pause-resume' to test sessions, 'make dev-stop' to stop."
