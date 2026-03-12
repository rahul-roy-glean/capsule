#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF' >&2
usage: .github/scripts/check-migrations.sh [--staged|--ci]

  --staged  Check the staged git diff (for pre-commit)
  --ci      Check the current branch diff against the merge base / previous commit
EOF
  exit 1
}

if [[ $# -ne 1 ]]; then
  usage
fi

mode="$1"
target_paths=("cmd/capsule-control-plane/migrations" "cmd/capsule-control-plane/schema.sql")

case "$mode" in
  --staged)
    diff_output="$(git diff --cached --name-status -- "${target_paths[@]}")"
    ;;
  --ci)
    if [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" && -n "${GITHUB_BASE_REF:-}" ]]; then
      git fetch --no-tags --depth=1 origin "${GITHUB_BASE_REF}" >/dev/null 2>&1 || true
      merge_base="$(git merge-base HEAD "origin/${GITHUB_BASE_REF}")"
      diff_output="$(git diff --name-status "${merge_base}"...HEAD -- "${target_paths[@]}")"
    elif git rev-parse HEAD^ >/dev/null 2>&1; then
      diff_output="$(git diff --name-status HEAD^..HEAD -- "${target_paths[@]}")"
    else
      diff_output=""
    fi
    ;;
  *)
    usage
    ;;
esac

if [[ -z "${diff_output}" ]]; then
  exit 0
fi

schema_changed=false
added_migration=false
errors=()

while IFS=$'\t' read -r status path extra; do
  [[ -z "${status}" ]] && continue

  if [[ "${path}" == "cmd/capsule-control-plane/schema.sql" || "${extra:-}" == "cmd/capsule-control-plane/schema.sql" ]]; then
    schema_changed=true
  fi

  for candidate in "$path" "${extra:-}"; do
    [[ -z "${candidate}" ]] && continue
    if [[ "${candidate}" == cmd/capsule-control-plane/migrations/*.sql ]]; then
      if [[ "${status}" == A* && "${candidate}" == "${path}" ]]; then
        added_migration=true
        base_name="$(basename "${candidate}")"
        if [[ ! "${base_name}" =~ ^[0-9]{3}_[a-z0-9_]+\.sql$ ]]; then
          errors+=("New migration '${candidate}' must match NNN_description.sql (lowercase, underscore-separated).")
        fi
      else
        errors+=("Do not edit existing migration file '${candidate}'. Create a new numbered migration instead.")
      fi
    fi
  done
done <<< "${diff_output}"

if [[ "${schema_changed}" == "true" && "${added_migration}" != "true" ]]; then
  errors+=("schema.sql changed without adding a new migration file. Add a new numbered migration under cmd/capsule-control-plane/migrations/.")
fi

if [[ ${#errors[@]} -gt 0 ]]; then
  printf 'Migration guard failed:\n' >&2
  for err in "${errors[@]}"; do
    printf '  - %s\n' "${err}" >&2
  done
  exit 1
fi
