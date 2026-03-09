#!/bin/bash
# Provision an AI agent sandbox snapshot for local development.
#
# This builds the agent rootfs (if needed), then uses snapshot-builder to create
# a golden snapshot with repos cloned and dependencies installed.
#
# Uploads a chunked golden snapshot to GCS so the manager can lazy-load via
# UFFD+FUSE, and session pause/resume uses GCS-backed chunks.
#
# Usage:
#   GCS_BUCKET=my-bucket make dev-agent-snapshot
#
# Prerequisites:
#   - make dev-build (builds snapshot-builder binary)
#   - Docker available (for rootfs build)
#   - /dev/kvm available
#   - gcloud auth
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
OUTPUT_DIR="/tmp/fc-dev/snapshot-build"
LOG_DIR="/tmp/fc-dev/logs"

# GCS config (required)
GCS_BUCKET=${GCS_BUCKET:-}
GCS_PREFIX=${GCS_PREFIX:-v1}

if [ -z "$GCS_BUCKET" ]; then
  echo "FAIL: GCS_BUCKET is required. Set GCS_BUCKET=<bucket> to upload chunked snapshots."
  exit 1
fi

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:$PATH"

echo "=== Provisioning AI Agent Sandbox Snapshot ==="
echo "  GCS mode: bucket=$GCS_BUCKET prefix=$GCS_PREFIX (chunked + UFFD/FUSE)"

# --- 1. Build agent rootfs if not present ---
AGENT_MARKER="$SNAPSHOT_DIR/.agent-rootfs-built"
if [ ! -f "$SNAPSHOT_DIR/rootfs.img" ] || [ ! -f "$AGENT_MARKER" ]; then
  echo ""
  echo "--- Building agent rootfs (Python, Node.js, Claude Code) ---"
  bash dev/build-agent-rootfs.sh
  touch "$AGENT_MARKER"
else
  echo "Agent rootfs already built, skipping (remove $AGENT_MARKER to force rebuild)"
fi

# --- 2. Verify prerequisites ---
if [ ! -f "$REPO_ROOT/bin/snapshot-builder" ]; then
  echo "FAIL: bin/snapshot-builder not found. Run 'make build' first."
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

# --- 3. Build golden snapshot with repos ---
echo ""
echo "--- Building golden snapshot with repos baked in ---"

SNAPSHOT_COMMANDS='[
  {"type":"shell","args":["bash","-c","rm -rf /workspace/markupsafe && git clone --depth=1 --branch main https://github.com/pallets/markupsafe /workspace/markupsafe"]},
  {"type":"shell","args":["bash","-c","rm -rf /workspace/camelcase && git clone --depth=1 --branch main https://github.com/sindresorhus/camelcase /workspace/camelcase"]},
  {"type":"shell","args":["pip3","install","--break-system-packages","markupsafe"],"run_as_root":true}
]'

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR" "$LOG_DIR"

echo "Snapshot commands: $SNAPSHOT_COMMANDS"
echo "Output dir: $OUTPUT_DIR"
echo ""

BUILDER_FLAGS=(
  --gcs-prefix="$GCS_PREFIX"
  --output-dir="$OUTPUT_DIR"
  --kernel-path="$SNAPSHOT_DIR/kernel.bin"
  --rootfs-path="$SNAPSHOT_DIR/rootfs.img"
  --firecracker-bin=/usr/local/bin/firecracker
  --vcpus=2
  --memory-mb=4096
  --warmup-timeout=5m
  --snapshot-commands="$SNAPSHOT_COMMANDS"
  --log-level=info
)

BUILDER_FLAGS+=(--gcs-bucket="$GCS_BUCKET")

set +e
sudo "$REPO_ROOT/bin/snapshot-builder" \
  "${BUILDER_FLAGS[@]}" \
  2>&1 | tee "$LOG_DIR/agent-snapshot-builder.log"
BUILD_EXIT=$?
set -e

if [ $BUILD_EXIT -ne 0 ]; then
  echo ""
  echo "--- Snapshot builder exited with code $BUILD_EXIT ---"
  echo "    Check GCS credentials and bucket configuration."
fi

# --- 4. Verify snapshot files ---
if [ ! -f "$OUTPUT_DIR/snapshot.mem" ] || [ ! -f "$OUTPUT_DIR/snapshot.state" ]; then
  echo ""
  echo "FAIL: snapshot.mem or snapshot.state not found in $OUTPUT_DIR"
  echo "The snapshot-builder failed before creating the snapshot."
  echo "Check $LOG_DIR/agent-snapshot-builder.log for details."
  exit 1
fi

echo ""
echo "--- Snapshot files created successfully ---"

# --- 5. Copy snapshot files to manager's snapshot cache ---
echo ""
echo "--- Copying snapshot files to $SNAPSHOT_DIR ---"
for f in "$OUTPUT_DIR"/*.img "$OUTPUT_DIR"/snapshot.mem "$OUTPUT_DIR"/snapshot.state "$OUTPUT_DIR"/kernel.bin; do
  if [ -f "$f" ]; then
    fname=$(basename "$f")
    cp "$f" "$SNAPSHOT_DIR/$fname"
    echo "  $fname ($(du -sh "$f" | cut -f1))"
  fi
done

cat > "$SNAPSHOT_DIR/metadata.json" << METADATA
{
  "version": "agent-sandbox-snapshot",
  "created_at": "$(date -Iseconds)",
  "kernel_path": "kernel.bin",
  "rootfs_path": "rootfs.img",
  "mem_path": "snapshot.mem",
  "state_path": "snapshot.state"
}
METADATA

echo ""
echo "=== Agent sandbox snapshot provisioned ==="
echo ""
echo "Files in $SNAPSHOT_DIR:"
ls -lh "$SNAPSHOT_DIR/"
echo ""
echo "GCS chunked snapshot uploaded to gs://$GCS_BUCKET/$GCS_PREFIX/"
echo "Run: GCS_BUCKET=$GCS_BUCKET make dev-stop dev-stack dev-test-agent-sessions"
