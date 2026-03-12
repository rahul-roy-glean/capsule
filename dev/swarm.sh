#!/usr/bin/env bash
#
# Claude Code Swarm — spins up Firecracker sandboxes with two roles:
#   creators:  explore the codebase, find issues, file them on GitHub
#   fixers:    pick an open issue, implement a fix, open a PR
#
# Usage:
#   ./dev/swarm.sh                          # defaults: 2 creators, 3 fixers
#   ./dev/swarm.sh --creators 3 --fixers 5
#   ./dev/swarm.sh --skip-build             # reuse existing snapshot
#   ./dev/swarm.sh --workload-key abc123    # skip register+build, use known key
#
set -euo pipefail

# ── Defaults ──────────────────────────────────────────────────────────────────
CP_BASE="${CONTROL_PLANE:-http://10.0.16.16:8080}"
CONFIG_FILE="${CONFIG_FILE:-bin/claude-sandbox-config.json}"
NUM_CREATORS=2
NUM_FIXERS=3
TIMEOUT=900         # 15 min per Claude run
SKIP_BUILD=false
WORKLOAD_KEY=""
CONFIG_ID=""
REPO="rahul-roy-glean/bazel-firecracker"
LOG_DIR="/tmp/swarm-$(date +%s)"

# Claude Code environment injected into every /exec call
CLAUDE_ENV='{
  "CLAUDE_CODE_USE_VERTEX": "1",
  "CLOUD_ML_REGION": "us-east5",
  "ANTHROPIC_VERTEX_PROJECT_ID": "dev-sandbox-334901",
  "ANTHROPIC_MODEL": "claude-opus-4-6",
  "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
  "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
  "OTEL_TRACES_EXPORTER": "otlp",
  "GCE_METADATA_HOST": "169.254.169.254",
  "METADATA_SERVER_DETECTION": "assume-present"
}'

# ── Arg parsing ───────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case $1 in
    --creators)       NUM_CREATORS="$2"; shift 2 ;;
    --fixers)         NUM_FIXERS="$2";   shift 2 ;;
    --skip-build)     SKIP_BUILD=true;   shift ;;
    --workload-key)   WORKLOAD_KEY="$2"; shift 2 ;;
    --config-id)      CONFIG_ID="$2";    shift 2 ;;
    --control-plane)  CP_BASE="$2";      shift 2 ;;
    --timeout)        TIMEOUT="$2";      shift 2 ;;
    --config-file)    CONFIG_FILE="$2";  shift 2 ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
done

mkdir -p "$LOG_DIR"
echo "Logs → $LOG_DIR"
echo "Control plane → $CP_BASE"
echo "Swarm: $NUM_CREATORS creators + $NUM_FIXERS fixers"

# ── Helpers ───────────────────────────────────────────────────────────────────

register_config() {
  echo "Registering config from $CONFIG_FILE ..."
  local resp
  resp=$(curl -sS -X POST "$CP_BASE/api/v1/layered-configs" \
    -H "Content-Type: application/json" \
    --data @"$CONFIG_FILE")

  CONFIG_ID=$(echo "$resp" | jq -r '.config_id')
  WORKLOAD_KEY=$(echo "$resp" | jq -r '.leaf_workload_key')

  if [[ -z "$CONFIG_ID" || "$CONFIG_ID" == "null" ]]; then
    echo "ERROR: config registration failed: $resp"
    exit 1
  fi
  echo "  config_id    = $CONFIG_ID"
  echo "  workload_key = $WORKLOAD_KEY"
}

build_and_wait() {
  echo "Triggering snapshot build for config $CONFIG_ID ..."
  curl -sS -X POST "$CP_BASE/api/v1/layered-configs/$CONFIG_ID/build" | jq .

  echo "Waiting for snapshot to be ready ..."
  local attempt=0
  while true; do
    local status
    status=$(curl -sS "$CP_BASE/api/v1/layered-configs/$CONFIG_ID" \
      | jq -r '[.layers[].status] | if all(. == "ready" or . == "active") then "ready" elif any(. == "failed") then "failed" else "building" end')

    if [[ "$status" == "ready" ]]; then
      echo "  Snapshot ready!"
      return
    elif [[ "$status" == "failed" ]]; then
      echo "  ERROR: snapshot build failed"
      curl -sS "$CP_BASE/api/v1/layered-configs/$CONFIG_ID" | jq '.layers[]'
      exit 1
    fi

    attempt=$((attempt + 1))
    if (( attempt > 120 )); then
      echo "  ERROR: timed out waiting for snapshot (60 min)"
      exit 1
    fi
    echo "  [$attempt] status=$status, waiting 30s ..."
    sleep 30
  done
}

