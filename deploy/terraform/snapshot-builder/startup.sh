#!/bin/bash
set -euo pipefail

echo "=== Snapshot Builder VM startup ==="

# Helper to read instance metadata
meta() {
  curl -sf -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" || echo "$2"
}

SNAPSHOT_BUCKET=$(meta snapshot-bucket "")
FIRECRACKER_VERSION=$(meta firecracker-version "1.14.1")
DEBUG_MODE=$(meta debug-mode "false")
REPO_URL=$(meta repo-url "")
REPO_BRANCH=$(meta repo-branch "main")
BAZEL_VERSION=$(meta bazel-version "8.5.1")
FETCH_TARGETS=$(meta fetch-targets "//...")
GITHUB_APP_ID=$(meta github-app-id "")
GITHUB_APP_SECRET=$(meta github-app-secret "")
GCP_PROJECT=$(meta gcp-project "")

# ---- 1. Install Firecracker ----
echo "Installing Firecracker v${FIRECRACKER_VERSION}..."
ARCH=$(uname -m)
FC_URL="https://github.com/firecracker-microvm/firecracker/releases/download/v${FIRECRACKER_VERSION}/firecracker-v${FIRECRACKER_VERSION}-${ARCH}.tgz"
cd /tmp
curl -fSL "$FC_URL" -o firecracker.tgz
tar xzf firecracker.tgz
mv "release-v${FIRECRACKER_VERSION}-${ARCH}/firecracker-v${FIRECRACKER_VERSION}-${ARCH}" /usr/local/bin/firecracker
chmod +x /usr/local/bin/firecracker
rm -rf firecracker.tgz "release-v${FIRECRACKER_VERSION}-${ARCH}"
echo "Firecracker installed: $(firecracker --version)"

# ---- 2. Load KVM modules and set permissions ----
echo "Setting up KVM..."
modprobe kvm_intel || modprobe kvm_amd || true
chmod 666 /dev/kvm || true
echo "KVM ready: $(ls -la /dev/kvm)"

# ---- 3. Download kernel + rootfs from GCS ----
echo "Downloading kernel and rootfs from GCS..."
mkdir -p /opt/firecracker
if [ -n "$SNAPSHOT_BUCKET" ]; then
  # Try build-artifacts first, fall back to current/
  gcloud storage cp "gs://${SNAPSHOT_BUCKET}/build-artifacts/kernel.bin" /opt/firecracker/kernel.bin 2>/dev/null \
    || gcloud storage cp "gs://${SNAPSHOT_BUCKET}/current/kernel.bin" /opt/firecracker/kernel.bin 2>/dev/null \
    || echo "WARNING: kernel.bin not found in GCS"

  gcloud storage cp "gs://${SNAPSHOT_BUCKET}/build-artifacts/rootfs.img" /opt/firecracker/rootfs.img 2>/dev/null \
    || gcloud storage cp "gs://${SNAPSHOT_BUCKET}/current/rootfs.img" /opt/firecracker/rootfs.img 2>/dev/null \
    || echo "WARNING: rootfs.img not found in GCS"
else
  echo "WARNING: No snapshot bucket configured, skipping kernel/rootfs download"
fi
echo "Firecracker artifacts:"
ls -lah /opt/firecracker/

# ---- 4. Download snapshot-builder binary from GCS ----
echo "Downloading snapshot-builder binary..."
if [ -n "$SNAPSHOT_BUCKET" ]; then
  gcloud storage cp "gs://${SNAPSHOT_BUCKET}/build-artifacts/snapshot-builder" /usr/local/bin/snapshot-builder 2>/dev/null \
    || echo "WARNING: snapshot-builder binary not found in GCS"
  chmod +x /usr/local/bin/snapshot-builder 2>/dev/null || true
fi

# ---- 5. Setup networking: bridge, IP forwarding, NAT, MTU clamping ----
echo "Setting up bridge networking..."
HOST_MTU=$(cat /sys/class/net/$(ip route | grep default | awk '{print $5}' | head -1)/mtu)
ip link add fcbr0 type bridge || true
ip link set fcbr0 mtu "$HOST_MTU"
ip addr add 172.16.0.1/24 dev fcbr0 || true
ip link set fcbr0 up
echo "Bridge MTU set to $HOST_MTU (matching host interface)"

