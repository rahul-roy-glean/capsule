# Alert policies for Firecracker runners (app stage)
#
# Dashboards and log-based metrics are in the infra/ stage.

locals {
  metric_prefix = "workload.googleapis.com"
  cp_service    = "control-plane"
  mgr_service   = "firecracker-manager"
}

# Alert: VM Boot Time Too High
resource "google_monitoring_alert_policy" "vm_boot_slow" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker VM Boot Time > ${var.alert_vm_boot_threshold_seconds}s"
  combiner     = "OR"

  conditions {
    display_name = "VM boot p95 above threshold"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_vm_boot_threshold_seconds
      duration        = "300s"
      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_DELTA"
        cross_series_reducer = "REDUCE_SUM"
      }
      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_PERCENTILE_95"
        cross_series_reducer = "REDUCE_MEAN"
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "VM boot time p95 exceeded ${var.alert_vm_boot_threshold_seconds} seconds. Check snapshot health and host resource availability."
    mime_type = "text/markdown"
  }

  alert_strategy {
    auto_close = "1800s"
  }
}

# Alert: No Idle Runners
resource "google_monitoring_alert_policy" "no_idle_runners" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker No Idle Runners Available"
  combiner     = "OR"

  conditions {
    display_name = "No idle runners for 5 minutes"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/control_plane.runners.idle\" AND metric.label.service_name=\"${local.cp_service}\""
      comparison      = "COMPARISON_LT"
      threshold_value = 1
      duration        = "300s"
      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_MAX"
        cross_series_reducer = "REDUCE_SUM"
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "No idle runners available. Jobs will queue. Consider scaling up hosts."
    mime_type = "text/markdown"
  }
}

# Alert: High Queue Depth
resource "google_monitoring_alert_policy" "high_queue_depth" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker High Queue Depth"
  combiner     = "OR"

  conditions {
    display_name = "Queue depth above threshold"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/control_plane.queue.depth\" AND metric.label.service_name=\"${local.cp_service}\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_queue_depth_threshold
      duration        = "300s"
      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_MAX"
        cross_series_reducer = "REDUCE_SUM"
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "Job queue depth exceeded ${var.alert_queue_depth_threshold}. Jobs are waiting for runners. Consider scaling up."
    mime_type = "text/markdown"
  }
}

# Alert: Snapshot Age Too Old
resource "google_monitoring_alert_policy" "snapshot_stale" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker Snapshot Too Old"
  combiner     = "OR"

  conditions {
    display_name = "Snapshot age above threshold"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/snapshot.age\" AND metric.label.service_name=\"${local.cp_service}\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_snapshot_age_threshold_hours * 3600
      duration        = "3600s"
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_MAX"
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "Active snapshot is older than ${var.alert_snapshot_age_threshold_hours} hours. Consider building a new snapshot."
    mime_type = "text/markdown"
  }
}

# Alert: Host Unhealthy
resource "google_monitoring_alert_policy" "host_unhealthy" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker Host Unhealthy"
  combiner     = "OR"

  conditions {
    display_name = "Host heartbeat missing"
    condition_absent {
      filter   = "metric.type=\"${local.metric_prefix}/host.heartbeat.latency\" AND metric.label.service_name=\"${local.mgr_service}\""
      duration = "300s"
      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_MEAN"
        group_by_fields    = ["metric.label.host_id"]
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "A host has not sent a heartbeat in 5 minutes. Check host health and connectivity."
    mime_type = "text/markdown"
  }
}

# Alert: Chunk Cache Hit Ratio Low
resource "google_monitoring_alert_policy" "chunk_cache_low_hit_ratio" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker Chunk Cache Hit Ratio Low"
  combiner     = "OR"

  conditions {
    display_name = "Chunk cache hit ratio below 50%"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/chunked.cache_hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
      comparison      = "COMPARISON_LT"
      threshold_value = 0.5
      duration        = "300s"
      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_MEAN"
        cross_series_reducer = "REDUCE_MEAN"
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "Chunk cache hit ratio dropped below 50%. VMs are fetching most chunks from GCS, increasing page fault latency. Consider increasing chunk cache size or investigating access patterns."
    mime_type = "text/markdown"
  }

  alert_strategy {
    auto_close = "1800s"
  }
}

# E2E Canary Alert
resource "google_monitoring_alert_policy" "e2e_canary_failures" {
  count        = var.enable_monitoring ? 1 : 0
  display_name = "E2E Canary Consecutive Failures"
  project      = local.infra.project_id
  combiner     = "OR"

  conditions {
    display_name = "E2E canary failures > 3 in 1h"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/e2e.canary.failure\" AND metric.label.service_name=\"${local.cp_service}\""
      duration        = "3600s"
      comparison      = "COMPARISON_GT"
      threshold_value = 3
      aggregations {
        alignment_period   = "900s"
        per_series_aligner = "ALIGN_DELTA"
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "E2E canary health check has failed more than 3 times in the last hour. Check the self-hosted runner infrastructure."
    mime_type = "text/markdown"
  }
}
