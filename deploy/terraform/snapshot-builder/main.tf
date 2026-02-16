terraform {
  required_version = ">= 1.0.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
  }

  # Use a separate state from main infra
  backend "gcs" {
    # Configure via backend config file or CLI flags
    # bucket = "your-terraform-state-bucket"
    # prefix = "snapshot-builder"
  }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

locals {
  name_prefix = "fc-runner-${var.environment}"

  labels = {
    environment = var.environment
    managed_by  = "terraform"
    project     = "firecracker-bazel-runner"
    component   = "snapshot-builder"
  }
}

data "google_compute_image" "ubuntu" {
  family  = "ubuntu-2204-lts"
  project = "ubuntu-os-cloud"
}

# Look up default compute engine SA when no explicit SA is provided
data "google_compute_default_service_account" "default" {
  count   = var.service_account_email == "" ? 1 : 0
  project = var.project_id
}

locals {
  service_account_email = var.service_account_email != "" ? var.service_account_email : data.google_compute_default_service_account.default[0].email
}

# IAM for snapshot builder to read secrets (GitHub App key)
resource "google_project_iam_member" "snapshot_builder_secrets" {
  project = var.project_id
  role    = "roles/secretmanager.secretAccessor"
  member  = "serviceAccount:${local.service_account_email}"
}

resource "google_compute_instance" "snapshot_builder" {
  name         = "${local.name_prefix}-snapshot-builder"
  machine_type = var.machine_type
  zone         = var.zone

  tags   = ["snapshot-builder"]
  labels = local.labels

  can_ip_forward = true

  # Enable nested virtualization for Firecracker
  advanced_machine_features {
    enable_nested_virtualization = true
  }

  boot_disk {
    initialize_params {
      image = data.google_compute_image.ubuntu.self_link
      type  = "pd-ssd"
      size  = var.disk_size_gb
    }
  }

  network_interface {
    subnetwork = var.subnet

    # External IP for GitHub/GCS egress
    access_config {}
  }

  service_account {
    email  = local.service_account_email
    scopes = ["cloud-platform"]
  }

  metadata = {
    snapshot-bucket     = var.snapshot_bucket
    repo-url            = var.repo_url
    repo-branch         = var.repo_branch
    bazel-version       = var.bazel_version
    fetch-targets       = var.fetch_targets
    github-app-id       = var.github_app_id
    github-app-secret   = var.github_app_secret
    gcp-project         = var.project_id
    debug-mode          = var.debug_mode ? "true" : "false"
    firecracker-version = var.firecracker_version
  }

  metadata_startup_script = file("${path.module}/startup.sh")

  depends_on = [
    google_project_iam_member.snapshot_builder_secrets,
  ]
}
