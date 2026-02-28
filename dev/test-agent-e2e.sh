#!/bin/bash
# AI Agent Sandbox E2E Test — full orchestrator.
#
# Runs both phases:
#   Phase 1: Onboard (build agent rootfs + golden snapshot with repos)
#   Phase 2: Sessions (Claude interaction + pause/resume + network policy)
#
# Usage: make dev-test-agent-e2e
#
# Prerequisites:
#   - make dev-build (builds binaries)
#   - Docker available (for rootfs build)
#   - /dev/kvm available
#   - Stack will be restarted between phases
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "========================================="
echo "=== AI Agent Sandbox E2E Test ==="
echo "========================================="
echo ""

# --- Phase 1: Onboard (build rootfs + golden snapshot) ---
echo "Phase 1: Onboard (build rootfs + golden snapshot)"
echo "-------------------------------------------------"
bash dev/test-agent-onboard.sh

echo ""
echo "Phase 1 complete. Restarting stack with new snapshot..."
echo ""

# Restart the stack so the manager picks up the new snapshot
bash dev/stop-stack.sh 2>/dev/null || true
sleep 2
bash dev/run-stack.sh
sleep 3

# --- Phase 2: Sessions (Claude interaction + pause/resume) ---
echo ""
echo "Phase 2: Sessions (Claude interaction + pause/resume + policy)"
echo "--------------------------------------------------------------"
bash dev/test-agent-sessions.sh

echo ""
echo "========================================="
echo "=== AI Agent Sandbox E2E Complete ==="
echo "========================================="
