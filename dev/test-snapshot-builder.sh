#!/bin/bash
# Snapshot-builder integration smoke tests.
#
# Covers:
#   1. Legacy rootfs.img build path produces snapshot artifacts
#   2. Base-image path works with a digest-pinned Debian-like image
#   3. Incremental base-image builds reject unpinned image references
#   4. Unsupported base images fail before VM boot
#
# Usage:
#   make dev-test-snapshot-builder
#   SNAPSHOT_BUILDER_BASE_IMAGE=debian:bookworm-slim make dev-test-snapshot-builder
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
TEST_ROOT="/tmp/fc-dev/snapshot-builder-tests"
LOG_DIR="$TEST_ROOT/logs"
BIN="$REPO_ROOT/bin/snapshot-builder"
THAW_AGENT_BIN="$REPO_ROOT/bin/thaw-agent"
KERNEL="$BASE_SNAPSHOT_DIR/kernel.bin"
ROOTFS="$BASE_SNAPSHOT_DIR/rootfs.img"

BASE_IMAGE=${SNAPSHOT_BUILDER_BASE_IMAGE:-debian:bookworm-slim}
UNSUPPORTED_IMAGE=${SNAPSHOT_BUILDER_UNSUPPORTED_IMAGE:-busybox:latest}
GCS_BUCKET=${SNAPSHOT_TEST_BUCKET:-local-dev-unused}
PASS=0
FAIL=0
SKIP=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
skip() { echo "  ⊘ $1"; SKIP=$((SKIP + 1)); }
header() { echo ""; echo "=== $1 ==="; }

ensure_file() {
  local path="$1" label="$2"
  if [ ! -f "$path" ]; then
    echo "FAIL: $label not found at $path"
    exit 1
  fi
}

run_builder_capture() {
  local log_file="$1"
  shift

  set +e
  sudo "$BIN" "$@" >"$log_file" 2>&1
  local exit_code=$?
  set -e
  return "$exit_code"
}

assert_artifacts_exist() {
  local output_dir="$1"
  if [ -f "$output_dir/snapshot.mem" ] && [ -f "$output_dir/snapshot.state" ]; then
    return 0
  fi
  return 1
}

resolve_repo_digest() {
  local image="$1"
  docker pull --platform=linux/amd64 "$image" >/dev/null
  docker inspect --format '{{index .RepoDigests 0}}' "$image" 2>/dev/null || true
}

mkdir -p "$LOG_DIR"
rm -rf "$TEST_ROOT"/legacy "$TEST_ROOT"/base-image "$TEST_ROOT"/unsupported "$TEST_ROOT"/invalid-policy

header "0. Prerequisites"
ensure_file "$BIN" "snapshot-builder binary"
ensure_file "$THAW_AGENT_BIN" "thaw-agent binary"
ensure_file "$KERNEL" "kernel"
ensure_file "$ROOTFS" "rootfs image"
if [ ! -e /dev/kvm ]; then
  echo "FAIL: /dev/kvm not found. KVM is required."
  exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "FAIL: docker is required for snapshot-builder base-image tests."
  exit 1
fi
pass "Required binaries and /dev/kvm present"

header "1. Legacy rootfs build smoke"
LEGACY_OUT="$TEST_ROOT/legacy/out"
mkdir -p "$LEGACY_OUT"
LEGACY_LOG="$LOG_DIR/legacy.log"
if run_builder_capture "$LEGACY_LOG" \
  --gcs-bucket="$GCS_BUCKET" \
  --gcs-prefix=v1 \
  --output-dir="$LEGACY_OUT" \
  --kernel-path="$KERNEL" \
  --rootfs-path="$ROOTFS" \
  --firecracker-bin=/usr/local/bin/firecracker \
  --vcpus=2 \
  --memory-mb=2048 \
  --warmup-timeout=5m \
  --enable-chunked=false \
  --snapshot-commands='[{"type":"shell","args":["echo","dev-snapshot-ready"]}]' \
  --log-level=info; then
  if assert_artifacts_exist "$LEGACY_OUT"; then
    pass "Legacy rootfs path produced snapshot artifacts"
  else
    fail "Legacy rootfs path exited successfully but missing snapshot artifacts"
  fi
else
  if assert_artifacts_exist "$LEGACY_OUT"; then
    pass "Legacy rootfs path produced snapshot artifacts before upload failed (expected without GCS creds)"
  else
    fail "Legacy rootfs path failed before producing snapshot artifacts (see $LEGACY_LOG)"
  fi
fi

