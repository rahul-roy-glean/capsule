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
  default     = "us-central1-a"
}

variable "environment" {
  description = "Environment name (dev, staging, prod)"
  type        = string
  default     = "dev"
}

variable "host_machine_type" {
  description = "Machine type for Firecracker host VMs"
  type        = string
  default     = "n2-standard-64"
}

variable "host_disk_size_gb" {
  description = "Boot disk size for host VMs in GB (OS + binaries only)"
  type        = number
  default     = 50
}

variable "host_data_disk_size_gb" {
  description = "Data disk size for snapshots/workspaces in GB (pd-ssd)"
  type        = number
  default     = 500
}

variable "min_hosts" {
  description = "Minimum number of host VMs in MIG"
  type        = number
  default     = 2
}

variable "max_hosts" {
  description = "Maximum number of host VMs in MIG"
  type        = number
  default     = 20
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

variable "microvm_subnet" {
  description = "Subnet CIDR for microVM NAT networking"
  type        = string
  default     = "172.16.0.0/24"
}

variable "control_plane_addr" {
  description = "Control plane address reachable from host VMs (e.g. internal LB DNS/IP:8080)"
  type        = string
  default     = ""
}

variable "vpc_cidr" {
  description = "CIDR range for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

variable "admin_cidrs" {
  description = "Additional CIDR blocks allowed to reach the GKE control plane API."
  type        = list(string)
  default     = []
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

variable "use_custom_host_image" {
  description = "Whether to use the custom Packer-built host image. Set to false for initial deployment, then true after building with Packer."
  type        = bool
  default     = false
}

variable "github_app_id" {
  description = "GitHub App ID for generating installation tokens (for private repos)"
  type        = string
  default     = ""
}

variable "github_app_secret" {
  description = "Secret Manager secret name containing GitHub App private key (for private repos)"
  type        = string
  default     = ""
}

variable "github_repo" {
  description = "GitHub repository for runner registration (e.g., 'askscio/scio')"
  type        = string
  default     = ""
}

# MicroVM configuration per host
variable "max_runners_per_host" {
  description = "Maximum number of microVMs (runners) per host"
  type        = number
  default     = 4
}

variable "idle_runners_target" {
  description = "Target number of idle runners to maintain per host"
  type        = number
  default     = 2
}

variable "use_chunked_snapshots" {
  description = "Enable chunked snapshot restore with UFFD (lazy memory) and FUSE (lazy disk). Requires chunked metadata in the snapshot bucket."
  type        = bool
  default     = true
}

variable "enable_session_chunks" {
  description = "Enable cloud-backed session pause/resume. Uses snapshot bucket for chunk storage. When enabled, PauseRunner uploads chunks to GCS and ResumeFromSession fetches lazily via UFFD+FUSE."
  type        = bool
  default     = true
}

variable "otel_collector_addr" {
  description = "OpenTelemetry Collector OTLP gRPC endpoint reachable from host VMs (e.g. internal LB IP:4317). Leave empty to disable OTel on hosts."
  type        = string
  default     = ""
}

variable "chunk_cache_size_gb" {
  description = "Size in GB of the on-disk LRU chunk cache for FUSE-backed disks. Larger values improve cache hit ratio and reduce GCS fetches."
  type        = number
  default     = 2
}

variable "mem_cache_size_gb" {
  description = "Size in GB of the in-memory LRU chunk cache for UFFD page fault handling. Larger values reduce GCS fetches for memory pages."
  type        = number
  default     = 2
}

variable "use_netns" {
  description = "Use per-VM network namespaces instead of a shared bridge. Provides VM-to-VM isolation by construction — each VM gets its own namespace with point-to-point veth routing."
  type        = bool
  default     = true
}


# Container Registry configuration
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

# Monitoring configuration
variable "enable_monitoring" {
  description = "Enable GCP Cloud Monitoring dashboards and log-based metrics"
  type        = bool
  default     = true
}

variable "enable_monitoring_alerts" {
  description = "Enable GCP Cloud Monitoring alert policies (requires enable_monitoring=true)"
  type        = bool
  default     = false
}

variable "otel_collector_endpoint" {
  description = "OpenTelemetry Collector OTLP/gRPC endpoint reachable from host VMs (e.g. 10.0.16.17:4317). Empty = OTel disabled (no-op)."
  type        = string
  default     = ""
}

variable "monitoring_notification_channels" {
  description = "List of notification channel IDs for alerts (e.g., Slack, PagerDuty)"
  type        = list(string)
  default     = []
}

variable "alert_vm_boot_threshold_seconds" {
  description = "Alert when VM boot p95 exceeds this threshold in seconds"
  type        = number
  default     = 10
}

variable "alert_queue_depth_threshold" {
  description = "Alert when job queue depth exceeds this threshold"
  type        = number
  default     = 50
}

variable "alert_snapshot_age_threshold_hours" {
  description = "Alert when active snapshot is older than this many hours"
  type        = number
  default     = 48
}

# CI system configuration
variable "ci_system" {
  description = "CI system integration (github-actions, none). Controls runner registration and webhook handling."
  type        = string
  default     = "github-actions"

  validation {
    condition     = contains(["github-actions", "none"], var.ci_system)
    error_message = "ci_system must be one of: github-actions, none"
  }
}

# GitHub organization for org-level runner registration
variable "github_org" {
  description = "GitHub organization for org-level runner registration. If set, uses org-level API instead of repo-level."
  type        = string
  default     = ""
}

# Snapshot automation configuration
variable "enable_snapshot_automation" {
  description = "Enable Cloud Scheduler for automated snapshot freshness checks and rebuild triggers"
  type        = bool
  default     = false
}

variable "snapshot_freshness_schedule" {
  description = "Cron schedule for snapshot freshness checks (Cloud Scheduler format)"
  type        = string
  default     = "0 */4 * * *"
}

variable "snapshot_max_age_hours" {
  description = "Maximum snapshot age in hours before triggering rebuild"
  type        = number
  default     = 24
}

variable "snapshot_max_commit_drift" {
  description = "Maximum number of commits behind HEAD before triggering rebuild"
  type        = number
  default     = 50
}
