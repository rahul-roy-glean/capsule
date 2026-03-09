#!/bin/bash
# Convenience wrapper for the full AI agent sandbox workflow.
#
# Runs both phases:
#   1. Provision an agent snapshot
#   2. Restart the local stack and run agent session tests
#
# Usage:
#   GCS_BUCKET=my-bucket make dev-run-agent-e2e
#   make dev-run-agent-e2e
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GCS_BUCKET=${GCS_BUCKET:-}

echo "========================================="
echo "=== AI Agent Sandbox End-to-End Run ==="
echo "========================================="
if [ -n "$GCS_BUCKET" ]; then
  echo "  Mode: GCS (bucket=$GCS_BUCKET)"
else
  echo "  Mode: local-only"
fi
echo ""

echo "Phase 1: Provision agent snapshot"
echo "---------------------------------"
bash dev/provision-agent-snapshot.sh

echo ""
echo "Provisioning complete. Restarting stack..."
echo ""

bash dev/stop-stack.sh 2>/dev/null || true
sleep 2

if [ -n "$GCS_BUCKET" ]; then
  GCS_BUCKET="$GCS_BUCKET" bash dev/run-stack.sh
else
  bash dev/run-stack.sh
fi
sleep 3

echo ""
echo "Phase 2: Agent session lifecycle tests"
echo "--------------------------------------"
if [ -n "$GCS_BUCKET" ]; then
  GCS_BUCKET="$GCS_BUCKET" bash dev/test-agent-sessions.sh
else
  bash dev/test-agent-sessions.sh
fi

echo ""
echo "========================================="
echo "=== AI Agent Sandbox End-to-End Complete ==="
echo "========================================="
