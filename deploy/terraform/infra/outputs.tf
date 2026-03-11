output "project_id" {
  value       = var.project_id
  description = "GCP project ID"
}

output "region" {
  value       = var.region
  description = "GCP region"
}

output "zone" {
  value       = var.zone
  description = "GCP zone"
}

output "environment" {
  value       = var.environment
  description = "Environment name"
}

output "name_prefix" {
  value       = local.name_prefix
  description = "Resource name prefix (fc-runner-{env})"
}

output "labels" {
  value       = local.labels
  description = "Common resource labels"
}

output "gke_endpoint" {
  value       = google_container_cluster.control_plane.endpoint
  description = "GKE cluster endpoint"
  sensitive   = true
}

output "gke_ca_certificate" {
  value       = google_container_cluster.control_plane.master_auth[0].cluster_ca_certificate
  description = "GKE cluster CA certificate (base64-encoded)"
  sensitive   = true
}

output "gke_cluster_name" {
  value       = google_container_cluster.control_plane.name
  description = "GKE cluster name"
}

output "host_subnet_id" {
  value       = google_compute_subnetwork.hosts.id
  description = "Host subnet ID for instance templates"
}

output "host_subnet_name" {
  value       = google_compute_subnetwork.hosts.name
  description = "Host subnet name for builder VMs"
}

output "host_agent_email" {
  value       = google_service_account.host_agent.email
  description = "Service account email for host VMs"
}

output "control_plane_service_account" {
  value       = google_service_account.control_plane.email
  description = "Service account email for control plane"
}

output "snapshot_builder_service_account" {
  value       = google_service_account.snapshot_builder.email
  description = "Service account email for snapshot builder"
}

output "db_private_ip" {
  value       = google_sql_database_instance.main.private_ip_address
  description = "Private IP address of Cloud SQL instance"
}

output "db_connection_name" {
  value       = google_sql_database_instance.main.connection_name
  description = "Cloud SQL connection name for Cloud SQL Proxy"
}

output "snapshot_bucket" {
  value       = google_storage_bucket.snapshots.name
  description = "GCS bucket name for Firecracker snapshots"
}

output "snapshot_bucket_url" {
  value       = google_storage_bucket.snapshots.url
  description = "GCS bucket URL for Firecracker snapshots"
}

output "container_registry" {
  value       = "${google_artifact_registry_repository.containers.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.containers.repository_id}"
  description = "Artifact Registry URL for container images"
}

output "control_plane_image" {
  value       = "${google_artifact_registry_repository.containers.location}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.containers.repository_id}/control-plane"
  description = "Full image path for control-plane container"
}

output "vpc_network" {
  value       = google_compute_network.main.name
  description = "VPC network name"
}

output "host_instance_group_manager_name" {
  value       = "${local.name_prefix}-hosts"
  description = "Pre-computed MIG manager name for the host fleet"
}
