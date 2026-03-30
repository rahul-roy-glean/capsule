variable "infra_state_bucket" {
  description = "GCS bucket containing the infra stage Terraform state"
  type        = string
}

variable "infra_state_prefix" {
  description = "GCS prefix for the infra stage Terraform state"
  type        = string
}

variable "db_password" {
  description = "Password for Cloud SQL postgres user (used for K8s secret)"
  type        = string
  sensitive   = true
}

variable "github_webhook_secret" {
  description = "GitHub webhook secret for control plane"
  type        = string
  default     = ""
  sensitive   = true
}

variable "host_bootstrap_token" {
  description = "Shared bearer token used by host VMs when sending authenticated heartbeats to the control plane"
  type        = string
  default     = ""
  sensitive   = true
}

variable "control_plane_image_tag" {
  description = "Docker image tag for the control plane"
  type        = string
  default     = "latest"
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
  default     = 1
}

variable "max_hosts" {
  description = "Maximum number of host VMs in MIG"
  type        = number
  default     = 20
}

variable "use_custom_host_image" {
  description = "Whether to use the custom Packer-built host image. Stock Ubuntu is bootstrap-only and must keep max_hosts = 0 until finalize."
  type        = bool
  default     = false
}

variable "microvm_subnet" {
  description = "Subnet CIDR for microVM NAT networking"
  type        = string
  default     = "172.16.0.0/24"
}

variable "chunk_cache_size_gb" {
  description = "Size in GB of the on-disk LRU chunk cache for FUSE-backed disks"
  type        = number
  default     = 0
}

variable "mem_cache_size_gb" {
  description = "Size in GB of the in-memory LRU chunk cache for UFFD page fault handling"
  type        = number
  default     = 0
}

variable "host_log_level" {
  description = "Log level for capsule-manager on host VMs (debug, info, warn, error)"
  type        = string
  default     = "info"
}

variable "enable_otel_collector" {
  description = "Deploy a standalone OTel Collector in GKE with an internal LB. Its IP is automatically passed to host VMs."
  type        = bool
  default     = true
}

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

variable "alert_snapshot_age_threshold_hours" {
  description = "Alert when active snapshot is older than this many hours"
  type        = number
  default     = 48
}

variable "alert_queue_depth_threshold" {
  description = "Alert when job queue depth exceeds this threshold"
  type        = number
  default     = 10
}

variable "autoscaler_scale_up_threshold" {
  description = "CPU utilization threshold (0-1) above which to scale up hosts"
  type        = string
  default     = "0.9"
}

variable "autoscaler_scale_down_threshold" {
  description = "CPU utilization threshold (0-1) below which to scale down hosts"
  type        = string
  default     = "0.5"
}

variable "autoscaler_cooldown" {
  description = "Minimum time between autoscale actions (Go duration, e.g. 5m)"
  type        = string
  default     = "5m"
}

variable "autoscaler_boot_cooldown" {
  description = "Cooldown after demand-driven scale-up to wait for host boot (Go duration)"
  type        = string
  default     = "3m"
}

variable "autoscaler_rate_window" {
  description = "Sliding window for allocation rate tracking (Go duration)"
  type        = string
  default     = "60s"
}

variable "autoscaler_settling_threshold" {
  description = "Minimum utilization (0-1) before a host counts for scale-down decisions"
  type        = string
  default     = "0.2"
}

variable "autoscaler_min_host_age" {
  description = "Minimum age before a host can be drained (Go duration)"
  type        = string
  default     = "10m"
}