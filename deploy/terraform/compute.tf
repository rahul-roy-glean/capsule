# Host VM Image
# When use_custom_host_image is false, use Ubuntu for initial deployment
# After building with Packer, set use_custom_host_image = true
data "google_compute_image" "host" {
  count   = var.use_custom_host_image ? 1 : 0
  family  = "firecracker-host"
  project = var.project_id
}

data "google_compute_image" "ubuntu" {
  family  = "ubuntu-2204-lts"
  project = "ubuntu-os-cloud"
}

locals {
  host_image = var.use_custom_host_image ? data.google_compute_image.host[0].self_link : data.google_compute_image.ubuntu.self_link
}


# Instance template for Firecracker hosts
resource "google_compute_instance_template" "firecracker_host" {
  name_prefix  = "${local.name_prefix}-host-"
  machine_type = var.host_machine_type
  region       = var.region

  tags = ["firecracker-host"]

  labels = local.labels

  # Required for NAT routing of microVM traffic
  can_ip_forward = true

  # Enable nested virtualization for Firecracker
  advanced_machine_features {
    enable_nested_virtualization = true
  }

  # Boot disk
  disk {
    source_image = local.host_image
    disk_type    = "pd-ssd"
    disk_size_gb = var.host_disk_size_gb
    boot         = true
    auto_delete  = true
  }

  # Data disk for snapshots and workspaces
  disk {
    device_name  = "data"
    type         = "PERSISTENT"
    disk_type    = "pd-ssd"
    disk_size_gb = var.host_data_disk_size_gb
    boot         = false
    auto_delete  = true
  }

  network_interface {
    subnetwork = google_compute_subnetwork.hosts.id
    # No external IP - egress via Cloud NAT
  }

  service_account {
    email  = google_service_account.host_agent.email
    scopes = ["cloud-platform"]
  }

  metadata = {
    snapshot-bucket       = google_storage_bucket.snapshots.name
    microvm-subnet        = var.microvm_subnet
    environment           = var.environment
    control-plane         = var.control_plane_addr
    github-app-id         = var.github_app_id
    github-app-secret     = var.github_app_secret
    github-repo           = var.github_repo
    max-runners             = var.max_runners_per_host
    idle-target           = var.idle_runners_target
    runner-ephemeral      = var.runner_ephemeral ? "true" : "false"
    use-chunked-snapshots  = var.use_chunked_snapshots ? "true" : "false"
    enable-session-chunks  = var.enable_session_chunks ? "true" : "false"
    chunk-cache-size-gb    = var.chunk_cache_size_gb
    mem-cache-size-gb      = var.mem_cache_size_gb
    use-netns              = var.use_netns ? "true" : "false"
    otel-collector-endpoint = var.otel_collector_addr
  }

  metadata_startup_script = <<-EOF
    #!/bin/bash
    set -e

    STARTUP_START=$(date +%s)
    echo "Starting Firecracker host initialization..."

    # Get key metadata
    SNAPSHOT_BUCKET=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/snapshot-bucket || echo "")

    # Wait for data disk (poll instead of fixed sleep)
    DATA_DISK="/dev/disk/by-id/google-data"
    for i in $(seq 1 30); do
      [ -b "$DATA_DISK" ] && break
      echo "Waiting for data disk... ($i/30)"
      sleep 2
    done

    # Mount the data disk
    mkdir -p /mnt/data

    if [ -b "$DATA_DISK" ]; then
      echo "Formatting data disk..."
      mkfs.xfs -f -L RUNNER_DATA "$DATA_DISK"
      mount "$DATA_DISK" /mnt/data
      echo "$DATA_DISK /mnt/data xfs defaults,nofail 0 0" >> /etc/fstab
    elif [ -b "/dev/sdb" ]; then
      mkfs.xfs -f -L RUNNER_DATA /dev/sdb
      mount /dev/sdb /mnt/data
      echo "/dev/sdb /mnt/data xfs defaults,nofail 0 0" >> /etc/fstab
    else
      echo "WARNING: No data disk found"
    fi

    # Create directories
    mkdir -p /mnt/data/snapshots

    # Download from GCS
    if [ -n "$SNAPSHOT_BUCKET" ]; then
      echo "Downloading Firecracker snapshot artifacts from GCS..."

      # Resolve versioned directory via pointer file (use curl + metadata API for speed)
      SNAPSHOT_VERSION=""
      TOKEN=$(curl -s -H "Metadata-Flavor: Google" http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])" 2>/dev/null || true)
      if [ -n "$TOKEN" ]; then
        SNAPSHOT_VERSION=$(curl -s -H "Authorization: Bearer $TOKEN" \
          "https://storage.googleapis.com/$SNAPSHOT_BUCKET/current-pointer.json" | \
          python3 -c "import sys,json; print(json.load(sys.stdin)['version'])" 2>/dev/null || true)
      fi
      if [ -z "$SNAPSHOT_VERSION" ]; then
        # Fallback to gcloud CLI
        POINTER_FILE=$(mktemp)
        if gcloud storage cp "gs://$SNAPSHOT_BUCKET/current-pointer.json" "$POINTER_FILE" 2>/dev/null; then
          SNAPSHOT_VERSION=$(python3 -c "import json; print(json.load(open('$POINTER_FILE'))['version'])" 2>/dev/null || true)
        fi
        rm -f "$POINTER_FILE"
      fi

      SNAPSHOT_DIR=""
      if [ -n "$SNAPSHOT_VERSION" ]; then
        echo "Resolved current pointer to version: $SNAPSHOT_VERSION"
        SNAPSHOT_DIR="gs://$SNAPSHOT_BUCKET/$SNAPSHOT_VERSION"
      else
        echo "No pointer file found, falling back to current/ directory"
        SNAPSHOT_DIR="gs://$SNAPSHOT_BUCKET/current"
      fi

      # Download snapshot artifacts (parallel composite download is faster than streaming)
      gcloud storage rsync -r "$SNAPSHOT_DIR/" /mnt/data/snapshots/ || true

      # Decompress snapshot.mem.zst in background while other setup continues
      if [ -f "/mnt/data/snapshots/snapshot.mem.zst" ] && [ ! -f "/mnt/data/snapshots/snapshot.mem" ]; then
        echo "Decompressing snapshot.mem.zst in background..."
        zstd -d /mnt/data/snapshots/snapshot.mem.zst -o /mnt/data/snapshots/snapshot.mem --no-progress &
        ZSTD_PID=$!
      fi
    fi

    # Create workspaces directory (per-VM, not in snapshot)
    mkdir -p /mnt/data/workspaces
    mkdir -p /var/run/firecracker

    # Setup bridge networking for microVMs
    echo "Setting up bridge networking..."
    # Get the host interface MTU (GCP uses 1460, not 1500)
    HOST_MTU=$(cat /sys/class/net/$(ip route | grep default | awk '{print $5}' | head -1)/mtu)
    ip link add fcbr0 type bridge || true
    ip link set fcbr0 mtu $HOST_MTU
    ip addr add ${cidrhost(var.microvm_subnet, 1)}/24 dev fcbr0 || true
    ip link set fcbr0 up
    echo "Bridge MTU set to $HOST_MTU (matching host interface)"

    # Enable IP forwarding
    echo 1 > /proc/sys/net/ipv4/ip_forward
    echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf

    # Setup NAT for microVM egress
    # Get the primary network interface
    PRIMARY_IFACE=$(ip route | grep default | awk '{print $5}' | head -1)
    iptables -t nat -A POSTROUTING -s ${var.microvm_subnet} -o "$PRIMARY_IFACE" -j MASQUERADE
    iptables -A FORWARD -i fcbr0 -o "$PRIMARY_IFACE" -j ACCEPT
    iptables -A FORWARD -i "$PRIMARY_IFACE" -o fcbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT

    # Clamp TCP MSS to path MTU - guest VMs may have MTU 1500 while GCP uses 1460.
    # Without this, large TCP segments get dropped after NAT (DF bit set, can't fragment).
    iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu

    # Save iptables rules
    iptables-save > /etc/iptables/rules.v4 || true

    # Load required kernel modules
    modprobe tun || true
    modprobe kvm_intel || modprobe kvm_amd || true

    # Set permissions for KVM
    chmod 666 /dev/kvm || true

    # Verify IP forwarding is enabled
    if [ "$(cat /proc/sys/net/ipv4/ip_forward)" != "1" ]; then
      echo "ERROR: IP forwarding not enabled"
      exit 1
    fi

    # Setup logrotate for Firecracker logs
    cat > /etc/logrotate.d/firecracker <<'LOGROTATE'