allocate_runner() {
  local session_id=$1
  local max_retries=5
  local retry_delay=15

  for attempt in $(seq 1 "$max_retries"); do
    local resp
    resp=$(curl -sS -X POST "$CP_BASE/api/v1/runners/allocate" \
      -H "Content-Type: application/json" \
      -d "{\"workload_key\":\"$WORKLOAD_KEY\", \"session_id\":\"$session_id\"}")

    local runner_id host_addr error
    runner_id=$(echo "$resp" | jq -r '.runner_id')
    host_addr=$(echo "$resp" | jq -r '.host_address')
    error=$(echo "$resp" | jq -r '.error // empty')

    if [[ -z "$error" && -n "$runner_id" && "$runner_id" != "null" ]]; then
      echo "$runner_id|$host_addr"
      return 0
    fi

    if (( attempt < max_retries )); then
      echo "  Allocation attempt $attempt/$max_retries failed for $session_id: $error — retrying in ${retry_delay}s ..." >&2
      sleep "$retry_delay"
    else
      echo "  ERROR allocating $session_id after $max_retries attempts: $error" >&2
      return 1
    fi
  done
}

wait_ready() {
  local runner_id=$1
  local attempt=0
  while true; do
    local http_code
    http_code=$(curl -sS -o /dev/null -w '%{http_code}' \
      "$CP_BASE/api/v1/runners/status?runner_id=$runner_id")
    if [[ "$http_code" == "200" ]]; then return 0; fi
    if [[ "$http_code" == "404" ]]; then
      echo "  Runner $runner_id not found (terminated?)" >&2
      return 1
    fi
    attempt=$((attempt + 1))
    if (( attempt > 60 )); then
      echo "  Timed out waiting for $runner_id" >&2
      return 1
    fi
    sleep 5
  done
}

# Execute a Claude Code prompt inside a runner.
# Streams ndjson, collects stdout lines into the log file.
exec_claude() {
  local host_addr=$1
  local runner_id=$2
  local prompt=$3
  local logfile=$4

  # Build the JSON payload with jq (safe escaping of the prompt)
  local payload
  payload=$(jq -n \
    --arg prompt "$prompt" \
    --argjson env "$CLAUDE_ENV" \
    --argjson timeout "$TIMEOUT" \
    '{
      command: ["claude", "-p", $prompt, "--dangerously-skip-permissions"],
      env: $env,
      working_dir: "/workspace/bazel-firecracker",
      timeout_seconds: $timeout
    }')

  echo "  Executing Claude in $runner_id (log: $logfile) ..."
  curl -sS -N -X POST "http://$host_addr/api/v1/runners/$runner_id/exec" \
    -H "Content-Type: application/json" \
    -d "$payload" \
  | while IFS= read -r line; do
      echo "$line" >> "$logfile"
      # Print stdout lines to console
      local typ
      typ=$(echo "$line" | jq -r '.type // empty' 2>/dev/null)
      if [[ "$typ" == "stdout" ]]; then
        echo "$line" | jq -r '.data // empty' 2>/dev/null
      elif [[ "$typ" == "exit" ]]; then
        local code
        code=$(echo "$line" | jq -r '.code // "?"' 2>/dev/null)
        echo "  [exit $code] $runner_id"
      fi
    done
}

release_runner() {
  local runner_id=$1
  curl -sS -X POST "$CP_BASE/api/v1/runners/release" \
    -H "Content-Type: application/json" \
    -d "{\"runner_id\":\"$runner_id\"}" > /dev/null 2>&1 || true
}

# ── Prompts ───────────────────────────────────────────────────────────────────

CREATOR_PROMPT='You are in the bazel-firecracker repository at /workspace/bazel-firecracker.
This is a Go-based Firecracker MicroVM orchestration system.

Your task: explore the codebase, find a REAL bug, code quality issue, or
improvement opportunity, and create a GitHub issue for it.

Steps:
1. Read key files to understand the codebase (start with cmd/ and pkg/)
2. Look for real issues: race conditions, error handling gaps, resource leaks,
   missing validation, performance problems, or missing features
3. Create a GitHub issue with:
   - A clear, specific title
   - Affected file(s) and line numbers
   - Description of the problem and why it matters
   - Suggested fix approach (if you have one)

To create the issue, run:
  HTTPS_PROXY=http://localhost:3128 gh issue create \
    --repo '"$REPO"' \
    --title "Your title" \
    --body "Your description"

IMPORTANT: Find something real and specific. Do NOT file trivial or vague issues.'

