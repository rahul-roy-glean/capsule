# Host VM Image
# When use_custom_host_image is false, use Ubuntu for initial deployment
# After building with Packer, set use_custom_host_image = true
data "google_compute_image" "host" {
  count   = var.use_custom_host_image ? 1 : 0
  family  = "capsule-host"
  project = local.infra.project_id
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
  region       = local.infra.region

  tags = ["capsule-host"]

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
    subnetwork = local.infra.host_subnet_id
    # No external IP - egress via Cloud NAT
  }

  service_account {
    email  = local.infra.host_agent_email
    scopes = ["cloud-platform"]
  }

  metadata = {
    snapshot-bucket         = local.infra.snapshot_bucket
    microvm-subnet          = var.microvm_subnet
    environment             = local.infra.environment
    control-plane           = local.control_plane_addr
    host-bootstrap-token    = var.host_bootstrap_token
    max-runners             = var.max_runners_per_host
    idle-target             = var.idle_runners_target
    chunk-cache-size-gb     = var.chunk_cache_size_gb
    mem-cache-size-gb       = var.mem_cache_size_gb
    otel-collector-endpoint = local.otel_collector_addr
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

    # Create workspaces directory (per-VM, not in snapshot)
    mkdir -p /mnt/data/workspaces
    mkdir -p /var/run/firecracker

    # Enable IP forwarding
    echo 1 > /proc/sys/net/ipv4/ip_forward
    echo "net.ipv4.ip_forward = 1" >> /etc/sysctl.conf

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
    CONTROL_PLANE=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/control-plane || echo "")
    HOST_BOOTSTRAP_TOKEN=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/host-bootstrap-token || echo "")

    # Stop capsule-manager if already running (from Packer image auto-start)
    # This ensures the override is applied before the service runs
    systemctl stop capsule-manager 2>/dev/null || true

    # Create systemd override for capsule-manager with configured values
    mkdir -p /etc/systemd/system/capsule-manager.service.d

    # Build the ExecStart line with optional flags
    EXEC_START="/usr/local/bin/capsule-manager"
    EXEC_START="$EXEC_START --max-runners=$MAX_RUNNERS"
    EXEC_START="$EXEC_START --idle-target=$IDLE_TARGET"
    EXEC_START="$EXEC_START --snapshot-cache=/mnt/data/snapshots"
    EXEC_START="$EXEC_START --workspace-dir=/mnt/data/workspaces"

    # Add control plane if configured
    if [ -n "$CONTROL_PLANE" ]; then
      EXEC_START="$EXEC_START --control-plane=$CONTROL_PLANE"
    fi
    if [ -n "$HOST_BOOTSTRAP_TOKEN" ]; then
      EXEC_START="$EXEC_START --host-bootstrap-token=$HOST_BOOTSTRAP_TOKEN"
    fi

    # Add snapshot flags
    EXEC_START="$EXEC_START --snapshot-bucket=$SNAPSHOT_BUCKET"
    CHUNK_CACHE_SIZE_GB=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/chunk-cache-size-gb || echo "2")
    MEM_CACHE_SIZE_GB=$(curl -sf -H "Metadata-Flavor: Google" \
      http://metadata.google.internal/computeMetadata/v1/instance/attributes/mem-cache-size-gb || echo "2")
    EXEC_START="$EXEC_START --chunk-cache-size-gb=$CHUNK_CACHE_SIZE_GB"
    EXEC_START="$EXEC_START --mem-cache-size-gb=$MEM_CACHE_SIZE_GB"

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

    cat > /etc/systemd/system/capsule-manager.service.d/override.conf << OVERRIDE
[Service]
ExecStart=
ExecStart=$EXEC_START
$([ -n "$ENV_LINES" ] && echo -e "$ENV_LINES")
OVERRIDE

    # Reload and restart capsule-manager service with new config
    echo "Starting capsule-manager with: max-runners=$MAX_RUNNERS, idle-target=$IDLE_TARGET, vcpus=$VCPUS_PER_RUNNER, memory=$MEMORY_PER_RUNNER"
    systemctl daemon-reload
    systemctl enable capsule-manager
    systemctl restart capsule-manager

    STARTUP_END=$(date +%s)
    STARTUP_DURATION=$((STARTUP_END - STARTUP_START))
    echo "Firecracker host initialization complete in $${STARTUP_DURATION}s"
  EOF

  lifecycle {
    create_before_destroy = true
  }

  depends_on = [
    data.kubernetes_service.control_plane,
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
  region             = local.infra.region

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