# Enable IP forwarding
echo 1 > /proc/sys/net/ipv4/ip_forward
echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf

# Setup NAT
PRIMARY_IFACE=$(ip route | grep default | awk '{print $5}' | head -1)
iptables -t nat -A POSTROUTING -s 172.16.0.0/24 -o "$PRIMARY_IFACE" -j MASQUERADE
iptables -A FORWARD -i fcbr0 -o "$PRIMARY_IFACE" -j ACCEPT
iptables -A FORWARD -i "$PRIMARY_IFACE" -o fcbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT

# Clamp TCP MSS to path MTU (GCP uses 1460, guests may assume 1500)
iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu

mkdir -p /etc/iptables
iptables-save > /etc/iptables/rules.v4 || true

# Load tun module
modprobe tun || true

# ---- 6/7. Run or prepare snapshot-builder ----
if [ "$DEBUG_MODE" = "true" ]; then
  echo "=== DEBUG MODE: Not auto-running snapshot build ==="
  echo "SSH in and run: run-snapshot-build"

  cat > /usr/local/bin/run-snapshot-build << 'SCRIPT'
#!/bin/bash
set -euo pipefail

meta() {
  curl -sf -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1" || echo "$2"
}

SNAPSHOT_BUCKET=$(meta snapshot-bucket "")
REPO_URL=$(meta repo-url "")
REPO_BRANCH=$(meta repo-branch "main")
BAZEL_VERSION=$(meta bazel-version "8.5.1")
FETCH_TARGETS=$(meta fetch-targets "//...")
GITHUB_APP_ID=$(meta github-app-id "")
GITHUB_APP_SECRET=$(meta github-app-secret "")
GCP_PROJECT=$(meta gcp-project "")

CMD=(/usr/local/bin/snapshot-builder
  -kernel-path=/opt/firecracker/kernel.bin
  -rootfs-path=/opt/firecracker/rootfs.img
  -firecracker-bin=/usr/local/bin/firecracker
  "-gcs-bucket=$SNAPSHOT_BUCKET"
)

[ -n "$REPO_URL" ]          && CMD+=("-repo-url=$REPO_URL")
[ -n "$REPO_BRANCH" ]       && CMD+=("-repo-branch=$REPO_BRANCH")
[ -n "$BAZEL_VERSION" ]     && CMD+=("-bazel-version=$BAZEL_VERSION")
[ -n "$FETCH_TARGETS" ]     && CMD+=("-fetch-targets=$FETCH_TARGETS")
[ -n "$GITHUB_APP_ID" ]     && CMD+=("-github-app-id=$GITHUB_APP_ID")
[ -n "$GITHUB_APP_SECRET" ] && CMD+=("-github-app-secret=$GITHUB_APP_SECRET")
[ -n "$GCP_PROJECT" ]       && CMD+=("-gcp-project=$GCP_PROJECT")

echo "Running: ${CMD[*]}"
exec "${CMD[@]}"
SCRIPT
  chmod +x /usr/local/bin/run-snapshot-build
else
  echo "=== AUTO MODE: Running snapshot build ==="
  CMD=(/usr/local/bin/snapshot-builder
    -kernel-path=/opt/firecracker/kernel.bin
    -rootfs-path=/opt/firecracker/rootfs.img
    -firecracker-bin=/usr/local/bin/firecracker
    "-gcs-bucket=$SNAPSHOT_BUCKET"
  )

  [ -n "$REPO_URL" ]          && CMD+=("-repo-url=$REPO_URL")
  [ -n "$REPO_BRANCH" ]       && CMD+=("-repo-branch=$REPO_BRANCH")
  [ -n "$BAZEL_VERSION" ]     && CMD+=("-bazel-version=$BAZEL_VERSION")
  [ -n "$FETCH_TARGETS" ]     && CMD+=("-fetch-targets=$FETCH_TARGETS")
  [ -n "$GITHUB_APP_ID" ]     && CMD+=("-github-app-id=$GITHUB_APP_ID")
  [ -n "$GITHUB_APP_SECRET" ] && CMD+=("-github-app-secret=$GITHUB_APP_SECRET")
  [ -n "$GCP_PROJECT" ]       && CMD+=("-gcp-project=$GCP_PROJECT")

  echo "Running: ${CMD[*]}"
  "${CMD[@]}"
fi

echo "=== Snapshot Builder VM startup complete ==="
