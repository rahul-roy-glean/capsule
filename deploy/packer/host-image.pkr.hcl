packer {
  required_plugins {
    googlecompute = {
      source  = "github.com/hashicorp/googlecompute"
      version = "~> 1.1"
    }
  }
}

variable "project_id" {
  type        = string
  description = "GCP project ID"
}

variable "zone" {
  type        = string
  default     = "us-central1-a"
  description = "GCP zone for building the image"
}

variable "firecracker_version" {
  type        = string
  default     = "1.14.2"
  description = "Firecracker version to install"
}

variable "image_family" {
  type        = string
  default     = "capsule-host"
  description = "Image family name"
}

variable "network" {
  type        = string
  default     = ""
  description = "VPC network to use for building (empty = default)"
}

variable "subnetwork" {
  type        = string
  default     = ""
  description = "Subnetwork to use for building (empty = default)"
}

variable "capsule_manager_binary" {
  type        = string
  default     = ""
  description = "Local path to capsule-manager binary to upload"
}

variable "service_account_email" {
  type        = string
  default     = ""
  description = "Service account attached to the temporary packer build VM"
}

source "googlecompute" "capsule-host" {
  project_id              = var.project_id
  zone                    = var.zone
  source_image_family     = "ubuntu-2204-lts"
  source_image_project_id = ["ubuntu-os-cloud"]

  machine_type = "n2-standard-4"
  disk_size    = 50
  disk_type    = "pd-ssd"

  image_name        = "capsule-host-{{timestamp}}"
  image_family      = var.image_family
  image_description = "Firecracker host image with KVM support"

  ssh_username          = "ubuntu"
  service_account_email = var.service_account_email
  scopes                = ["https://www.googleapis.com/auth/cloud-platform"]

  network    = var.network
  subnetwork = var.subnetwork
  tags       = ["capsule-host"]

  # Use IAP tunnel for SSH (requires firewall rule for 35.235.240.0/20 -> port 22)
  use_iap    = true

  # Enable nested virtualization for Firecracker
  enable_nested_virtualization = true

  metadata = {
    enable-oslogin = "FALSE"
  }
}

