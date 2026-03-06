#!/bin/bash
# Performance benchmark for allocate / pause / resume operations.
#
# Measures wall-clock latency for each operation across multiple dirty-page
# workloads. Outputs a summary table at the end.
#
# Usage:
#   SESSION_CHUNK_BUCKET=rroy-gc-testing make dev-bench-pause-resume
#
# Prerequisites:
#   - Golden chunked snapshot uploaded: GCS_BUCKET=<bucket> ENABLE_CHUNKED=true make dev-snapshot
#   - Stack running with GCS sessions:  SESSION_CHUNK_BUCKET=<bucket> make dev-stack
set -uo pipefail

CP=http://localhost:8080
MGR=http://localhost:9080
GCS_BUCKET=${SESSION_CHUNK_BUCKET:-}
SNAPSHOT_COMMANDS=${SNAPSHOT_COMMANDS:-'[{"type":"shell","args":["echo","dev-snapshot-ready"]}]'}

if [ -z "$GCS_BUCKET" ]; then
  echo "FAIL: SESSION_CHUNK_BUCKET is required."
  exit 1
fi

# --- Helpers ---

now_ms() { date +%s%3N; }

elapsed_ms() {
  local start=$1 end=$2
  echo $((end - start))
}

register_config() {
  curl -s -X POST "$CP/api/v1/layered-configs" \
    -H 'Content-Type: application/json' \
    -d '{
      "display_name": "bench-pause-resume",
      "commands": '"$SNAPSHOT_COMMANDS"',
      "runner_ttl_seconds": 600,
      "auto_pause": true,
      "session_max_age_seconds": 3600
    }' | jq -r '.workload_key'
}

allocate() {
  local wk=$1 sid=$2
  curl -s -X POST "$CP/api/v1/runners/allocate" \
    -H 'Content-Type: application/json' \
    -d "{\"ci_system\":\"none\", \"workload_key\":\"$wk\", \"session_id\":\"$sid\"}"
}

wait_ready() {
  local runner_id=$1 max_wait=${2:-60}
  for i in $(seq 1 "$max_wait"); do
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
      "$CP/api/v1/runners/status?runner_id=$runner_id")
    [ "$HTTP_CODE" = "200" ] && return 0
    sleep 1
  done
  return 1
}

wait_exec_ready() {
  local runner_id=$1 max_wait=${2:-30}
  for i in $(seq 1 "$max_wait"); do
    OUT=$(curl -s --no-buffer --max-time 5 -X POST "$MGR/api/v1/runners/$runner_id/exec" \
      -H 'Content-Type: application/json' \
      -d '{"command":["echo","ready"],"timeout_seconds":3}' 2>&1 || true)
    echo "$OUT" | grep -q 'ready' && return 0
    sleep 1
  done
  return 1
}

vm_exec() {
  local runner_id=$1 cmd=$2
  curl -s --no-buffer --max-time 30 -X POST "$MGR/api/v1/runners/$runner_id/exec" \
    -H 'Content-Type: application/json' \
    -d "{\"command\":[\"sh\",\"-c\",\"$cmd\"],\"timeout_seconds\":20}" 2>&1 || true
}

pause_runner() {
  local runner_id=$1
  curl -s -X POST "$CP/api/v1/runners/pause" \
    -H 'Content-Type: application/json' \
    -d "{\"runner_id\":\"$runner_id\"}"
}

release_runner() {
  local runner_id=$1
  curl -s -X POST "$CP/api/v1/runners/release" \
    -H 'Content-Type: application/json' \
    -d "{\"runner_id\":\"$runner_id\"}" > /dev/null 2>&1 || true
}

# --- Setup ---
echo "==========================================="
echo "  Pause/Resume Performance Benchmark"
echo "==========================================="
echo ""
echo "GCS bucket: $GCS_BUCKET"
echo ""

WORKLOAD_KEY=$(register_config)
echo "Workload key: $WORKLOAD_KEY"
echo ""

# --- Results table ---
printf "%-12s %10s %10s %10s %10s %10s %10s %12s\n" \
  "DIRTY_MB" "ALLOC_ms" "EXEC_ms" "PAUSE_ms" "RESUME_ms" "VERIFY_ms" "TOTAL_ms" "CHUNKS_UP"
printf "%-12s %10s %10s %10s %10s %10s %10s %12s\n" \
  "--------" "--------" "--------" "--------" "--------" "--------" "--------" "--------"

