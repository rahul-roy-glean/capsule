#!/bin/bash
# Install prerequisites for local Firecracker development on a Linux host.
# Run as root: sudo bash dev/setup-linux.sh
#
# Installs Go, Firecracker, Docker, Postgres, and configures KVM on
# Ubuntu/Debian Linux hosts.
set -eux

export DEBIAN_FRONTEND=noninteractive

ARCH=$(uname -m)  # x86_64 or aarch64

# --- Check KVM ---
if [ ! -e /dev/kvm ]; then
  echo "ERROR: /dev/kvm not found. Enable nested virtualization or use a bare-metal host."
  exit 1
fi
chmod 666 /dev/kvm

# --- Core packages ---
apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates curl wget gnupg lsb-release \
  e2fsprogs qemu-utils \
  bridge-utils iptables iproute2 ipset \
  jq git make build-essential \
  docker.io docker-buildx \
  postgresql postgresql-client

# --- Go 1.24 ---
GO_VERSION=1.24.0
if [ ! -d /usr/local/go ]; then
  case "$ARCH" in
    x86_64)  GO_ARCH=amd64 ;;
    aarch64) GO_ARCH=arm64 ;;
    *)       GO_ARCH=amd64 ;;
  esac
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" | tar -C /usr/local -xzf -
fi
echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/golang.sh
export PATH=/usr/local/go/bin:$PATH

# --- Firecracker 1.14.2 ---
FC_VERSION=1.14.2
if [ ! -f /usr/local/bin/firecracker ]; then
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-${ARCH}.tgz" \
    -o /tmp/fc.tgz
  tar -xzf /tmp/fc.tgz -C /tmp
  mv "/tmp/release-v${FC_VERSION}-${ARCH}/firecracker-v${FC_VERSION}-${ARCH}" /usr/local/bin/firecracker
  mv "/tmp/release-v${FC_VERSION}-${ARCH}/jailer-v${FC_VERSION}-${ARCH}" /usr/local/bin/jailer
  chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer
  rm -rf /tmp/fc.tgz /tmp/release-*
fi

# --- Networking ---
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-ip-forward.conf

# --- Postgres: create dev database ---
systemctl enable postgresql
systemctl start postgresql
su - postgres -c "psql -tc \"SELECT 1 FROM pg_database WHERE datname='capsule'\" | grep -q 1 || psql -c 'CREATE DATABASE capsule'"
# Allow local connections without password
PG_HBA=$(su - postgres -c "psql -t -A -c 'SHOW hba_file'")
if ! grep -q 'local.*all.*all.*trust' "$PG_HBA" 2>/dev/null; then
  sed -i 's/^local\s\+all\s\+all\s\+peer/local   all   all   trust/' "$PG_HBA"
  sed -i 's/^host\s\+all\s\+all\s\+127.0.0.1\/32\s\+\(scram-sha-256\|md5\)/host    all   all   127.0.0.1\/32   trust/' "$PG_HBA"
  systemctl reload postgresql
fi

# --- Docker: allow current user ---
if [ -n "${SUDO_USER:-}" ]; then
  usermod -aG docker "$SUDO_USER" 2>/dev/null || true
fi

# --- Dev directories ---
mkdir -p /tmp/fc-dev
if ! mountpoint -q /tmp/fc-dev; then
  mount --bind /tmp/fc-dev /tmp/fc-dev
fi
mkdir -p /tmp/fc-dev/{snapshots/overlays,sockets,logs,workspaces,pids}
chmod -R 777 /tmp/fc-dev

echo ""
echo "=== Setup complete ==="
echo "  KVM:         $(ls -la /dev/kvm)"
echo "  Firecracker: $(firecracker --version 2>&1 | head -1)"
echo "  Go:          $(go version)"
echo "  Postgres:    $(pg_lsclusters -h | head -1)"
echo "  Docker:      $(docker --version)"
echo ""
echo "Next steps:"
echo "  make dev-build                 # Build binaries + rootfs"
echo "  make dev-test-snapshot-builder # Run snapshot-builder smoke tests"
echo "  make dev-stack                 # Start control-plane + capsule-manager"