header "2. Resolve digest-pinned base image"
PINNED_IMAGE=$(resolve_repo_digest "$BASE_IMAGE")
if [ -z "$PINNED_IMAGE" ] || [ "$PINNED_IMAGE" = "<no value>" ]; then
  fail "Could not resolve digest for base image $BASE_IMAGE"
else
  echo "  Pinned image: $PINNED_IMAGE"
  pass "Resolved digest-pinned base image"
fi

header "3. Base-image pinned Debian-like smoke"
BASE_OUT="$TEST_ROOT/base-image/out"
mkdir -p "$BASE_OUT"
BASE_LOG="$LOG_DIR/base-image.log"
if run_builder_capture "$BASE_LOG" \
  --gcs-bucket="$GCS_BUCKET" \
  --gcs-prefix=v1 \
  --output-dir="$BASE_OUT" \
  --kernel-path="$KERNEL" \
  --firecracker-bin=/usr/local/bin/firecracker \
  --vcpus=2 \
  --memory-mb=2048 \
  --warmup-timeout=5m \
  --enable-chunked=false \
  --base-image="$PINNED_IMAGE" \
  --thaw-agent-path="$THAW_AGENT_BIN" \
  --snapshot-commands='[{"type":"shell","args":["echo","dev-snapshot-ready"]}]' \
  --log-level=info; then
  if assert_artifacts_exist "$BASE_OUT"; then
    pass "Base-image path produced snapshot artifacts"
  else
    fail "Base-image path exited successfully but missing snapshot artifacts"
  fi
else
  if assert_artifacts_exist "$BASE_OUT"; then
    pass "Base-image path produced snapshot artifacts before upload failed (expected without GCS creds)"
  else
    fail "Base-image path failed before producing snapshot artifacts (see $BASE_LOG)"
  fi
fi

header "4. Incremental base-image policy rejects unpinned images"
INVALID_LOG="$LOG_DIR/invalid-policy.log"
if run_builder_capture "$INVALID_LOG" \
  --gcs-bucket="$GCS_BUCKET" \
  --gcs-prefix=v1 \
  --output-dir="$TEST_ROOT/invalid-policy/out" \
  --kernel-path="$KERNEL" \
  --firecracker-bin=/usr/local/bin/firecracker \
  --vcpus=2 \
  --memory-mb=2048 \
  --warmup-timeout=5m \
  --enable-chunked=true \
  --build-type=refresh \
  --base-image="$BASE_IMAGE" \
  --thaw-agent-path="$THAW_AGENT_BIN" \
  --snapshot-commands='[{"type":"shell","args":["echo","dev-snapshot-ready"]}]' \
  --log-level=info; then
  fail "Unpinned incremental base-image build unexpectedly succeeded"
else
  if grep -q "digest-pinned" "$INVALID_LOG"; then
    pass "Unpinned incremental base-image build rejected early"
  else
    fail "Unpinned incremental base-image build failed for the wrong reason (see $INVALID_LOG)"
  fi
fi

header "5. Unsupported base image fails fast"
UNSUPPORTED_LOG="$LOG_DIR/unsupported.log"
if run_builder_capture "$UNSUPPORTED_LOG" \
  --gcs-bucket="$GCS_BUCKET" \
  --gcs-prefix=v1 \
  --output-dir="$TEST_ROOT/unsupported/out" \
  --kernel-path="$KERNEL" \
  --firecracker-bin=/usr/local/bin/firecracker \
  --vcpus=2 \
  --memory-mb=2048 \
  --warmup-timeout=5m \
  --enable-chunked=false \
  --base-image="$UNSUPPORTED_IMAGE" \
  --thaw-agent-path="$THAW_AGENT_BIN" \
  --snapshot-commands='[{"type":"shell","args":["echo","dev-snapshot-ready"]}]' \
  --log-level=info; then
  fail "Unsupported base image unexpectedly succeeded"
else
  if grep -q "unsupported base image" "$UNSUPPORTED_LOG" || grep -q "unsupported rootfs flavor" "$UNSUPPORTED_LOG"; then
    pass "Unsupported base image rejected before boot"
  else
    fail "Unsupported base image failed for the wrong reason (see $UNSUPPORTED_LOG)"
  fi
fi

header "RESULTS"
echo ""
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo "  Skipped: $SKIP"
echo ""

if [ "$FAIL" -gt 0 ]; then
  echo "========================================="
  echo "=== SOME TESTS FAILED ==="
  echo "========================================="
  exit 1
else
  echo "========================================="
  echo "=== SNAPSHOT-BUILDER TESTS PASSED ==="
  echo "========================================="
fi
