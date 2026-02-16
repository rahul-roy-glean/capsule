variable "project_id" {
  description = "GCP project ID"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "us-central1"
}

variable "zone" {
  description = "GCP zone for the snapshot builder VM"
  type        = string
  default     = "us-central1-a"
}

variable "environment" {
  description = "Environment name (dev, staging, prod)"
  type        = string
  default     = "dev"
}

variable "machine_type" {
  description = "Machine type for snapshot builder VM (needs nested virt + enough RAM for Bazel)"
  type        = string
  default     = "n2-standard-8"
}

variable "disk_size_gb" {
  description = "Boot disk size in GB"
  type        = number
  default     = 200
}

variable "subnet" {
  description = "Subnet for snapshot builder VM"
  type        = string
  default     = "default"
}

variable "snapshot_bucket" {
  description = "GCS bucket name for snapshots (from main infra output)"
  type        = string
}

variable "service_account_email" {
  description = "Service account email for the snapshot builder (from main infra output)"
  type        = string
}

variable "repo_url" {
  description = "Repository URL for snapshot warmup"
  type        = string
  default     = ""
}

variable "repo_branch" {
  description = "Repository branch for snapshot warmup"
  type        = string
  default     = "main"
}

variable "bazel_version" {
  description = "Bazel version for snapshot warmup"
  type        = string
  default     = "8.5.1"
}

variable "fetch_targets" {
  description = "Bazel fetch target pattern"
  type        = string
  default     = "//..."
}

variable "github_app_id" {
  description = "GitHub App ID for generating installation tokens (for private repos)"
  type        = string
  default     = ""
}

variable "github_app_secret" {
  description = "Secret Manager secret name containing GitHub App private key"
  type        = string
  default     = ""
}

variable "debug_mode" {
  description = "When true, VM starts but does NOT auto-run snapshot build (SSH in to run manually)"
  type        = bool
  default     = false
}

variable "firecracker_version" {
  description = "Firecracker binary version (must match across snapshot-builder and hosts)"
  type        = string
  default     = "1.14.1"
}
