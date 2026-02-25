#!/bin/bash
# Build a full Firecracker snapshot (kernel + rootfs + snapshot.mem + snapshot.state)
# for local dev testing. Runs the snapshot-builder, then copies local files to
# /tmp/fc-dev/snapshots/ where the manager expects them.
#
# Once a snapshot exists, the manager uses snapshot restore (fast) instead of cold boot.
#
# Usage: make dev-snapshot
#
# Environment variables:
#   GCS_BUCKET         - GCS bucket for chunked snapshot upload (default: none, local-only)
#   GCS_PREFIX         - GCS path prefix (default: v1)
#   ENABLE_CHUNKED     - Upload chunked snapshot to GCS (default: false; true requires GCS_BUCKET)
#   SNAPSHOT_COMMANDS  - JSON array of warmup commands (default: echo dev-snapshot-ready)
#
# Prerequisites:
#   - make dev-build   (builds all binaries + rootfs.img + kernel.bin)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
OUTPUT_DIR="/tmp/fc-dev/snapshot-build"
LOG_DIR="/tmp/fc-dev/logs"

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

if [ ! -f "$SNAPSHOT_DIR/rootfs.img" ]; then
  echo "FAIL: rootfs.img not found. Run 'make dev-build' first."
  exit 1
fi

if [ ! -e /dev/kvm ]; then
  echo "FAIL: /dev/kvm not found. KVM is required."
  exit 1
fi

# Clean previous build output (but keep snapshots dir)
rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR" "$LOG_DIR"

# Snapshot commands: for dev, a minimal warmup. Override with SNAPSHOT_COMMANDS env var.
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}

# GCS config: when GCS_BUCKET is set, enable chunked upload so the golden snapshot
# is available in GCS for cross-host session resume testing.
GCS_BUCKET=${GCS_BUCKET:-local-dev-unused}
GCS_PREFIX=${GCS_PREFIX:-v1}
ENABLE_CHUNKED=${ENABLE_CHUNKED:-false}

echo "Snapshot commands: $SNAPSHOT_COMMANDS"
echo "GCS bucket: $GCS_BUCKET"
echo "Enable chunked: $ENABLE_CHUNKED"
echo "Output dir: $OUTPUT_DIR"
echo ""

# Run snapshot-builder (needs root for networking + TAP devices).
echo "--- Running snapshot-builder ---"
set +e
sudo "$REPO_ROOT/bin/snapshot-builder" \
  --gcs-bucket="$GCS_BUCKET" \
  --gcs-prefix="$GCS_PREFIX" \
  --output-dir="$OUTPUT_DIR" \
  --kernel-path="$SNAPSHOT_DIR/kernel.bin" \
  --rootfs-path="$SNAPSHOT_DIR/rootfs.img" \
  --firecracker-bin=/usr/local/bin/firecracker \
  --vcpus=2 \
  --memory-mb=2048 \
  --warmup-timeout=5m \
  --repo-cache-seed-size-gb=1 \
  --repo-cache-upper-size-gb=1 \
  --enable-chunked="$ENABLE_CHUNKED" \
  --snapshot-commands="$SNAPSHOT_COMMANDS" \
  --log-level=info \
  2>&1 | tee "$LOG_DIR/snapshot-builder.log"
BUILD_EXIT=$?
set -e

if [ $BUILD_EXIT -ne 0 ]; then
  echo ""
  echo "--- Snapshot builder exited with code $BUILD_EXIT ---"
  echo "    (Expected: GCS upload fails without credentials, but local files should exist)"
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
for f in kernel.bin rootfs.img snapshot.mem snapshot.state repo-cache-seed.img repo-cache-upper.img credentials.img git-cache.img; do
  if [ -f "$OUTPUT_DIR/$f" ]; then
    cp "$OUTPUT_DIR/$f" "$SNAPSHOT_DIR/$f"
    echo "  $f ($(du -sh "$OUTPUT_DIR/$f" | cut -f1))"
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
  "state_path": "snapshot.state",
  "repo_cache_seed_path": "repo-cache-seed.img"
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
