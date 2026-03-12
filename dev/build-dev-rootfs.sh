#!/bin/bash
# Build the minimal dev rootfs + kernel for local Firecracker testing.
# Usage: make dev-build
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SNAPSHOT_DIR="/tmp/fc-dev/snapshots"
IMAGE_NAME="fc-dev-rootfs"
ROOTFS_SIZE="512M"
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

echo "=== Building dev rootfs ==="
echo "Repo root: $REPO_ROOT"
echo "Output:    $SNAPSHOT_DIR"
echo "Arch:      $ARCH ($GOARCH)"

mkdir -p "$SNAPSHOT_DIR"

# --- 1. Build capsule-thaw-agent binary (for the Docker build context) ---
echo ""
echo "--- Building capsule-thaw-agent binary ---"
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/capsule-thaw-agent ./cmd/capsule-thaw-agent

# --- 2. Build Docker image ---
echo ""
echo "--- Building Docker image ---"
docker buildx build --platform "$DOCKER_PLATFORM" --load \
  -t "$IMAGE_NAME" \
  -f dev/Dockerfile.dev-rootfs .

# --- 3. Export rootfs as tar ---
echo ""
echo "--- Exporting rootfs ---"
CONTAINER_ID=$(docker create "$IMAGE_NAME")
docker export "$CONTAINER_ID" > /tmp/fc-dev-rootfs.tar
docker rm "$CONTAINER_ID"

# --- 4. Create ext4 image ---
echo ""
echo "--- Creating ext4 rootfs image ($ROOTFS_SIZE) ---"
ROOTFS_IMG="$SNAPSHOT_DIR/rootfs.img"
truncate -s "$ROOTFS_SIZE" "$ROOTFS_IMG"
mkfs.ext4 -F "$ROOTFS_IMG"

MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$ROOTFS_IMG" "$MOUNT_DIR"
sudo tar -xf /tmp/fc-dev-rootfs.tar -C "$MOUNT_DIR"
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
rm -f /tmp/fc-dev-rootfs.tar

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

echo ""
echo "=== Dev rootfs build complete ==="
echo "Contents of $SNAPSHOT_DIR:"
ls -lh "$SNAPSHOT_DIR/"