build {
  sources = ["source.googlecompute.capsule-host"]

  # Update system and install base packages
  provisioner "shell" {
    environment_vars = [
      "DEBIAN_FRONTEND=noninteractive"
    ]
    inline = [
      "set -o errexit -o nounset -o xtrace",
      "sudo apt-get update",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get upgrade -y",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y curl wget gnupg2 software-properties-common apt-transport-https ca-certificates jq git xfsprogs"
    ]
  }

  # Install Cloud Ops Agent for monitoring and logging
  provisioner "shell" {
    inline = [
      "curl -sSO https://dl.google.com/cloudagents/add-google-cloud-ops-agent-repo.sh",
      "sudo bash add-google-cloud-ops-agent-repo.sh --also-install",
      "sudo systemctl enable google-cloud-ops-agent"
    ]
  }

  # Install KVM and virtualization tools
  provisioner "shell" {
    inline = [
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-kvm bridge-utils",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y linux-headers-$(uname -r) || true",
      "sudo modprobe kvm",
      "sudo modprobe kvm_intel || sudo modprobe kvm_amd || true"
    ]
  }

  # Install networking tools (pre-seed debconf to avoid interactive prompts)
  provisioner "shell" {
    inline = [
      "echo iptables-persistent iptables-persistent/autosave_v4 boolean true | sudo debconf-set-selections",
      "echo iptables-persistent iptables-persistent/autosave_v6 boolean true | sudo debconf-set-selections",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y iptables iptables-persistent iproute2 net-tools dnsmasq-base bridge-utils"
    ]
  }

  # Install Firecracker
  provisioner "shell" {
    inline = [
      "ARCH=x86_64",
      "curl -fsSL https://github.com/firecracker-microvm/firecracker/releases/download/v${var.firecracker_version}/firecracker-v${var.firecracker_version}-$${ARCH}.tgz | sudo tar -xz -C /tmp",
      "sudo mv /tmp/release-v${var.firecracker_version}-$${ARCH}/firecracker-v${var.firecracker_version}-$${ARCH} /usr/local/bin/firecracker",
      "sudo mv /tmp/release-v${var.firecracker_version}-$${ARCH}/jailer-v${var.firecracker_version}-$${ARCH} /usr/local/bin/jailer",
      "sudo rm -rf /tmp/release-v${var.firecracker_version}-$${ARCH}",
      "sudo chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer",
      "firecracker --version"
    ]
  }

  # Download guest kernel (5.10 - required for entropy device support)
  provisioner "shell" {
    inline = [
      "sudo mkdir -p /opt/firecracker",
      "sudo curl -fsSL -o /opt/firecracker/kernel.bin https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.14-def/x86_64/vmlinux-5.10.242",
      "echo 'Guest kernel 5.10.242 installed at /opt/firecracker/kernel.bin'"
    ]
  }

  # Install Google Cloud SDK
  provisioner "shell" {
    inline = [
      "curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg",
      "echo 'deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main' | sudo tee /etc/apt/sources.list.d/google-cloud-sdk.list",
      "sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y google-cloud-cli"
    ]
  }

  # Install Docker (needed for layer builder --base-image rootfs builds)
  provisioner "shell" {
    inline = [
      "curl -fsSL https://get.docker.com | sudo sh",
      "sudo systemctl enable docker"
    ]
  }

  # Install qemu-img for overlay creation
  provisioner "shell" {
    inline = [
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-utils"
    ]
  }

  # Create directories
  provisioner "shell" {
    inline = [
      "sudo mkdir -p /var/run/firecracker",
      "sudo mkdir -p /var/log/firecracker",
      "sudo mkdir -p /mnt/data/snapshots",
      "sudo mkdir -p /mnt/data/workspaces",
      "sudo mkdir -p /mnt/data/git-cache",
      "sudo mkdir -p /opt/capsule-manager",
      "sudo mkdir -p /etc/iptables"
    ]
  }

  # Upload capsule-manager binary from local build
  provisioner "file" {
    source      = var.capsule_manager_binary
    destination = "/tmp/capsule-manager"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/capsule-manager /usr/local/bin/capsule-manager",
      "sudo chmod +x /usr/local/bin/capsule-manager",
      "capsule-manager --version || echo 'capsule-manager installed (no --version flag)'"
    ]
  }

  # Create capsule-manager systemd service
  provisioner "shell" {
    inline = [
      "cat <<'EOF' | sudo tee /etc/systemd/system/capsule-manager.service",
      "[Unit]",
      "Description=Firecracker Manager",
      "After=network.target",
      "Wants=network.target",
      "",
      "[Service]",
      "Type=simple",
      "ExecStart=/usr/local/bin/capsule-manager",
      "Restart=always",
      "RestartSec=5",
      "Environment=LOG_LEVEL=info",
      "",
      "[Install]",
      "WantedBy=multi-user.target",
      "EOF",
      "sudo systemctl daemon-reload"
    ]
  }

  # Configure sysctl for networking
  provisioner "shell" {
    inline = [
      "echo 'net.ipv4.ip_forward = 1' | sudo tee /etc/sysctl.d/99-firecracker.conf",
      "echo 'net.bridge.bridge-nf-call-iptables = 0' | sudo tee -a /etc/sysctl.d/99-firecracker.conf",
      "echo 'net.bridge.bridge-nf-call-ip6tables = 0' | sudo tee -a /etc/sysctl.d/99-firecracker.conf"
    ]
  }

  # Configure KVM permissions
  provisioner "shell" {
    inline = [
      "echo 'KERNEL==\"kvm\", GROUP=\"kvm\", MODE=\"0666\"' | sudo tee /etc/udev/rules.d/99-kvm.rules"
    ]
  }

  # Disable unnecessary services to speed up boot (~35s savings)
  provisioner "shell" {
    inline = [
      "sudo systemctl disable snap.lxd.activate.service || true",
      "sudo systemctl disable snapd.service snapd.socket snapd.seeded.service || true",
      "sudo systemctl disable apport.service || true",
      "sudo systemctl mask snap.lxd.activate.service snapd.service snapd.socket snapd.seeded.service apport.service || true",
      "sudo apt-get purge -y snapd lxd-agent-loader apport || true",
      "sudo apt-get autoremove -y"
    ]
  }

  # Cleanup
  provisioner "shell" {
    inline = [
      "sudo apt-get clean",
      "sudo rm -rf /var/lib/apt/lists/*",
      "sudo rm -rf /tmp/*",
      "sudo rm -rf /var/tmp/*"
    ]
  }
}
