locals {
  use_existing_network              = var.network_mode == "existing"
  use_existing_gke_subnet           = local.use_existing_network && trimspace(var.existing_gke_subnet_name) != ""
  gke_subnet_primary_cidr           = trimspace(var.gke_subnet_cidr) != "" ? var.gke_subnet_cidr : cidrsubnet(var.vpc_cidr, 4, 1)
  manage_cloud_nat                  = !local.use_existing_network || var.manage_existing_network_nat
  manage_private_service_connection = !local.use_existing_network || var.manage_existing_network_private_service_connection
}

data "google_compute_network" "existing" {
  count = local.use_existing_network ? 1 : 0
  name  = var.existing_network_name
}

data "google_compute_subnetwork" "existing_hosts" {
  count  = local.use_existing_network ? 1 : 0
  name   = var.existing_host_subnet_name
  region = var.region
}

data "google_compute_subnetwork" "existing_gke" {
  count  = local.use_existing_gke_subnet ? 1 : 0
  name   = var.existing_gke_subnet_name
  region = var.region
}

# VPC Network
resource "google_compute_network" "main" {
  count                   = local.use_existing_network ? 0 : 1
  name                    = "${local.name_prefix}-vpc"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"

  depends_on = [google_project_service.apis]
}

# Main subnet for host VMs
resource "google_compute_subnetwork" "hosts" {
  count         = local.use_existing_network ? 0 : 1
  name          = "${local.name_prefix}-hosts"
  ip_cidr_range = cidrsubnet(var.vpc_cidr, 4, 0) # /20 for hosts
  region        = var.region
  network       = google_compute_network.main[0].id

  private_ip_google_access = true

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

# Subnet for GKE cluster
resource "google_compute_subnetwork" "gke" {
  count         = local.use_existing_gke_subnet ? 0 : 1
  name          = "${local.name_prefix}-gke"
  ip_cidr_range = local.gke_subnet_primary_cidr
  region        = var.region
  network       = local.use_existing_network ? data.google_compute_network.existing[0].id : google_compute_network.main[0].id

  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = var.gke_pods_cidr
  }

  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = var.gke_services_cidr
  }
}

locals {
  network_name      = local.use_existing_network ? data.google_compute_network.existing[0].name : google_compute_network.main[0].name
  network_id        = local.use_existing_network ? data.google_compute_network.existing[0].id : google_compute_network.main[0].id
  network_self_link = local.use_existing_network ? data.google_compute_network.existing[0].self_link : google_compute_network.main[0].self_link

  host_subnet_name = local.use_existing_network ? data.google_compute_subnetwork.existing_hosts[0].name : google_compute_subnetwork.hosts[0].name
  host_subnet_id   = local.use_existing_network ? data.google_compute_subnetwork.existing_hosts[0].id : google_compute_subnetwork.hosts[0].id
  host_subnet_cidr = local.use_existing_network ? data.google_compute_subnetwork.existing_hosts[0].ip_cidr_range : google_compute_subnetwork.hosts[0].ip_cidr_range

  gke_subnet_name = local.use_existing_gke_subnet ? data.google_compute_subnetwork.existing_gke[0].name : google_compute_subnetwork.gke[0].name
  gke_subnet_id   = local.use_existing_gke_subnet ? data.google_compute_subnetwork.existing_gke[0].id : google_compute_subnetwork.gke[0].id
  gke_subnet_cidr = local.use_existing_gke_subnet ? data.google_compute_subnetwork.existing_gke[0].ip_cidr_range : google_compute_subnetwork.gke[0].ip_cidr_range

  gke_pods_secondary_range_name     = local.use_existing_gke_subnet ? var.existing_gke_pods_secondary_range_name : "pods"
  gke_services_secondary_range_name = local.use_existing_gke_subnet ? var.existing_gke_services_secondary_range_name : "services"
  gke_secondary_ranges = local.use_existing_gke_subnet ? {
    for range in data.google_compute_subnetwork.existing_gke[0].secondary_ip_range : range.range_name => range.ip_cidr_range
    } : {
    for range in google_compute_subnetwork.gke[0].secondary_ip_range : range.range_name => range.ip_cidr_range
  }

  gke_pods_cidr     = lookup(local.gke_secondary_ranges, local.gke_pods_secondary_range_name, null)
  gke_services_cidr = lookup(local.gke_secondary_ranges, local.gke_services_secondary_range_name, null)

  internal_source_ranges = distinct(compact([
    local.host_subnet_cidr,
    local.gke_subnet_cidr,
    local.gke_pods_cidr,
  ]))

  nat_subnet_ids = distinct([
    local.host_subnet_id,
    local.gke_subnet_id,
  ])
}

# Cloud Router for NAT
resource "google_compute_router" "main" {
  count   = local.manage_cloud_nat ? 1 : 0
  name    = "${local.name_prefix}-router"
  region  = var.region
  network = local.network_id
}