# --- Benchmark loop ---
# Test with different amounts of dirty data: 0MB, 1MB, 4MB (1 chunk), 16MB (4 chunks), 64MB
for DIRTY_MB in 0 1 4 16 64; do
  SESSION_ID="bench-$(date +%s)-${DIRTY_MB}mb"

  # 1. Allocate
  T_START=$(now_ms)
  ALLOC_RESP=$(allocate "$WORKLOAD_KEY" "$SESSION_ID")
  RUNNER_ID=$(echo "$ALLOC_RESP" | jq -r '.runner_id')
  if [ -z "$RUNNER_ID" ] || [ "$RUNNER_ID" = "null" ]; then
    echo "SKIP ${DIRTY_MB}MB: allocate failed: $ALLOC_RESP"
    continue
  fi

  wait_ready "$RUNNER_ID" 60
  wait_exec_ready "$RUNNER_ID" 30
  T_ALLOC=$(now_ms)
  ALLOC_MS=$(elapsed_ms "$T_START" "$T_ALLOC")

  # 2. Write dirty data
  T_EXEC_START=$(now_ms)
  if [ "$DIRTY_MB" -gt 0 ]; then
    vm_exec "$RUNNER_ID" "dd if=/dev/urandom of=/tmp/bench-dirty.bin bs=1M count=$DIRTY_MB 2>/dev/null && echo done" > /dev/null
  fi
  # Also write a verification marker
  vm_exec "$RUNNER_ID" "echo bench-marker-$DIRTY_MB > /tmp/bench-marker.txt" > /dev/null
  T_EXEC_END=$(now_ms)
  EXEC_MS=$(elapsed_ms "$T_EXEC_START" "$T_EXEC_END")

  # 3. Pause (includes GCS upload)
  T_PAUSE_START=$(now_ms)
  PAUSE_RESP=$(pause_runner "$RUNNER_ID")
  T_PAUSE_END=$(now_ms)
  PAUSE_MS=$(elapsed_ms "$T_PAUSE_START" "$T_PAUSE_END")
  PAUSE_SIZE=$(echo "$PAUSE_RESP" | jq -r '.snapshot_size_bytes // 0')

  # Delete local session files to force GCS resume
  SESSION_DIR="/tmp/fc-dev/sessions/$SESSION_ID"
  rm -rf "$SESSION_DIR/layer_"* 2>/dev/null

  # 4. Resume from GCS
  T_RESUME_START=$(now_ms)
  RESUME_RESP=$(allocate "$WORKLOAD_KEY" "$SESSION_ID")
  RESUME_ID=$(echo "$RESUME_RESP" | jq -r '.runner_id')
  RESUMED=$(echo "$RESUME_RESP" | jq -r '.resumed // false')

  if [ "$RESUMED" != "true" ]; then
    echo "SKIP ${DIRTY_MB}MB: resume failed: $RESUME_RESP"
    release_runner "$RUNNER_ID" 2>/dev/null || true
    continue
  fi

  wait_ready "$RESUME_ID" 60
  wait_exec_ready "$RESUME_ID" 30
  T_RESUME_END=$(now_ms)
  RESUME_MS=$(elapsed_ms "$T_RESUME_START" "$T_RESUME_END")

  # 5. Verify marker survived
  T_VERIFY_START=$(now_ms)
  VERIFY_OUT=$(vm_exec "$RESUME_ID" "cat /tmp/bench-marker.txt")
  T_VERIFY_END=$(now_ms)
  VERIFY_MS=$(elapsed_ms "$T_VERIFY_START" "$T_VERIFY_END")

  VERIFIED="FAIL"
  echo "$VERIFY_OUT" | grep -q "bench-marker-$DIRTY_MB" && VERIFIED="OK"

  # Count uploaded chunks from GCS session metadata
  GCS_MEM_INDEX=$(jq -r '.gcs_mem_index_object // empty' "$SESSION_DIR/metadata.json" 2>/dev/null || echo "")
  CHUNKS_UP="?"
  if [ -n "$GCS_MEM_INDEX" ]; then
    CHUNKS_UP=$(gsutil cat "gs://$GCS_BUCKET/$GCS_MEM_INDEX" 2>/dev/null | jq '.region.extents | length' 2>/dev/null || echo "?")
  fi

  TOTAL_MS=$((ALLOC_MS + EXEC_MS + PAUSE_MS + RESUME_MS + VERIFY_MS))

  printf "%-12s %10d %10d %10d %10d %10d %10d %12s\n" \
    "${DIRTY_MB}MB${VERIFIED:+ ($VERIFIED)}" \
    "$ALLOC_MS" "$EXEC_MS" "$PAUSE_MS" "$RESUME_MS" "$VERIFY_MS" "$TOTAL_MS" "$CHUNKS_UP"

  # Cleanup
  release_runner "$RESUME_ID"

  # Wait for slot to be fully freed before next iteration
  sleep 3