/var/log/firecracker/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
LOGROTATE

    # Get microVM configuration from metadata
    MAX_RUNNERS=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/max-runners || echo "16")
    IDLE_TARGET=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/idle-target || echo "2")
    RUNNER_EPHEMERAL=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/runner-ephemeral || echo "true")
    GITHUB_REPO=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/github-repo || echo "")
    CONTROL_PLANE=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/control-plane || echo "")

    USE_CHUNKED_SNAPSHOTS=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/use-chunked-snapshots || echo "false")
    USE_NETNS=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/use-netns || echo "false")

    # Stop firecracker-manager if already running (from Packer image auto-start)
    # This ensures the override is applied before the service runs
    systemctl stop firecracker-manager 2>/dev/null || true

    # Create systemd override for firecracker-manager with configured values
    mkdir -p /etc/systemd/system/firecracker-manager.service.d
    
    # Build the ExecStart line with optional flags
    EXEC_START="/usr/local/bin/firecracker-manager"
    EXEC_START="$EXEC_START --max-runners=$MAX_RUNNERS"
    EXEC_START="$EXEC_START --idle-target=$IDLE_TARGET"
    EXEC_START="$EXEC_START --runner-ephemeral=$RUNNER_EPHEMERAL"
    EXEC_START="$EXEC_START --snapshot-cache=/mnt/data/snapshots"
    EXEC_START="$EXEC_START --workspace-dir=/mnt/data/workspaces"

    # Add GitHub repo if configured
    if [ -n "$GITHUB_REPO" ]; then
      EXEC_START="$EXEC_START --github-repo=$GITHUB_REPO"
    fi

    # Add control plane if configured
    if [ -n "$CONTROL_PLANE" ]; then
      EXEC_START="$EXEC_START --control-plane=$CONTROL_PLANE"
    fi

    # Add chunked snapshot flag if enabled
    if [ "$USE_CHUNKED_SNAPSHOTS" = "true" ]; then
      EXEC_START="$EXEC_START --use-chunked-snapshots"
      EXEC_START="$EXEC_START --snapshot-bucket=$SNAPSHOT_BUCKET"
      CHUNK_CACHE_SIZE_GB=$(curl -sf -H "Metadata-Flavor: Google" \
        http://metadata.google.internal/computeMetadata/v1/instance/attributes/chunk-cache-size-gb || echo "2")
      MEM_CACHE_SIZE_GB=$(curl -sf -H "Metadata-Flavor: Google" \
        http://metadata.google.internal/computeMetadata/v1/instance/attributes/mem-cache-size-gb || echo "2")
      EXEC_START="$EXEC_START --chunk-cache-size-gb=$CHUNK_CACHE_SIZE_GB"
      EXEC_START="$EXEC_START --mem-cache-size-gb=$MEM_CACHE_SIZE_GB"

      ENABLE_SESSION_CHUNKS=$(curl -sf -H "Metadata-Flavor: Google" \
        http://metadata.google.internal/computeMetadata/v1/instance/attributes/enable-session-chunks || echo "false")
      if [ "$ENABLE_SESSION_CHUNKS" = "true" ]; then
        EXEC_START="$EXEC_START --enable-session-chunks"
      fi
    fi

    # Add network namespace flag if enabled
    if [ "$USE_NETNS" = "true" ]; then
      EXEC_START="$EXEC_START --use-netns"
    fi

    # Read OTel collector endpoint from metadata (empty = OTel disabled)
    OTEL_ENDPOINT=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/otel-collector-endpoint || echo "")
    ENVIRONMENT=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/environment || echo "dev")

    # Build environment lines for the systemd override
    ENV_LINES=""
    if [ -n "$OTEL_ENDPOINT" ]; then
      ENV_LINES="Environment=OTEL_EXPORTER_OTLP_ENDPOINT=$OTEL_ENDPOINT"
      ENV_LINES="$ENV_LINES\nEnvironment=ENVIRONMENT=$ENVIRONMENT"
    fi

    cat > /etc/systemd/system/firecracker-manager.service.d/override.conf << OVERRIDE
[Service]
ExecStart=
ExecStart=$EXEC_START
$([ -n "$ENV_LINES" ] && echo -e "$ENV_LINES")
OVERRIDE

    # Wait for background decompression before starting firecracker-manager
    if [ -n "$${ZSTD_PID:-}" ]; then
      echo "Waiting for snapshot.mem decompression to finish..."
      wait $ZSTD_PID && echo "snapshot.mem ready" || echo "WARNING: snapshot.mem decompression failed"
    fi

    # Reload and restart firecracker-manager service with new config
    echo "Starting firecracker-manager with: max-runners=$MAX_RUNNERS, idle-target=$IDLE_TARGET, vcpus=$VCPUS_PER_RUNNER, memory=$MEMORY_PER_RUNNER"
    systemctl daemon-reload
    systemctl enable firecracker-manager
    systemctl restart firecracker-manager

    STARTUP_END=$(date +%s)
    STARTUP_DURATION=$((STARTUP_END - STARTUP_START))
    echo "Firecracker host initialization complete in $${STARTUP_DURATION}s"
  EOF

  lifecycle {
    create_before_destroy = true
  }

  depends_on = [
    google_storage_bucket.snapshots,
    google_compute_subnetwork.hosts,
  ]
}