FIXER_PROMPT='You are in the bazel-firecracker repository at /workspace/bazel-firecracker.
This is a Go-based Firecracker MicroVM orchestration system.

Your task: find an open GitHub issue, implement a fix, and open a pull request.

Steps:
1. List open issues:
   HTTPS_PROXY=http://localhost:3128 gh issue list --repo '"$REPO"'
2. Pick one issue that you can realistically fix
3. Read the issue details:
   HTTPS_PROXY=http://localhost:3128 gh issue view <number> --repo '"$REPO"'
4. Understand the relevant code
5. Create a branch: git checkout -b fix/<issue-number>-short-description
6. Implement the fix (edit files, run tests if possible)
7. Commit your changes with a descriptive message referencing the issue
8. Push the branch:
   git push origin fix/<issue-number>-short-description
9. Create a PR:
   HTTPS_PROXY=http://localhost:3128 gh pr create \
     --repo '"$REPO"' \
     --title "Fix #<issue-number>: short description" \
     --body "Closes #<issue-number>\n\n## What\n...\n\n## Why\n..."

IMPORTANT: Make a real, working fix. Test your changes if possible.'

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
  echo "════════════════════════════════════════════════════════════════"
  echo "  Claude Code Swarm"
  echo "════════════════════════════════════════════════════════════════"

  # Step 1: Register config + build snapshot (unless skipped)
  if [[ -z "$WORKLOAD_KEY" ]]; then
    register_config
    if [[ "$SKIP_BUILD" == "false" ]]; then
      build_and_wait
    fi
  else
    echo "Using provided workload_key=$WORKLOAD_KEY"
  fi

  # Track all runner IDs for cleanup
  declare -a ALL_RUNNERS=()

  # Step 2: Phase 1 — Issue Creators
  echo ""
  echo "──── Phase 1: Issue Creators ($NUM_CREATORS) ────"
  declare -a creator_pids=()
  for i in $(seq 1 "$NUM_CREATORS"); do
    (
      local_log="$LOG_DIR/creator-$i.ndjson"
      echo "Allocating creator-$i ..."
      result=$(allocate_runner "swarm-creator-$i") || exit 1
      runner_id=$(echo "$result" | cut -d'|' -f1)
      host_addr=$(echo "$result" | cut -d'|' -f2)
      echo "  creator-$i → runner=$runner_id host=$host_addr"

      echo "  Waiting for runner to be ready ..."
      wait_ready "$runner_id" || exit 1

      exec_claude "$host_addr" "$runner_id" "$CREATOR_PROMPT" "$local_log"
      release_runner "$runner_id"
      echo "  creator-$i complete."
    ) &
    creator_pids+=($!)
  done

  # Wait for all creators to finish before fixers start
  local failed=0
  for pid in "${creator_pids[@]}"; do
    if ! wait "$pid"; then
      echo "WARNING: a creator process failed"
      failed=$((failed + 1))
    fi
  done
  echo "Phase 1 done ($((NUM_CREATORS - failed))/$NUM_CREATORS succeeded)"

  # Step 3: Phase 2 — Issue Fixers
  echo ""
  echo "──── Phase 2: Issue Fixers ($NUM_FIXERS) ────"
  declare -a fixer_pids=()
  for i in $(seq 1 "$NUM_FIXERS"); do
    (
      local_log="$LOG_DIR/fixer-$i.ndjson"
      echo "Allocating fixer-$i ..."
      result=$(allocate_runner "swarm-fixer-$i") || exit 1
      runner_id=$(echo "$result" | cut -d'|' -f1)
      host_addr=$(echo "$result" | cut -d'|' -f2)
      echo "  fixer-$i → runner=$runner_id host=$host_addr"

      echo "  Waiting for runner to be ready ..."
      wait_ready "$runner_id" || exit 1

      exec_claude "$host_addr" "$runner_id" "$FIXER_PROMPT" "$local_log"
      release_runner "$runner_id"
      echo "  fixer-$i complete."
    ) &
    fixer_pids+=($!)
  done

  failed=0
  for pid in "${fixer_pids[@]}"; do
    if ! wait "$pid"; then
      echo "WARNING: a fixer process failed"
      failed=$((failed + 1))
    fi
  done
  echo "Phase 2 done ($((NUM_FIXERS - failed))/$NUM_FIXERS succeeded)"

  # Summary
  echo ""
  echo "════════════════════════════════════════════════════════════════"
  echo "  Swarm complete"
  echo "  Logs: $LOG_DIR/"
  echo "  Issues: https://github.com/$REPO/issues"
  echo "  PRs:    https://github.com/$REPO/pulls"
  echo "════════════════════════════════════════════════════════════════"
}

main "$@"