done

echo ""
echo "==========================================="
echo "  Sequential benchmark complete"
echo "==========================================="

# Wait for all resources to be fully released before concurrent benchmark
echo ""
echo "Waiting for resource cleanup before concurrent benchmark..."
sleep 5

# ==========================================================================
# Part 2: Concurrent scaling benchmark
# ==========================================================================
echo ""
echo "==========================================="
echo "  Concurrent Scaling Benchmark"
echo "==========================================="
echo ""

CONCURRENCY_LEVELS=${BENCH_CONCURRENCY:-"1 2 4"}
DIRTY_MB_CONCURRENT=${BENCH_DIRTY_MB:-4}  # fixed dirty size for scaling test
TMPDIR_BENCH=$(mktemp -d)

# run_single_cycle: allocate → write → pause → delete local → resume → verify → release
# Writes timing to $TMPDIR_BENCH/result-$slot.txt
run_single_cycle() {
  local slot=$1 wk=$2 dirty_mb=$3
  local sid="bench-conc-$(date +%s)-slot${slot}-$$"
  local result_file="$TMPDIR_BENCH/result-${slot}.txt"

  # Allocate
  local t0=$(now_ms)
  local resp=$(allocate "$wk" "$sid")
  local rid=$(echo "$resp" | jq -r '.runner_id')
  if [ -z "$rid" ] || [ "$rid" = "null" ]; then
    echo "ALLOC_FAIL slot=$slot resp=$resp" >> "$TMPDIR_BENCH/errors.log"
    echo "ALLOC_FAIL" > "$result_file"
    return
  fi
  wait_ready "$rid" 60 || { echo "READY_TIMEOUT slot=$slot rid=$rid" >> "$TMPDIR_BENCH/errors.log"; echo "ALLOC_FAIL" > "$result_file"; release_runner "$rid"; return; }
  wait_exec_ready "$rid" 30 || { echo "EXEC_READY_TIMEOUT slot=$slot rid=$rid" >> "$TMPDIR_BENCH/errors.log"; echo "ALLOC_FAIL" > "$result_file"; release_runner "$rid"; return; }
  local t_alloc=$(now_ms)

  # Write dirty data + marker
  if [ "$dirty_mb" -gt 0 ]; then
    vm_exec "$rid" "dd if=/dev/urandom of=/tmp/bench-dirty.bin bs=1M count=$dirty_mb 2>/dev/null" > /dev/null
  fi
  vm_exec "$rid" "echo conc-marker-$slot > /tmp/bench-marker.txt" > /dev/null
  local t_exec=$(now_ms)

  # Pause
  pause_runner "$rid" > /dev/null
  local t_pause=$(now_ms)

  # Delete local layers
  local sdir="/tmp/fc-dev/sessions/$sid"
  rm -rf "$sdir/layer_"* 2>/dev/null

  # Resume
  local rresp=$(allocate "$wk" "$sid")
  local rrid=$(echo "$rresp" | jq -r '.runner_id')
  local resumed=$(echo "$rresp" | jq -r '.resumed // false')
  if [ "$resumed" != "true" ]; then
    echo "RESUME_FAIL slot=$slot resp=$rresp" >> "$TMPDIR_BENCH/errors.log"
    echo "RESUME_FAIL" > "$result_file"
    release_runner "$rid" 2>/dev/null || true
    return
  fi
  wait_ready "$rrid" 60 || true
  wait_exec_ready "$rrid" 30 || true
  local t_resume=$(now_ms)

  # Verify
  local vout=$(vm_exec "$rrid" "cat /tmp/bench-marker.txt")
  local verified="FAIL"
  echo "$vout" | grep -q "conc-marker-$slot" && verified="OK"
  local t_verify=$(now_ms)

  # Cleanup
  release_runner "$rrid"

  # Write results
  echo "$(elapsed_ms $t0 $t_alloc) $(elapsed_ms $t_alloc $t_exec) $(elapsed_ms $t_exec $t_pause) $(elapsed_ms $t_pause $t_resume) $(elapsed_ms $t_resume $t_verify) $verified" > "$result_file"
}

printf "%-8s %10s %10s %10s %10s %10s %10s %10s %8s\n" \
  "CONC" "ALLOC_p50" "PAUSE_p50" "RESUME_p50" "ALLOC_max" "PAUSE_max" "RESUME_max" "WALL_ms" "STATUS"
