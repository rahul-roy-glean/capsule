#!/bin/bash

resolve_gcs_bucket() {
  echo "${GCS_BUCKET:-${SESSION_CHUNK_BUCKET:-${SNAPSHOT_BUCKET:-}}}"
}

require_gcs_bucket() {
  local bucket
  bucket="$(resolve_gcs_bucket)"
  if [ -z "$bucket" ]; then
    echo "FAIL: GCS_BUCKET is required."
    exit 1
  fi
  echo "$bucket"
}

assert_manager_gcs_mode() {
  local bucket="$1"
  local pid_file="/tmp/fc-dev/pids/capsule-manager.pid"
  if [ ! -f "$pid_file" ]; then
    echo "FAIL: capsule-manager pid file not found at $pid_file"
    exit 1
  fi

  local pid cmdline
  pid=$(cat "$pid_file")
  if [ -z "$pid" ] || [ ! -r "/proc/$pid/cmdline" ]; then
    echo "FAIL: capsule-manager process $pid is not readable via /proc"
    exit 1
  fi
  cmdline=$(tr '\0' ' ' < "/proc/$pid/cmdline")

  echo "$cmdline" | grep -q -- "--snapshot-bucket=$bucket" || {
    echo "FAIL: capsule-manager snapshot bucket does not match GCS_BUCKET=$bucket"
    echo "Command line: $cmdline"
    exit 1
  }
}
