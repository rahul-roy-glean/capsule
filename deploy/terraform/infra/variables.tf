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
  default     = "capsule"
}

variable "enable_monitoring" {
  description = "Enable GCP Cloud Monitoring dashboards and log-based metrics"
  type        = bool
  default     = true
}