# Health check for host VMs
resource "google_compute_health_check" "host" {
  name                = "${local.name_prefix}-host-health"
  check_interval_sec  = 10
  timeout_sec         = 5
  healthy_threshold   = 2
  unhealthy_threshold = 3

  http_health_check {
    port         = 8080
    request_path = "/health"
  }
}

# Regional Managed Instance Group
resource "google_compute_region_instance_group_manager" "hosts" {
  name               = "${local.name_prefix}-hosts"
  base_instance_name = "${local.name_prefix}-host"
  region             = var.region

  version {
    instance_template = google_compute_instance_template.firecracker_host.id
  }

  target_size = var.min_hosts

  named_port {
    name = "grpc"
    port = 50051
  }

  named_port {
    name = "metrics"
    port = 9090
  }

  auto_healing_policies {
    health_check      = google_compute_health_check.host.id
    initial_delay_sec = 300
  }

  update_policy {
    type                           = "PROACTIVE"
    minimal_action                 = "REPLACE"
    most_disruptive_allowed_action = "REPLACE"
    max_surge_fixed                = 3
    max_unavailable_fixed          = 0
    replacement_method             = "SUBSTITUTE"
  }

  instance_lifecycle_policy {
    force_update_on_repair = "YES"
  }
}

# Autoscaler for host MIG
resource "google_compute_region_autoscaler" "hosts" {
  name   = "${local.name_prefix}-hosts-autoscaler"
  region = var.region
  target = google_compute_region_instance_group_manager.hosts.id

  autoscaling_policy {
    min_replicas    = var.min_hosts
    max_replicas    = var.max_hosts
    cooldown_period = 120
    # IMPORTANT: only scale out via the managed autoscaler. Scale-in should be
    # handled explicitly by the control plane so we never terminate hosts with
    # busy nested microVMs.
    mode = "ONLY_UP"

    # Scale based on free microVM slots per host, published by the control plane.
    # The control plane publishes fleet_free_slots_per_host every 30s based on
    # TotalSlots/UsedSlots reported via host heartbeats — independent of CI system.
    # Target=2 means: scale out when fewer than 2 free slots per host on average,
    # ensuring there is always headroom to accept new jobs without waiting for a
    # new host VM to boot.
    metric {
      name   = "custom.googleapis.com/firecracker/control_plane/fleet_free_slots_per_host"
      target = 2
      type   = "GAUGE"
    }

    # Also consider CPU utilization
    cpu_utilization {
      target = 0.7
    }
  }

  lifecycle {
    ignore_changes = [
      autoscaling_policy[0].mode,
    ]
  }
}