# Cloud NAT for Capsule host and GKE subnet egress
resource "google_compute_router_nat" "main" {
  count                              = local.manage_cloud_nat ? 1 : 0
  name                               = "${local.name_prefix}-nat"
  router                             = google_compute_router.main[0].name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "LIST_OF_SUBNETWORKS"

  dynamic "subnetwork" {
    for_each = toset(local.nat_subnet_ids)

    content {
      name                    = subnetwork.value
      source_ip_ranges_to_nat = ["ALL_IP_RANGES"]
    }
  }

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# Firewall rules

# Allow internal communication between Capsule subnets and pods
resource "google_compute_firewall" "internal" {
  name    = "${local.name_prefix}-allow-internal"
  network = local.network_name

  allow {
    protocol = "icmp"
  }

  allow {
    protocol = "tcp"
    ports    = ["0-65535"]
  }

  allow {
    protocol = "udp"
    ports    = ["0-65535"]
  }

  source_ranges = local.internal_source_ranges
}

# Allow SSH from IAP
resource "google_compute_firewall" "iap_ssh" {
  name    = "${local.name_prefix}-allow-iap-ssh"
  network = local.network_name

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }

  source_ranges = ["35.235.240.0/20"] # IAP IP range
  target_tags   = ["capsule-host"]
}

# Allow health checks from GCP
resource "google_compute_firewall" "health_checks" {
  name    = "${local.name_prefix}-allow-health-checks"
  network = local.network_name

  allow {
    protocol = "tcp"
    ports    = ["8080", "9090"] # API and metrics ports
  }

  source_ranges = [
    "130.211.0.0/22", # GCP health check IPs
    "35.191.0.0/16",  # GCP health check IPs
  ]

  target_tags = ["capsule-host"]
}

# Allow gRPC from GKE to hosts
resource "google_compute_firewall" "grpc" {
  name    = "${local.name_prefix}-allow-grpc"
  network = local.network_name

  allow {
    protocol = "tcp"
    ports    = ["50051"] # gRPC port
  }

  source_ranges = compact([local.gke_pods_cidr])
  target_tags   = ["capsule-host"]

  lifecycle {
    precondition {
      condition     = local.gke_pods_cidr != null
      error_message = "The selected GKE subnet must already contain the configured pods secondary range."
    }
  }
}

# Private service connection for Cloud SQL
resource "google_compute_global_address" "private_ip_range" {
  count         = local.manage_private_service_connection ? 1 : 0
  name          = trimspace(var.private_service_range_name) != "" ? var.private_service_range_name : "${local.name_prefix}-private-ip"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  address       = trimspace(var.private_service_range_address) != "" ? var.private_service_range_address : null
  prefix_length = var.private_service_range_prefix_length
  network       = local.network_id
}

resource "google_service_networking_connection" "private_vpc_connection" {
  count                   = local.manage_private_service_connection ? 1 : 0
  network                 = local.network_id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_ip_range[0].name]

  depends_on = [google_project_service.apis]
}

# --- Access Plane VPC Peering (Cross-Project) ---
# Enables microVMs on host VMs to reach tenant access planes deployed in a
# separate GCP project/VPC (e.g. a dedicated access-plane GKE cluster).
#
# Cross-project peering requirements:
#   1. This side: creates peering from capsule VPC → access-plane VPC (below)
#   2. Remote side: must create matching peering from access-plane VPC → capsule VPC
#      using vpc_network_self_link output from this module
#   3. IAM: the access-plane project must grant compute.networkPeering.create
#      or roles/compute.networkAdmin to the identity running this Terraform
#      (typically handled by the remote project's Terraform)
#
# Set access_plane_vpc_network to the full self-link:
#   projects/{ACCESS_PLANE_PROJECT}/global/networks/{NETWORK_NAME}

resource "google_compute_network_peering" "capsule_to_access_plane" {
  count        = var.access_plane_vpc_peering_enabled ? 1 : 0
  name         = "${local.name_prefix}-to-access-plane"
  network      = local.network_self_link
  peer_network = var.access_plane_vpc_network

  export_custom_routes = true
  import_custom_routes = true

  lifecycle {
    precondition {
      condition     = trimspace(var.access_plane_vpc_network) != ""
      error_message = "access_plane_vpc_network must be set when access_plane_vpc_peering_enabled is true."
    }
  }
}

# Allow microVM traffic (NAT'd through hosts) to reach access plane ports
resource "google_compute_firewall" "access_plane_egress" {
  count   = var.access_plane_vpc_peering_enabled ? 1 : 0
  name    = "${local.name_prefix}-allow-access-plane"
  network = local.network_name

  allow {
    protocol = "tcp"
    ports    = ["8080", "3128"] # HTTP API + CONNECT proxy
  }

  # Host subnet is the source (microVM traffic is NAT'd through the host)
  source_ranges = [local.host_subnet_cidr]
  direction     = "EGRESS"
}
