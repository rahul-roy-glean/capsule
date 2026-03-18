#!/bin/bash
# Git push PreToolUse Hook
# Blocks pushing new branches that don't follow naming rules before they hit remote.
# Only active when BRANCH_NAMING_ENFORCEMENT_ENABLED=true

set -euo pipefail

# Read the JSON input from stdin
# Format: { "tool_name": "Bash", "tool_input": { "command": "..." } }
input=$(cat)

tool_name=$(echo "$input" | jq -r '.tool_name // ""')
if [[ "$tool_name" != "Bash" ]]; then
  exit 0
fi

command=$(echo "$input" | jq -r '.tool_input.command // ""')
command_lower=$(printf '%s' "$command" | tr '[:upper:]' '[:lower:]')

# Detect git push (best-effort). Skip deletes/tags/all pushes.
if [[ "$command_lower" != git\ push* && "$command_lower" != *" git push"* ]]; then
  exit 0
fi
if [[ "$command_lower" =~ --delete ]] || [[ "$command_lower" =~ -d[[:space:]] ]]; then
  exit 0
fi
if [[ "$command_lower" =~ --tags ]] || [[ "$command_lower" =~ --all ]]; then
  exit 0
fi

REPO_DIR="${REPO_DIR:-/workspace/repo}"
SNAPSHOT_FILE="/opt/claude-hooks/remote-branches-snapshot"
if [[ -f /opt/claude-hooks/branch-guard.sh ]]; then
  source /opt/claude-hooks/branch-guard.sh
fi

if command -v ensure_branch_ok >/dev/null 2>&1; then
  if ! ensure_branch_ok "$REPO_DIR" "$SNAPSHOT_FILE"; then
    print_branch_guard_block "push" && exit 0
  fi
fi

exit 0

