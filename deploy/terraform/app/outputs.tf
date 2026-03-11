output "control_plane_ip" {
  value       = local.control_plane_ip
  description = "Internal Load Balancer IP of the control plane service"
}

output "control_plane_addr" {
  value       = local.control_plane_addr
  description = "Control plane address (IP:port) for host VMs"
}

output "host_instance_group" {
  value       = google_compute_region_instance_group_manager.hosts.instance_group
  description = "Instance group for Firecracker hosts"
}

output "host_instance_group_manager_name" {
  value       = google_compute_region_instance_group_manager.hosts.name
  description = "Instance group manager name for Firecracker hosts"
}

output "otel_collector_addr" {
  value       = local.otel_collector_addr
  description = "OTel Collector OTLP endpoint (http://IP:4317) passed to host VMs, empty if disabled"
}

output "gke_get_credentials" {
  value       = "gcloud container clusters get-credentials ${local.infra.gke_cluster_name} --region ${local.infra.region} --project ${local.infra.project_id}"
  description = "Command to get GKE credentials"
}
