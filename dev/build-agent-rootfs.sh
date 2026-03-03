#!/bin/bash
# Build the agent sandbox rootfs (Python3, Node.js, git, Claude Code) for AI agent E2E testing.
# This extends the minimal dev rootfs with development tools needed by coding agents.
# Usage: make dev-agent-rootfs
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
IMAGE_NAME="fc-agent-rootfs"
ROOTFS_SIZE="1024M"
KERNEL_VERSION="${KERNEL_VERSION:-5.10.242}"

cd "$REPO_ROOT"
export PATH="/usr/local/go/bin:$PATH"

# Detect architecture
ARCH=$(uname -m)  # x86_64 or aarch64
case "$ARCH" in
  x86_64)  GOARCH=amd64; DOCKER_PLATFORM=linux/amd64 ;;
  aarch64) GOARCH=arm64; DOCKER_PLATFORM=linux/arm64 ;;
  *)       echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

echo "=== Building agent sandbox rootfs ==="
echo "Repo root: $REPO_ROOT"
echo "Output:    $SNAPSHOT_DIR"
echo "Arch:      $ARCH ($GOARCH)"
echo "Size:      $ROOTFS_SIZE (includes Python, Node.js, Claude Code)"

mkdir -p "$SNAPSHOT_DIR"

# --- 1. Build thaw-agent binary (for the Docker build context) ---
echo ""
echo "--- Building thaw-agent binary ---"
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/thaw-agent ./cmd/thaw-agent

# --- 2. Build Docker image ---
echo ""
echo "--- Building Docker image (agent rootfs) ---"
docker buildx build --platform "$DOCKER_PLATFORM" --load \
  -t "$IMAGE_NAME" \
  -f dev/Dockerfile.agent-rootfs .

# --- 3. Export rootfs as tar ---
echo ""
echo "--- Exporting rootfs ---"
CONTAINER_ID=$(docker create "$IMAGE_NAME")
docker export "$CONTAINER_ID" > /tmp/fc-agent-rootfs.tar
docker rm "$CONTAINER_ID"

# --- 4. Create ext4 image ---
echo ""
echo "--- Creating ext4 rootfs image ($ROOTFS_SIZE) ---"
ROOTFS_IMG="$SNAPSHOT_DIR/rootfs.img"
truncate -s "$ROOTFS_SIZE" "$ROOTFS_IMG"
mkfs.ext4 -F "$ROOTFS_IMG"

MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$ROOTFS_IMG" "$MOUNT_DIR"
sudo tar -xf /tmp/fc-agent-rootfs.tar -C "$MOUNT_DIR"
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
rm -f /tmp/fc-agent-rootfs.tar

echo "Rootfs: $ROOTFS_IMG ($(du -sh "$ROOTFS_IMG" | cut -f1))"

# --- 5. Download kernel if not cached ---
KERNEL_FILE="$SNAPSHOT_DIR/kernel.bin"
if [ ! -f "$KERNEL_FILE" ]; then
  echo ""
  echo "--- Downloading kernel $KERNEL_VERSION ($ARCH) ---"
  curl -fsSL \
    "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14-def/${ARCH}/vmlinux-${KERNEL_VERSION}" \
    -o "$KERNEL_FILE"
fi
echo "Kernel: $KERNEL_FILE"

# --- 6. Create placeholder block device images ---
echo ""
echo "--- Creating placeholder block images ---"
for img_spec in "repo-cache-seed.img:64M" "repo-cache-upper.img:64M" "credentials.img:32M" "git-cache.img:64M"; do
  img_name="${img_spec%%:*}"
  img_size="${img_spec##*:}"
  img_path="$SNAPSHOT_DIR/$img_name"
  if [ ! -f "$img_path" ]; then
    truncate -s "$img_size" "$img_path"
    mkfs.ext4 -F "$img_path" > /dev/null 2>&1
    echo "  Created $img_name ($img_size)"
  else
    echo "  $img_name already exists, skipping"
  fi
done

echo ""
echo "=== Agent sandbox rootfs build complete ==="
echo "Contents of $SNAPSHOT_DIR:"
ls -lh "$SNAPSHOT_DIR/"
