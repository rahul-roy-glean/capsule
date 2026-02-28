#!/bin/bash
# Phase 1: Onboard repos + build golden snapshot for AI agent sandbox testing.
#
# This builds the agent rootfs (if needed), then uses snapshot-builder to create
# a golden snapshot with repos cloned and dependencies installed.
#
# Usage: make dev-agent-snapshot
#
# Prerequisites:
#   - make dev-build (builds snapshot-builder binary)
#   - Docker available (for rootfs build)
#   - /dev/kvm available
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
OUTPUT_DIR="/tmp/fc-dev/snapshot-build"
LOG_DIR="/tmp/fc-dev/logs"

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:$PATH"

echo "=== Phase 1: AI Agent Sandbox Onboarding ==="

# --- 1. Build agent rootfs if not present ---
# We check for a marker that indicates the agent rootfs was built (vs the minimal dev one).
# If rootfs.img doesn't exist at all, or we haven't built the agent variant, rebuild.
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

# Snapshot commands: clone repos + install deps
SNAPSHOT_COMMANDS='[
  {"type":"git-clone","args":["https://github.com/pallets/markupsafe","main"]},
  {"type":"git-clone","args":["https://github.com/sindresorhus/is-odd","main"]},
  {"type":"shell","args":["pip3","install","-e","/workspace/markupsafe"],"run_as_root":true},
  {"type":"shell","args":["bash","-c","cd /workspace/is-odd && npm install"]}
]'

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR" "$LOG_DIR"

echo "Snapshot commands: $SNAPSHOT_COMMANDS"
echo "Output dir: $OUTPUT_DIR"
echo ""

set +e
sudo "$REPO_ROOT/bin/snapshot-builder" \
  --gcs-bucket=local-dev-unused \
  --gcs-prefix=v1 \
  --output-dir="$OUTPUT_DIR" \
  --kernel-path="$SNAPSHOT_DIR/kernel.bin" \
  --rootfs-path="$SNAPSHOT_DIR/rootfs.img" \
  --firecracker-bin=/usr/local/bin/firecracker \
  --vcpus=2 \
  --memory-mb=4096 \
  --warmup-timeout=5m \
  --repo-cache-seed-size-gb=1 \
  --repo-cache-upper-size-gb=1 \
  --enable-chunked=false \
  --snapshot-commands="$SNAPSHOT_COMMANDS" \
  --log-level=info \
  2>&1 | tee "$LOG_DIR/agent-snapshot-builder.log"
BUILD_EXIT=$?
set -e

if [ $BUILD_EXIT -ne 0 ]; then
  echo ""
  echo "--- Snapshot builder exited with code $BUILD_EXIT ---"
  echo "    (Expected: GCS upload fails without credentials, but local files should exist)"
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
for f in kernel.bin rootfs.img snapshot.mem snapshot.state repo-cache-seed.img repo-cache-upper.img credentials.img git-cache.img; do
  if [ -f "$OUTPUT_DIR/$f" ]; then
    cp "$OUTPUT_DIR/$f" "$SNAPSHOT_DIR/$f"
    echo "  $f ($(du -sh "$OUTPUT_DIR/$f" | cut -f1))"
  fi
done

# Write metadata.json
cat > "$SNAPSHOT_DIR/metadata.json" << METADATA
{
  "version": "agent-sandbox-snapshot",
  "created_at": "$(date -Iseconds)",
  "kernel_path": "kernel.bin",
  "rootfs_path": "rootfs.img",
  "mem_path": "snapshot.mem",
  "state_path": "snapshot.state",
  "repo_cache_seed_path": "repo-cache-seed.img"
}
METADATA

echo ""
echo "=== Phase 1 complete: agent sandbox snapshot built ==="
echo ""
echo "Files in $SNAPSHOT_DIR:"
ls -lh "$SNAPSHOT_DIR/"
echo ""
echo "Run 'make dev-stop && make dev-stack && make dev-test-agent-sessions' to test."
