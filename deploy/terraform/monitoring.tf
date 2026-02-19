# GCP Cloud Monitoring configuration for Firecracker runners
# This creates dashboards and alerting policies

locals {
  metric_prefix = "custom.googleapis.com/firecracker"
}

# Dashboard for Firecracker Runner Overview
resource "google_monitoring_dashboard" "firecracker_overview" {
  count = var.enable_monitoring ? 1 : 0
  dashboard_json = jsonencode({
    displayName = "Firecracker Runner Overview"
    labels = {
      environment = var.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Row 1: High-level stats
        {
          width  = 3
          height = 2
          widget = {
            title = "Active Hosts"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane/hosts_total\" resource.type=\"global\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 3
          width  = 3
          height = 2
          widget = {
            title = "Active Runners"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane/runners_total\" resource.type=\"global\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 6
          width  = 3
          height = 2
          widget = {
            title = "Queue Depth"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane/queue_depth\" resource.type=\"global\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 9
          width  = 3
          height = 2
          widget = {
            title = "Snapshot Age (hours)"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/snapshot/age_seconds\" resource.type=\"global\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        # Row 2: VM Boot Time (from host-side allocation timer)
        {
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "VM Boot Duration (p50, p95, p99)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/boot_duration_seconds\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_50"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p50"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/boot_duration_seconds\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_95"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p95"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/boot_duration_seconds\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_99"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p99"
                }
              ]
              yAxis = {
                label = "seconds"
              }
            }
          }
        },
        # VM Allocations (success/failure)
        {
          yPos   = 2
          xPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "VM Allocations"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/allocations_total\" resource.type=\"gce_instance\" metric.label.result=\"success\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Success"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/allocations_total\" resource.type=\"gce_instance\" metric.label.result=\"failure\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Failure"
                }
              ]
              yAxis = {
                label = "allocations/min"
              }
            }
          }
        },
        # Row 3: Host Utilization
        {
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Host Slot Utilization"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host/slots_used\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Used Slots"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host/slots_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Total Slots"
                }
              ]
              yAxis = {
                label = "slots"
              }
            }
          }
        },
        # Runner States
        {
          yPos   = 6
          xPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Runner States"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host/runners_idle\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Idle"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host/runners_busy\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Busy"
                }
              ]
              yAxis = {
                label = "runners"
              }
            }
          }
        },
        # Row 4: Chunk Cache + Pool Performance
        {
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Chunk Cache Hit Ratio"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/chunked/cache_hit_ratio\" resource.type=\"gce_instance\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_MEAN"
                    }
                  }
                }
                legendTemplate = "Hit Ratio"
              }]
              yAxis = {
                label = "ratio (0-1)"
              }
            }
          }
        },
        # Pool Hit Ratio
        {
          yPos   = 10
          xPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Runner Pool Hit Ratio"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/pool/hit_ratio\" resource.type=\"gce_instance\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_MEAN"
                    }
                  }
                }
                legendTemplate = "Hit Ratio"
              }]
              yAxis = {
                label = "ratio (0-1)"
              }
            }
          }
        }
      ]
    }
  })
}

# Dashboard for VM Boot Phases (debugging)
# Boot phase data comes from log-based metrics (thaw-agent runs inside the VM
# without Cloud Monitoring API credentials, so it emits structured JSON logs
# that are extracted by the log-based metric defined below).
resource "google_monitoring_dashboard" "vm_boot_phases" {
  count = var.enable_monitoring ? 1 : 0
  dashboard_json = jsonencode({
    displayName = "Firecracker VM Boot Phases"
    labels = {
      environment = var.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        {
          width  = 12
          height = 6
          widget = {
            title = "Boot Phase Duration by Phase (p50)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/firecracker/vm_boot_phase_from_logs\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_50"
                        crossSeriesReducer = "REDUCE_MEAN"
                        groupByFields      = ["metric.label.phase"]
                      }
                    }
                  }
                  legendTemplate = "$${metric.label.phase}"
                }
              ]
              yAxis = {
                label = "ms"
              }
            }
          }
        },
        {
          yPos   = 6
          width  = 12
          height = 6
          widget = {
            title = "Boot Phase Distribution (Stacked)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"logging.googleapis.com/user/firecracker/vm_boot_phase_from_logs\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_PERCENTILE_50"
                        groupByFields      = ["metric.label.phase"]
                      }
                    }
                  }
                  legendTemplate = "$${metric.label.phase}"
                  plotType       = "STACKED_AREA"
                }
              ]
              yAxis = {
                label = "ms"
              }
            }
          }
        }
      ]
    }
  })
}

