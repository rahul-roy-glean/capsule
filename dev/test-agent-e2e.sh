#!/bin/bash
# AI Agent Sandbox E2E Test — full orchestrator.
#
# Runs both phases:
#   Phase 1: Onboard (build agent rootfs + golden snapshot with repos)
#   Phase 2: Sessions (Claude interaction + pause/resume + network policy)
#
# When GCS_BUCKET is set, the entire flow uses GCS:
#   - Golden snapshot uploaded as chunked to GCS
#   - Manager started with --use-chunked-snapshots + --enable-session-chunks
#   - Session pause uploads diff chunks to GCS
#   - Resume deletes local layers and restores from GCS (cross-host simulation)
#
# Usage:
#   GCS_BUCKET=my-bucket make dev-test-agent-e2e   # full GCS route
#   make dev-test-agent-e2e                         # local-only fallback
#
# Prerequisites:
#   - make dev-build (builds binaries)
#   - Docker available (for rootfs build)
#   - /dev/kvm available
#   - gcloud auth (if using GCS)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GCS_BUCKET=${GCS_BUCKET:-}

echo "========================================="
echo "=== AI Agent Sandbox E2E Test ==="
echo "========================================="
if [ -n "$GCS_BUCKET" ]; then
  echo "  Mode: GCS (bucket=$GCS_BUCKET)"
else
  echo "  Mode: local-only"
fi
echo ""

# --- Phase 1: Onboard (build rootfs + golden snapshot) ---
echo "Phase 1: Onboard (build rootfs + golden snapshot)"
echo "-------------------------------------------------"
# GCS_BUCKET is passed through to test-agent-onboard.sh via env
bash dev/test-agent-onboard.sh

echo ""
echo "Phase 1 complete. Restarting stack with new snapshot..."
echo ""

# Restart the stack so the manager picks up the new snapshot.
# When GCS_BUCKET is set, start with chunked snapshots + GCS session chunks.
bash dev/stop-stack.sh 2>/dev/null || true
sleep 2

if [ -n "$GCS_BUCKET" ]; then
  SESSION_CHUNK_BUCKET="$GCS_BUCKET" bash dev/run-stack.sh
else
  bash dev/run-stack.sh
fi
sleep 3

# --- Phase 2: Sessions (Claude interaction + pause/resume) ---
echo ""
echo "Phase 2: Sessions (Claude interaction + pause/resume + policy)"
echo "--------------------------------------------------------------"
# SESSION_CHUNK_BUCKET tells test-agent-sessions.sh to verify GCS artifacts
# and do cross-host simulation (delete local layers before resume).
if [ -n "$GCS_BUCKET" ]; then
  SESSION_CHUNK_BUCKET="$GCS_BUCKET" bash dev/test-agent-sessions.sh
else
  bash dev/test-agent-sessions.sh
fi

echo ""
echo "========================================="
echo "=== AI Agent Sandbox E2E Complete ==="
echo "========================================="
