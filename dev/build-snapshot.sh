#!/bin/bash
# Build a full Firecracker snapshot (kernel + rootfs + snapshot.mem + snapshot.state)
# for local dev testing. Runs the snapshot-builder, then copies local files to
# /tmp/fc-dev/snapshots/ where the manager expects them.
#
# Once a snapshot exists, the manager uses snapshot restore (fast) instead of cold boot.
#
# Usage: GCS_BUCKET=<bucket> make dev-snapshot
#
# Environment variables:
#   GCS_BUCKET         - GCS bucket for chunked snapshot upload (required)
#   GCS_PREFIX         - GCS path prefix (default: v1)
#   SNAPSHOT_COMMANDS  - JSON array of warmup commands (default: echo dev-snapshot-ready)
#
# Prerequisites:
#   - make dev-build   (builds all binaries + rootfs.img + kernel.bin)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/tmp/fc-dev/snapshots}"
OUTPUT_DIR="${OUTPUT_DIR:-/tmp/fc-dev/snapshot-build}"
LOG_DIR="${LOG_DIR:-/tmp/fc-dev/logs}"

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:$PATH"

echo "=== Building Firecracker Snapshot ==="

# --- Prerequisites ---
if [ ! -f "$REPO_ROOT/bin/snapshot-builder" ]; then
  echo "FAIL: bin/snapshot-builder not found. Run 'make dev-build' first."
  exit 1
fi

if [ ! -f "$SNAPSHOT_DIR/kernel.bin" ]; then
  echo "FAIL: kernel.bin not found. Run 'make dev-build' first."
  exit 1
fi

if [ ! -e /dev/kvm ]; then
  echo "FAIL: /dev/kvm not found. KVM is required."
  exit 1
fi

BASE_IMAGE=${BASE_IMAGE:-}
RUNNER_USER=${RUNNER_USER:-}
ROOTFS_SIZE_GB=${ROOTFS_SIZE_GB:-}
THAW_AGENT_PATH=${THAW_AGENT_PATH:-}
EXTRA_SNAPSHOT_BUILDER_FLAGS=${EXTRA_SNAPSHOT_BUILDER_FLAGS:-}

if [ -z "$BASE_IMAGE" ] && [ ! -f "$SNAPSHOT_DIR/rootfs.img" ]; then
  echo "FAIL: rootfs.img not found. Run 'make dev-build' first, or set BASE_IMAGE=<image>."
  exit 1
fi

# Clean previous build output (but keep snapshots dir)
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR" "$LOG_DIR"

# Snapshot commands: for dev, a minimal warmup. Override with SNAPSHOT_COMMANDS env var.
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}

# GCS config
GCS_BUCKET=${GCS_BUCKET:-}
GCS_PREFIX=${GCS_PREFIX:-v1}

if [ -z "$GCS_BUCKET" ]; then
  echo "FAIL: GCS_BUCKET is required. Set GCS_BUCKET=<bucket> to upload chunked snapshots."
  exit 1
fi

echo "Snapshot commands: $SNAPSHOT_COMMANDS"
echo "GCS bucket: $GCS_BUCKET"
if [ -n "$BASE_IMAGE" ]; then
  echo "Base image: $BASE_IMAGE"
else
  echo "Rootfs path: $SNAPSHOT_DIR/rootfs.img"
fi
echo "Output dir: $OUTPUT_DIR"
echo ""

# Run snapshot-builder (needs root for networking + TAP devices).
echo "--- Running snapshot-builder ---"
BUILDER_FLAGS=(
  --gcs-bucket="$GCS_BUCKET"
  --gcs-prefix="$GCS_PREFIX"
  --output-dir="$OUTPUT_DIR"
  --kernel-path="$SNAPSHOT_DIR/kernel.bin"
  --firecracker-bin=/usr/local/bin/firecracker
  --vcpus=2
  --memory-mb=2048
  --warmup-timeout=5m
  --snapshot-commands="$SNAPSHOT_COMMANDS"
  --log-level=info
)

if [ -n "$BASE_IMAGE" ]; then
  BUILDER_FLAGS+=(--base-image="$BASE_IMAGE")
else
  BUILDER_FLAGS+=(--rootfs-path="$SNAPSHOT_DIR/rootfs.img")
fi

if [ -n "$RUNNER_USER" ]; then
  BUILDER_FLAGS+=(--runner-user="$RUNNER_USER")
fi

if [ -n "$ROOTFS_SIZE_GB" ]; then
  BUILDER_FLAGS+=(--rootfs-size-gb="$ROOTFS_SIZE_GB")
fi

if [ -n "$THAW_AGENT_PATH" ]; then
  BUILDER_FLAGS+=(--thaw-agent-path="$THAW_AGENT_PATH")
fi

set +e
sudo "$REPO_ROOT/bin/snapshot-builder" \
  "${BUILDER_FLAGS[@]}" \
  ${EXTRA_SNAPSHOT_BUILDER_FLAGS:+$EXTRA_SNAPSHOT_BUILDER_FLAGS} \
  2>&1 | tee "$LOG_DIR/snapshot-builder.log"
BUILD_EXIT=$?
set -e

if [ $BUILD_EXIT -ne 0 ]; then
  echo ""
  echo "--- Snapshot builder exited with code $BUILD_EXIT ---"
  echo "    Check GCS credentials and bucket configuration."
fi

# Check if snapshot files were created (they're produced before the GCS upload step)
if [ ! -f "$OUTPUT_DIR/snapshot.mem" ] || [ ! -f "$OUTPUT_DIR/snapshot.state" ]; then
  echo ""
  echo "FAIL: snapshot.mem or snapshot.state not found in $OUTPUT_DIR"
  echo "The snapshot-builder failed before creating the snapshot (not a GCS issue)."
  echo "Check $LOG_DIR/snapshot-builder.log for details."
  exit 1
fi

echo ""
echo "--- Snapshot files created successfully ---"

# Copy snapshot files to the manager's snapshot cache directory
echo ""
echo "--- Copying snapshot files to $SNAPSHOT_DIR ---"
for f in "$OUTPUT_DIR"/*.img "$OUTPUT_DIR"/snapshot.mem "$OUTPUT_DIR"/snapshot.state "$OUTPUT_DIR"/kernel.bin; do
  if [ -f "$f" ]; then
    fname=$(basename "$f")
    cp "$f" "$SNAPSHOT_DIR/$fname"
    echo "  $fname ($(du -sh "$f" | cut -f1))"
  fi
done

# Write metadata.json so the snapshot cache can load version info.
# The manager reads this at startup and GetSnapshotPaths() uses it.
cat > "$SNAPSHOT_DIR/metadata.json" << METADATA
{
  "version": "dev-snapshot",
  "created_at": "$(date -Iseconds)",
  "kernel_path": "kernel.bin",
  "rootfs_path": "rootfs.img",
  "mem_path": "snapshot.mem",
  "state_path": "snapshot.state"
}
METADATA

echo ""
echo "=== Snapshot build complete ==="
echo ""
echo "Files in $SNAPSHOT_DIR:"
ls -lh "$SNAPSHOT_DIR/"
echo ""
echo "The manager will now use snapshot restore (fast path) instead of cold boot."
echo "Run 'make dev-stop && make dev-stack' to restart with the new snapshot."
