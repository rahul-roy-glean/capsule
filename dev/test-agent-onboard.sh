#!/bin/bash
# Phase 1: Onboard repos + build golden snapshot for AI agent sandbox testing.
#
# This builds the agent rootfs (if needed), then uses snapshot-builder to create
# a golden snapshot with repos cloned and dependencies installed.
#
# When GCS_BUCKET is set, uploads a chunked golden snapshot to GCS so the manager
# can lazy-load via UFFD+FUSE, and session pause/resume uses GCS-backed chunks.
# Without GCS_BUCKET, falls back to local-only mode (no chunked, no GCS).
#
# Usage:
#   GCS_BUCKET=my-bucket make dev-agent-snapshot    # GCS route
#   make dev-agent-snapshot                          # local-only fallback
#
# Prerequisites:
#   - make dev-build (builds snapshot-builder binary)
#   - Docker available (for rootfs build)
#   - /dev/kvm available
#   - gcloud auth (if using GCS)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
OUTPUT_DIR="/tmp/fc-dev/snapshot-build"
LOG_DIR="/tmp/fc-dev/logs"

# GCS config: set GCS_BUCKET to enable chunked upload + GCS session resume
GCS_BUCKET=${GCS_BUCKET:-}
GCS_PREFIX=${GCS_PREFIX:-v1}

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:$PATH"

echo "=== Phase 1: AI Agent Sandbox Onboarding ==="
if [ -n "$GCS_BUCKET" ]; then
  echo "  GCS mode: bucket=$GCS_BUCKET prefix=$GCS_PREFIX (chunked + UFFD/FUSE)"
else
  echo "  Local-only mode (set GCS_BUCKET=<bucket> for GCS route)"
fi

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

# Snapshot commands: clone repos + install deps.
# We use "shell" + git clone instead of "git-clone" because:
#   1. git-clone uses the repoName/repoName convention (GitHub Actions compat)
#      which puts repos at /workspace/markupsafe/markupsafe — awkward for agent use
#   2. git-clone requires a git_token in MMDS for private repos; public repos
#      work fine with plain git clone + GIT_TERMINAL_PROMPT=0
SNAPSHOT_COMMANDS='[
  {"type":"shell","args":["git","clone","--depth=1","--branch","main","https://github.com/pallets/markupsafe","/workspace/markupsafe"]},
  {"type":"shell","args":["git","clone","--depth=1","--branch","main","https://github.com/sindresorhus/camelcase","/workspace/camelcase"]},
  {"type":"shell","args":["pip3","install","--break-system-packages","-e","/workspace/markupsafe"],"run_as_root":true},
  {"type":"shell","args":["bash","-c","cd /workspace/camelcase && npm install"]}
]'

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR" "$LOG_DIR"

echo "Snapshot commands: $SNAPSHOT_COMMANDS"
echo "Output dir: $OUTPUT_DIR"
echo ""

# Build snapshot-builder flags based on GCS availability
BUILDER_FLAGS=(
  --gcs-prefix="$GCS_PREFIX"
  --output-dir="$OUTPUT_DIR"
  --kernel-path="$SNAPSHOT_DIR/kernel.bin"
  --rootfs-path="$SNAPSHOT_DIR/rootfs.img"
  --firecracker-bin=/usr/local/bin/firecracker
  --vcpus=2
  --memory-mb=4096
  --warmup-timeout=5m
  --repo-cache-seed-size-gb=1
  --repo-cache-upper-size-gb=1
  --snapshot-commands="$SNAPSHOT_COMMANDS"
  --log-level=info
)

if [ -n "$GCS_BUCKET" ]; then
  BUILDER_FLAGS+=(--gcs-bucket="$GCS_BUCKET" --enable-chunked=true)
  echo "--- Snapshot will be uploaded to GCS (chunked) ---"
else
  BUILDER_FLAGS+=(--gcs-bucket=local-dev-unused --enable-chunked=false)
fi

set +e
sudo "$REPO_ROOT/bin/snapshot-builder" \
  "${BUILDER_FLAGS[@]}" \
  2>&1 | tee "$LOG_DIR/agent-snapshot-builder.log"
BUILD_EXIT=$?
set -e

if [ $BUILD_EXIT -ne 0 ]; then
  echo ""
  echo "--- Snapshot builder exited with code $BUILD_EXIT ---"
  if [ -n "$GCS_BUCKET" ]; then
    echo "    Check GCS credentials if upload failed."
  else
    echo "    (Expected: GCS upload fails without credentials, but local files should exist)"
  fi
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
if [ -n "$GCS_BUCKET" ]; then
  echo "GCS chunked snapshot uploaded to gs://$GCS_BUCKET/$GCS_PREFIX/"
  echo "Run: SESSION_CHUNK_BUCKET=$GCS_BUCKET make dev-stop dev-stack dev-test-agent-sessions"
else
  echo "Run 'make dev-stop && make dev-stack && make dev-test-agent-sessions' to test."
fi