# Alert: VM Boot Time Too High
resource "google_monitoring_alert_policy" "vm_boot_slow" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker VM Boot Time > ${var.alert_vm_boot_threshold_seconds}s"
  combiner     = "OR"

  conditions {
    display_name = "VM boot p95 above threshold"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/vm/boot_duration_seconds\" resource.type=\"gce_instance\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_vm_boot_threshold_seconds
      duration        = "300s"
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
      filter          = "metric.type=\"${local.metric_prefix}/host/runners_idle\" resource.type=\"gce_instance\""
      comparison      = "COMPARISON_LT"
      threshold_value = 1
      duration        = "300s"
      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_MEAN"
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
      filter          = "metric.type=\"${local.metric_prefix}/control_plane/queue_depth\" resource.type=\"global\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_queue_depth_threshold
      duration        = "300s"
      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_MEAN"
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
      filter          = "metric.type=\"${local.metric_prefix}/snapshot/age_seconds\" resource.type=\"global\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.alert_snapshot_age_threshold_hours * 3600
      duration        = "3600s"
      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_MEAN"
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
      filter   = "metric.type=\"${local.metric_prefix}/host/heartbeat_latency_seconds\" resource.type=\"gce_instance\""
      duration = "300s"
      aggregations {
        alignment_period   = "60s"
        per_series_aligner = "ALIGN_MEAN"
        group_by_fields    = ["resource.label.instance_id"]
      }
    }
  }

  notification_channels = var.monitoring_notification_channels

  documentation {
    content   = "A host has not sent a heartbeat in 5 minutes. Check host health and connectivity."
    mime_type = "text/markdown"
  }
}

# ============================================================================
# Operations Dashboard (snapshot automation & fleet health)
# ============================================================================

resource "google_monitoring_dashboard" "firecracker_operations" {
  count   = var.enable_monitoring ? 1 : 0
  project = var.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Runner Operations"
    labels = {
      environment = var.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Fleet overview
        {
          width  = 6
          height = 4
          widget = {
            title = "Active Hosts"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane/hosts_total\" resource.type=\"global\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Total Runners"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane/runners_total\" resource.type=\"global\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        # VM Boot Time
        {
          yPos   = 4
          width  = 6
          height = 4
          widget = {
            title = "VM Boot Duration (p50/p95)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/boot_duration_seconds\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_50"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p50"
                  plotType       = "LINE"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/boot_duration_seconds\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_95"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p95"
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "seconds" }
            }
          }
        },
        # Idle vs Busy Runners
        {
          xPos   = 6
          yPos   = 4
          width  = 6
          height = 4
          widget = {
            title = "Idle vs Busy Runners"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host/runners_idle\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Idle"
                  plotType       = "STACKED_AREA"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host/runners_busy\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Busy"
                  plotType       = "STACKED_AREA"
                }
              ]
              yAxis = { label = "runners" }
            }
          }
        },
        # Snapshot Age
        {
          yPos   = 8
          width  = 6
          height = 4
          widget = {
            title = "Snapshot Age"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/snapshot/age_seconds\" resource.type=\"global\""
                    aggregation = {
                      alignmentPeriod  = "300s"
                      perSeriesAligner = "ALIGN_MEAN"
                    }
                  }
                }
                legendTemplate = "Age"
                plotType       = "LINE"
              }]
              yAxis = { label = "seconds" }
            }
          }
        },
        # VM Allocations
        {
          xPos   = 6
          yPos   = 8
          width  = 6
          height = 4
          widget = {
            title = "VM Allocations (Success/Failure)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/allocations_total\" resource.type=\"gce_instance\" metric.label.result=\"success\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Success"
                  plotType       = "STACKED_BAR"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/vm/allocations_total\" resource.type=\"gce_instance\" metric.label.result=\"failure\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Failure"
                  plotType       = "STACKED_BAR"
                }
              ]
              yAxis = { label = "allocations/5min" }
            }
          }
        }
      ]
    }
  })
}

# ============================================================================
# Chunked Snapshot & Pool Dashboard
# ============================================================================

