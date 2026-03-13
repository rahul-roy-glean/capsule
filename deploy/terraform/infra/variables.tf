variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "GCP region for resources"
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone for zonal resources"
  type        = string
  default     = "us-central1-c"
}

variable "environment" {
  description = "Environment name (dev, staging, prod)"
  type        = string
  default     = "dev"
}

variable "network_mode" {
  description = "Whether Terraform manages the VPC/subnets or reuses existing ones"
  type        = string
  default     = "managed"

  validation {
    condition     = contains(["managed", "existing"], var.network_mode)
    error_message = "network_mode must be either \"managed\" or \"existing\"."
  }
}

variable "existing_network_name" {
  description = "Name of an existing VPC network to reuse when network_mode is \"existing\""
  type        = string
  default     = ""

  validation {
    condition     = var.network_mode != "existing" || trimspace(var.existing_network_name) != ""
    error_message = "existing_network_name must be set when network_mode is \"existing\"."
  }
}

variable "existing_host_subnet_name" {
  description = "Name of an existing subnet for Capsule host VMs when network_mode is \"existing\""
  type        = string
  default     = ""

  validation {
    condition     = var.network_mode != "existing" || trimspace(var.existing_host_subnet_name) != ""
    error_message = "existing_host_subnet_name must be set when network_mode is \"existing\"."
  }
}

variable "existing_gke_subnet_name" {
  description = "Optional existing subnet for the GKE cluster when network_mode is \"existing\"; leave empty to create a dedicated Capsule GKE subnet in the reused VPC"
  type        = string
  default     = ""
}

variable "existing_gke_pods_secondary_range_name" {
  description = "Existing secondary range name to use for GKE pods when network_mode is \"existing\""
  type        = string
  default     = ""

  validation {
    condition     = var.network_mode != "existing" || trimspace(var.existing_gke_subnet_name) == "" || trimspace(var.existing_gke_pods_secondary_range_name) != ""
    error_message = "existing_gke_pods_secondary_range_name must be set when reusing an existing GKE subnet."
  }
}

variable "existing_gke_services_secondary_range_name" {
  description = "Existing secondary range name to use for GKE services when network_mode is \"existing\""
  type        = string
  default     = ""

  validation {
    condition     = var.network_mode != "existing" || trimspace(var.existing_gke_subnet_name) == "" || trimspace(var.existing_gke_services_secondary_range_name) != ""
    error_message = "existing_gke_services_secondary_range_name must be set when reusing an existing GKE subnet."
  }
}

variable "vpc_cidr" {
  description = "CIDR range for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

variable "gke_pods_cidr" {
  description = "Secondary CIDR for GKE pods"
  type        = string
  default     = "10.1.0.0/16"
}

variable "gke_services_cidr" {
  description = "Secondary CIDR for GKE services"
  type        = string
  default     = "10.2.0.0/16"
}

variable "gke_subnet_cidr" {
  description = "Primary CIDR for the Capsule GKE subnet; optional in managed mode, required when reusing a VPC but creating a dedicated GKE subnet"
  type        = string
  default     = ""

  validation {
    condition     = var.network_mode != "existing" || trimspace(var.existing_gke_subnet_name) != "" || trimspace(var.gke_subnet_cidr) != ""
    error_message = "gke_subnet_cidr must be set when reusing a VPC and creating a dedicated Capsule GKE subnet."
  }
}

variable "gke_master_ipv4_cidr_block" {
  description = "CIDR block for the private GKE control plane endpoint"
  type        = string
  default     = "172.16.0.0/28"
}

variable "manage_existing_network_nat" {
  description = "Whether Terraform should create Cloud NAT when reusing an existing network"
  type        = bool
  default     = false
}

variable "manage_existing_network_private_service_connection" {
  description = "Whether Terraform should reserve a private service networking range and connection when reusing an existing network"
  type        = bool
  default     = false
}

variable "private_service_range_name" {
  description = "Optional name for the private service networking range reservation"
  type        = string
  default     = ""
}

variable "private_service_range_address" {
  description = "Optional starting IP address for the reserved private service networking range"
  type        = string
  default     = ""
}

variable "private_service_range_prefix_length" {
  description = "Prefix length for the reserved private service networking range"
  type        = number
  default     = 16
}

variable "gke_node_machine_type" {
  description = "Machine type for GKE nodes"
  type        = string
  default     = "e2-standard-4"
}

variable "gke_min_nodes" {
  description = "Minimum nodes per zone in GKE"
  type        = number
  default     = 1
}

variable "gke_max_nodes" {
  description = "Maximum nodes per zone in GKE"
  type        = number
  default     = 3
}

variable "db_tier" {
  description = "Cloud SQL instance tier"
  type        = string
  default     = "db-custom-2-4096"
}

variable "db_password" {
  description = "Password for Cloud SQL postgres user"
  type        = string
  sensitive   = true
}

variable "container_registry_location" {
  description = "Location for Artifact Registry (e.g., us-central1, us, eu)"
  type        = string
  default     = "us-central1"
}

variable "container_registry_repo_name" {
  description = "Name of the Artifact Registry repository"
  type        = string
  default     = "firecracker"
}

variable "enable_monitoring" {
  description = "Enable GCP Cloud Monitoring dashboards and log-based metrics"
  type        = bool
  default     = true
}
