# GCP Cloud Monitoring configuration for Firecracker runners
# Log-based metrics only (infra stage)
#
# Dashboards and alert policies are in the app/ stage.

# Log-based metric for capsule-thaw-agent boot phases (runs inside VM)
resource "google_logging_metric" "vm_boot_phase_from_logs" {
  count       = var.enable_monitoring ? 1 : 0
  name        = "capsule/vm_boot_phase_from_logs"
  description = "VM boot phase durations from capsule-thaw-agent structured logs"
  filter      = <<-EOT
    resource.type="gce_instance"
    jsonPayload.event="boot_phase_complete"
  EOT

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "DISTRIBUTION"
    unit        = "ms"
    labels {
      key         = "phase"
      value_type  = "STRING"
      description = "Boot phase name"
    }
    labels {
      key         = "runner_id"
      value_type  = "STRING"
      description = "Runner ID"
    }
  }

  value_extractor = "EXTRACT(jsonPayload.duration_ms)"

  label_extractors = {
    "phase"     = "EXTRACT(jsonPayload.phase)"
    "runner_id" = "EXTRACT(jsonPayload.runner_id)"
  }

  bucket_options {
    exponential_buckets {
      num_finite_buckets = 64
      growth_factor      = 1.4
      scale              = 10
    }
  }
}

# Log-based metric for job completions
resource "google_logging_metric" "job_complete_from_logs" {
  count       = var.enable_monitoring ? 1 : 0
  name        = "capsule/job_complete_from_logs"
  description = "Job completion metrics from capsule-thaw-agent structured logs"
  filter      = <<-EOT
    resource.type="gce_instance"
    jsonPayload.event="job_complete"
  EOT

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "DISTRIBUTION"
    unit        = "ms"
    labels {
      key         = "repo"
      value_type  = "STRING"
      description = "Repository name"
    }
    labels {
      key         = "result"
      value_type  = "STRING"
      description = "Job result (success/failure)"
    }
  }

  value_extractor = "EXTRACT(jsonPayload.duration_ms)"

  label_extractors = {
    "repo"   = "EXTRACT(jsonPayload.repo)"
    "result" = "EXTRACT(jsonPayload.result)"
  }

  bucket_options {
    exponential_buckets {
      num_finite_buckets = 64
      growth_factor      = 1.4
      scale              = 1000 # Scale for milliseconds (job durations can be long)
    }
  }
}