resource "google_monitoring_dashboard" "chunked_snapshot" {
  count   = var.enable_monitoring ? 1 : 0
  project = var.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Chunked Snapshots & Pool"
    labels = {
      environment = var.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Row 1: Chunk cache scorecards
        {
          width  = 3
          height = 2
          widget = {
            title = "Chunk Cache Hit Ratio"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked/cache_hit_ratio\" resource.type=\"gce_instance\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 3
          width  = 3
          height = 2
          widget = {
            title = "Page Faults Total"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked/page_faults_total\" resource.type=\"gce_instance\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MEAN"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 6
          width  = 3
          height = 2
          widget = {
            title = "Pool Hit Ratio"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/pool/hit_ratio\" resource.type=\"gce_instance\""
                  aggregation = {
                    alignmentPeriod  = "60s"
                    perSeriesAligner = "ALIGN_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 9
          width  = 3
          height = 2
          widget = {
            title = "Pooled Runners"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/pool/runners\" resource.type=\"gce_instance\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MEAN"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        # Row 2: Cache hit ratio over time + page faults rate
        {
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "Chunk Cache Hit Ratio Over Time"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/chunked/cache_hit_ratio\" resource.type=\"gce_instance\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_MEAN"
                    }
                  }
                }
                legendTemplate = "Hit Ratio"
                plotType       = "LINE"
              }]
              yAxis = { label = "ratio (0-1)" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "UFFD Page Faults & Chunk Fetches"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/page_faults_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Page Faults"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/chunk_fetches_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Chunk Fetches (GCS)"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/cache_hits_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Cache Hits"
                }
              ]
              yAxis = { label = "count/min" }
            }
          }
        },
        # Row 3: FUSE disk I/O + cache size
        {
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "FUSE Disk I/O"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/disk_reads_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Reads"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/disk_writes_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Writes"
                }
              ]
              yAxis = { label = "ops/min" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Chunk Cache Size"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/cache_size_bytes\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Used"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked/cache_max_size_bytes\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Max"
                }
              ]
              yAxis = { label = "bytes" }
            }
          }
        },
        # Row 4: Pool hit ratio over time + pool hits/misses
        {
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Pool Hit Ratio Over Time"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/pool/hit_ratio\" resource.type=\"gce_instance\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_MEAN"
                    }
                  }
                }
                legendTemplate = "Hit Ratio"
                plotType       = "LINE"
              }]
              yAxis = { label = "ratio (0-1)" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Pool Hits vs Misses vs Evictions"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool/hits_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Hits"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool/misses_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Misses"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool/evictions_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Evictions"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool/recycle_failures_total\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Recycle Failures"
                }
              ]
              yAxis = { label = "count/min" }
            }
          }
        },
        # Row 5: Pool memory usage + dirty chunks
        {
          yPos   = 14
          width  = 6
          height = 4
          widget = {
            title = "Pool Memory Usage"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool/memory_used_bytes\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Used"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool/memory_max_bytes\" resource.type=\"gce_instance\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Max"
                }
              ]
              yAxis = { label = "bytes" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 14
          width  = 6
          height = 4
          widget = {
            title = "FUSE Dirty Chunks"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/chunked/dirty_chunks\" resource.type=\"gce_instance\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_SUM"
                    }
                  }
                }
                legendTemplate = "Dirty Chunks"
                plotType       = "LINE"
              }]
              yAxis = { label = "chunks" }
            }
          }
        }
      ]
    }
  })
}

# Alert: Chunk Cache Hit Ratio Low
resource "google_monitoring_alert_policy" "chunk_cache_low_hit_ratio" {
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "Firecracker Chunk Cache Hit Ratio Low"
  combiner     = "OR"

  conditions {
    display_name = "Chunk cache hit ratio below 50%"
    condition_threshold {
      filter          = "metric.type=\"${local.metric_prefix}/chunked/cache_hit_ratio\" resource.type=\"gce_instance\""
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

# Log-based metric for thaw-agent boot phases (runs inside VM)
# Using DISTRIBUTION type to extract numeric values
resource "google_logging_metric" "vm_boot_phase_from_logs" {
  count       = var.enable_monitoring ? 1 : 0
  name        = "firecracker/vm_boot_phase_from_logs"
  description = "VM boot phase durations from thaw-agent structured logs"
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
# Using DISTRIBUTION type to extract numeric values
resource "google_logging_metric" "job_complete_from_logs" {
  count       = var.enable_monitoring ? 1 : 0
  name        = "firecracker/job_complete_from_logs"
  description = "Job completion metrics from thaw-agent structured logs"
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

