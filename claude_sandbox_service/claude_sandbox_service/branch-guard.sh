#!/bin/bash
# Shared branch guard for Claude hooks
# Enforces branch naming convention when BRANCH_NAMING_ENFORCEMENT_ENABLED=true

ensure_branch_ok() {
  local repo_dir="${1:-/workspace/repo}"
  local snapshot_file="${2:-/opt/claude-hooks/remote-branches-snapshot}"

  BRANCH_GUARD_ERR_TYPE=""
  BRANCH_GUARD_BRANCH=""
  BRANCH_GUARD_BRANCH_LOWER=""

  # Skip enforcement if flag is not enabled
  if [[ "${BRANCH_NAMING_ENFORCEMENT_ENABLED:-false}" != "true" ]]; then
    return 0
  fi

  if [[ ! -d "$repo_dir/.git" ]]; then
    return 0
  fi

  local branch
  branch=$(git -C "$repo_dir" branch --show-current 2>/dev/null || true)
  if [[ -z "$branch" ]]; then
    return 0
  fi

  local existed_at_start="false"
  if [[ -f "$snapshot_file" ]]; then
    if grep -q "origin/$branch\$" "$snapshot_file" 2>/dev/null; then
      existed_at_start="true"
    fi
  elif git -C "$repo_dir" rev-parse "origin/$branch" >/dev/null 2>&1; then
    existed_at_start="true"
  fi

  if [[ "$existed_at_start" == "true" ]]; then
    return 0
  fi

  BRANCH_GUARD_BRANCH="$branch"

  if [[ ! "$branch" =~ ^glean/code-writer/.+ ]]; then
    BRANCH_GUARD_ERR_TYPE="pattern"
    return 1
  fi

  if [[ "$branch" =~ [A-Z] ]]; then
    BRANCH_GUARD_ERR_TYPE="lowercase"
    BRANCH_GUARD_BRANCH_LOWER=$(echo "$branch" | tr '[:upper:]' '[:lower:]')
    return 1
  fi

  return 0
}

print_branch_guard_block() {
  local action="$1"

  if [[ "$BRANCH_GUARD_ERR_TYPE" == "pattern" ]]; then
    cat >&2 <<EOF
❌ Cannot ${action}: branch name must be glean/code-writer/<feature>

Current branch '$BRANCH_GUARD_BRANCH' must be renamed.
Run: git branch -m "$BRANCH_GUARD_BRANCH" "glean/code-writer/<your-feature-name>"
EOF
    echo '{"decision":"block","reason":"Rename branch to glean/code-writer/<feature-name> before '"$action"'"}'
    return 0
  fi

  if [[ "$BRANCH_GUARD_ERR_TYPE" == "lowercase" ]]; then
    cat >&2 <<EOF
❌ Cannot ${action}: branch name must be lowercase

Branch '$BRANCH_GUARD_BRANCH' contains uppercase letters.
Run: git branch -m "$BRANCH_GUARD_BRANCH" "$BRANCH_GUARD_BRANCH_LOWER"
EOF
    echo '{"decision":"block","reason":"Branch name must be lowercase before '"$action"'"}'
    return 0
  fi

  return 1
}