printf "%-8s %10s %10s %10s %10s %10s %10s %10s %8s\n" \
  "----" "---------" "---------" "----------" "---------" "---------" "----------" "-------" "------"

for N in $CONCURRENCY_LEVELS; do
  # Clean up any leftover VMs and wait for slots to free
  sleep 5
  > "$TMPDIR_BENCH/errors.log"  # reset error log

  echo -n "  Running $N concurrent cycles (${DIRTY_MB_CONCURRENT}MB dirty each)..."

  T_WALL_START=$(now_ms)

  # Launch N cycles in parallel
  for slot in $(seq 1 "$N"); do
    run_single_cycle "$slot" "$WORKLOAD_KEY" "$DIRTY_MB_CONCURRENT" &
  done
  wait

  T_WALL_END=$(now_ms)
  WALL_MS=$(elapsed_ms "$T_WALL_START" "$T_WALL_END")
  echo " done (${WALL_MS}ms wall)"

  # Collect results
  ALLOC_TIMES=()
  PAUSE_TIMES=()
  RESUME_TIMES=()
  FAILURES=0
  SUCCESSES=0

  for slot in $(seq 1 "$N"); do
    rf="$TMPDIR_BENCH/result-${slot}.txt"
    if [ ! -f "$rf" ]; then
      FAILURES=$((FAILURES + 1))
      continue
    fi
    content=$(cat "$rf")
    if [ "$content" = "ALLOC_FAIL" ] || [ "$content" = "RESUME_FAIL" ]; then
      FAILURES=$((FAILURES + 1))
      continue
    fi
    read -r a e p r v status <<< "$content"
    ALLOC_TIMES+=("$a")
    PAUSE_TIMES+=("$p")
    RESUME_TIMES+=("$r")
    if [ "$status" = "OK" ]; then
      SUCCESSES=$((SUCCESSES + 1))
    else
      FAILURES=$((FAILURES + 1))
    fi
  done

  # Compute p50 and max (sort numerically, pick median and last)
  percentile() {
    local -n arr=$1
    if [ ${#arr[@]} -eq 0 ]; then echo "0"; return; fi
    local sorted=($(printf '%s\n' "${arr[@]}" | sort -n))
    local mid=$(( ${#sorted[@]} / 2 ))
    echo "${sorted[$mid]}"
  }
  max_of() {
    local -n arr=$1
    if [ ${#arr[@]} -eq 0 ]; then echo "0"; return; fi
    printf '%s\n' "${arr[@]}" | sort -n | tail -1
  }

  A_P50=$(percentile ALLOC_TIMES)
  P_P50=$(percentile PAUSE_TIMES)
  R_P50=$(percentile RESUME_TIMES)
  A_MAX=$(max_of ALLOC_TIMES)
  P_MAX=$(max_of PAUSE_TIMES)
  R_MAX=$(max_of RESUME_TIMES)

  STATUS="${SUCCESSES}ok"
  [ "$FAILURES" -gt 0 ] && STATUS="${STATUS}/${FAILURES}fail"

  printf "%-8s %10d %10d %10d %10d %10d %10d %10d %8s\n" \
    "${N}x" "$A_P50" "$P_P50" "$R_P50" "$A_MAX" "$P_MAX" "$R_MAX" "$WALL_MS" "$STATUS"

  # Print errors if any
  if [ -s "$TMPDIR_BENCH/errors.log" ]; then
    echo "    Errors:"
    sed 's/^/      /' "$TMPDIR_BENCH/errors.log"
  fi

  # Clean result files
  rm -f "$TMPDIR_BENCH"/result-*.txt
done

rm -rf "$TMPDIR_BENCH"

echo ""
echo "==========================================="
echo "  Full benchmark complete"
echo "==========================================="
echo ""
echo "Notes:"
echo "  ALLOC_ms   = fresh allocate from chunked snapshot (UFFD+FUSE) until exec-ready"
echo "  EXEC_ms    = time to write dirty data inside VM"
echo "  PAUSE_ms   = diff snapshot + merge + GCS upload"
echo "  RESUME_ms  = GCS download + UFFD/FUSE setup + restore until exec-ready"
echo "  VERIFY_ms  = read back marker file (proves state survived)"
echo "  CHUNKS_UP  = number of non-zero extents in session ChunkIndex"
echo "  CONC       = number of concurrent pause/resume cycles"
echo "  p50/max    = median and worst-case latency across concurrent runs"
echo "  WALL_ms    = total wall-clock time for all concurrent cycles"
