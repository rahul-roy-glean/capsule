output "instance_name" {
  description = "Name of the snapshot builder VM"
  value       = google_compute_instance.snapshot_builder.name
}

output "instance_zone" {
  description = "Zone of the snapshot builder VM"
  value       = google_compute_instance.snapshot_builder.zone
}

output "external_ip" {
  description = "External IP of snapshot builder VM (for SSH)"
  value       = google_compute_instance.snapshot_builder.network_interface[0].access_config[0].nat_ip
}

output "ssh_command" {
  description = "gcloud SSH command for the snapshot builder VM"
  value       = "gcloud compute ssh ${google_compute_instance.snapshot_builder.name} --zone=${google_compute_instance.snapshot_builder.zone} --tunnel-through-iap"
}
