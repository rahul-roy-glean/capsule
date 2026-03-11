# Monitoring for Firecracker runners (app stage)
#
# Dashboards and alert policies. Log-based metrics are in the infra/ stage.

locals {
  metric_prefix = "workload.googleapis.com"
  cp_service    = "control-plane"
  mgr_service   = "firecracker-manager"
}

# Dashboard for Firecracker Runner Overview
resource "google_monitoring_dashboard" "firecracker_overview" {
  count = var.enable_monitoring ? 1 : 0
  dashboard_json = jsonencode({
    displayName = "Firecracker Runner Overview"
    labels = {
      environment = local.infra.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Row 1: High-level stats
        {
          width  = 3
          height = 2
          widget = {
            title = "Ready Hosts"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts.ready\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
            title = "Total Runners"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.total\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
            title = "Snapshot Age"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/snapshot.age\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "300s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_MAX"
                  }
                }
              }
            }
          }
        },
        # Row 2: VM Boot Time (host-side histogram, needs ALIGN_DELTA for cumulative distribution)
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.allocations\" AND metric.label.service_name=\"${local.mgr_service}\" AND metric.label.result=\"success\""
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.allocations\" AND metric.label.service_name=\"${local.mgr_service}\" AND metric.label.result=\"failure\""
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
            title = "Runners By State (Fleet)"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.idle\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
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
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.busy\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Busy"
                  plotType       = "STACKED_AREA"
                }
              ]
              yAxis = {
                label = "runners"
              }
            }
          }
        },
        # Hosts By Status
        {
          yPos   = 6
          xPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Hosts By Status"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts.ready\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Ready"
                  plotType       = "STACKED_AREA"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts.draining\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Draining"
                  plotType       = "STACKED_AREA"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts.unhealthy\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Unhealthy"
                  plotType       = "STACKED_AREA"
                }
              ]
              yAxis = {
                label = "hosts"
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
                    filter = "metric.type=\"${local.metric_prefix}/chunked.cache_hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                    filter = "metric.type=\"${local.metric_prefix}/pool.hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
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
resource "google_monitoring_dashboard" "vm_boot_phases" {
  count = var.enable_monitoring ? 1 : 0
  dashboard_json = jsonencode({
    displayName = "Firecracker VM Boot Phases"
    labels = {
      environment = local.infra.environment
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

# ============================================================================
# Operations Dashboard (snapshot automation & fleet health)
resource "google_monitoring_dashboard" "firecracker_operations" {
  count   = var.enable_monitoring ? 1 : 0
  project = local.infra.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Runner Operations"
    labels = {
      environment = local.infra.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Fleet overview
        {
          width  = 6
          height = 4
          widget = {
            title = "Ready Hosts"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts.ready\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.total\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
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
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.idle\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
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
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.busy\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
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
                    filter = "metric.type=\"${local.metric_prefix}/snapshot.age\" AND metric.label.service_name=\"${local.cp_service}\""
                    aggregation = {
                      alignmentPeriod    = "300s"
                      perSeriesAligner   = "ALIGN_MAX"
                      crossSeriesReducer = "REDUCE_MAX"
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.allocations\" AND metric.label.service_name=\"${local.mgr_service}\" AND metric.label.result=\"success\""
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
                      filter = "metric.type=\"${local.metric_prefix}/vm.allocations\" AND metric.label.service_name=\"${local.mgr_service}\" AND metric.label.result=\"failure\""
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

# Chunked Snapshot & Pool Dashboard
resource "google_monitoring_dashboard" "chunked_snapshot" {
  count   = var.enable_monitoring ? 1 : 0
  project = local.infra.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Chunked Snapshots & Pool"
    labels = {
      environment = local.infra.environment
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
                  filter = "metric.type=\"${local.metric_prefix}/chunked.cache_hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MEAN"
                    crossSeriesReducer = "REDUCE_MEAN"
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
            title = "Disk Cache Used"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked.disk_cache.size\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
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
                  filter = "metric.type=\"${local.metric_prefix}/pool.hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MEAN"
                    crossSeriesReducer = "REDUCE_MEAN"
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
            title = "Dirty Chunks"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked.dirty_chunks\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        # Row 2: UFFD page faults + memory cache hits vs misses
        {
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.page_faults\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.chunk_fetches\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Chunk Fetches (GCS)"
                }
              ]
              yAxis = { label = "count/min" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "Memory Cache Hits vs Misses"
            xyChart = {
              dataSets = [
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.cache_hits\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.cache_misses\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Misses"
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.disk_reads\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.disk_writes\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.disk_cache.size\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Used"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.disk_cache.max\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
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
                    filter = "metric.type=\"${local.metric_prefix}/pool.hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/pool.hits\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/pool.misses\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/pool.evictions\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/pool.recycle_failures\" AND metric.label.service_name=\"${local.mgr_service}\""
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
                      filter = "metric.type=\"${local.metric_prefix}/pool.memory.used\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "Used"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/pool.memory.max\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
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
                    filter = "metric.type=\"${local.metric_prefix}/chunked.dirty_chunks\" AND metric.label.service_name=\"${local.mgr_service}\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MAX"
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

# Dashboard for Control Plane Endpoint Health
resource "google_monitoring_dashboard" "endpoint_health" {
  count   = var.enable_monitoring ? 1 : 0
  project = local.infra.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Control Plane Endpoint Health"
    labels = {
      environment = local.infra.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Row 1: Scorecards
        {
          width  = 2
          height = 2
          widget = {
            title = "Requests / 5m"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "300s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 2
          width  = 2
          height = 2
          widget = {
            title = "5xx / 5m"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.status_class=\"5xx\""
                  aggregation = {
                    alignmentPeriod    = "300s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 4
          width  = 2
          height = 2
          widget = {
            title = "Allocate Requests / 1h"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/allocate\""
                  aggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 6
          width  = 2
          height = 2
          widget = {
            title = "Pause Requests / 1h"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/pause\""
                  aggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 8
          width  = 2
          height = 2
          widget = {
            title = "Connect/Resume Requests / 1h"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/connect\""
                  aggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 10
          width  = 2
          height = 2
          widget = {
            title = "Max Inflight Requests"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests.inflight\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        # Row 2: P95 latency scorecards
        {
          yPos   = 2
          width  = 4
          height = 2
          widget = {
            title = "Allocate P95 / 1h"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.request.duration\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/allocate\""
                  aggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                  secondaryAggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_PERCENTILE_95"
                    crossSeriesReducer = "REDUCE_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 4
          yPos   = 2
          width  = 4
          height = 2
          widget = {
            title = "Pause P95 / 1h"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.request.duration\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/pause\""
                  aggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                  secondaryAggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_PERCENTILE_95"
                    crossSeriesReducer = "REDUCE_MEAN"
                  }
                }
              }
            }
          }
        },
        {
          xPos   = 8
          yPos   = 2
          width  = 4
          height = 2
          widget = {
            title = "Connect/Resume P95 / 1h"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.request.duration\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/connect\""
                  aggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                  secondaryAggregation = {
                    alignmentPeriod    = "3600s"
                    perSeriesAligner   = "ALIGN_PERCENTILE_95"
                    crossSeriesReducer = "REDUCE_MEAN"
                  }
                }
              }
            }
          }
        },
        # Row 3: Latency charts (p50/p95/p99 per endpoint)
        {
          yPos   = 4
          width  = 4
          height = 4
          widget = {
            title = "Allocate Latency (P50/P95/P99)"
            xyChart = {
              dataSets = [
                for p in ["50", "95", "99"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.request.duration\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/allocate\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_${p}"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p${p}"
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "seconds" }
            }
          }
        },
        {
          xPos   = 4
          yPos   = 4
          width  = 4
          height = 4
          widget = {
            title = "Pause Latency (P50/P95/P99)"
            xyChart = {
              dataSets = [
                for p in ["50", "95", "99"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.request.duration\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/pause\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_${p}"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p${p}"
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "seconds" }
            }
          }
        },
        {
          xPos   = 8
          yPos   = 4
          width  = 4
          height = 4
          widget = {
            title = "Connect/Resume Latency (P50/P95/P99)"
            xyChart = {
              dataSets = [
                for p in ["50", "95", "99"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.request.duration\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"/api/v1/runners/connect\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                      secondaryAggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_PERCENTILE_${p}"
                        crossSeriesReducer = "REDUCE_MEAN"
                      }
                    }
                  }
                  legendTemplate = "p${p}"
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "seconds" }
            }
          }
        },
        # Row 4: Request volume and error volume
        {
          yPos   = 8
          width  = 6
          height = 4
          widget = {
            title = "Key Endpoint Request Volume"
            xyChart = {
              dataSets = [
                for route in [
                  { path = "/api/v1/runners/allocate", label = "allocate" },
                  { path = "/api/v1/runners/pause", label = "pause" },
                  { path = "/api/v1/runners/connect", label = "connect/resume" },
                  { path = "/api/v1/runners/release", label = "release" },
                ] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"${route.path}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = route.label
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "requests/min" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 8
          width  = 6
          height = 4
          widget = {
            title = "Key Endpoint Error Volume"
            xyChart = {
              dataSets = flatten([
                for route in [
                  { path = "/api/v1/runners/allocate", label = "allocate" },
                  { path = "/api/v1/runners/pause", label = "pause" },
                  { path = "/api/v1/runners/connect", label = "connect/resume" },
                  { path = "/api/v1/runners/release", label = "release" },
                ] : [
                  for code in ["4xx", "5xx"] : {
                    timeSeriesQuery = {
                      timeSeriesFilter = {
                        filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"${route.path}\" AND metric.label.status_class=\"${code}\""
                        aggregation = {
                          alignmentPeriod    = "60s"
                          perSeriesAligner   = "ALIGN_DELTA"
                          crossSeriesReducer = "REDUCE_SUM"
                        }
                      }
                    }
                    legendTemplate = "${route.label} ${code}"
                    plotType       = "STACKED_BAR"
                  }
                ]
              ])
              yAxis = { label = "errors/min" }
            }
          }
        }
      ]
    }
  })
}

# Dashboard for Fleet Capacity and Scheduling
resource "google_monitoring_dashboard" "fleet_capacity" {
  count   = var.enable_monitoring ? 1 : 0
  project = local.infra.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Fleet Capacity And Scheduling"
    labels = {
      environment = local.infra.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Row 1: Scorecards
        {
          width  = 3
          height = 2
          widget = {
            title = "Ready Hosts"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.status=\"ready\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
            title = "Total Runners"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.total\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
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
            title = "Idle Runners"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.idle\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
            title = "Queue Depth"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/control_plane.queue.depth\" AND metric.label.service_name=\"${local.cp_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        # Row 2: Hosts and Runners by status
        {
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "Hosts By Status"
            xyChart = {
              dataSets = [
                for status in ["ready", "draining", "terminating", "unhealthy"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.hosts.${status}\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = status
                  plotType       = "STACKED_AREA"
                }
              ]
              yAxis = { label = "hosts" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "Runners By State"
            xyChart = {
              dataSets = [
                for state in ["idle", "busy"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.runners.${state}\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = state
                  plotType       = "STACKED_AREA"
                }
              ]
              yAxis = { label = "runners" }
            }
          }
        },
        # Row 3: Fleet CPU and Memory
        {
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Fleet CPU Used / Free / Total"
            xyChart = {
              dataSets = [
                for label in ["used", "free", "total"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.fleet.cpu_millicores.${label}\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = label
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "millicores" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Fleet Memory Used / Free / Total"
            xyChart = {
              dataSets = [
                for label in ["used", "free", "total"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.fleet.memory_mb.${label}\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = label
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "MB" }
            }
          }
        },
        # Row 4: Utilization and Snapshot Age
        {
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Fleet Utilization"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/control_plane.fleet.utilization\" AND metric.label.service_name=\"${local.cp_service}\""
                    aggregation = {
                      alignmentPeriod    = "60s"
                      perSeriesAligner   = "ALIGN_MEAN"
                      crossSeriesReducer = "REDUCE_MEAN"
                    }
                  }
                }
                legendTemplate = "utilization"
                plotType       = "LINE"
              }]
              yAxis = { label = "ratio" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Snapshot Age"
            xyChart = {
              dataSets = [{
                timeSeriesQuery = {
                  timeSeriesFilter = {
                    filter = "metric.type=\"${local.metric_prefix}/snapshot.age\" AND metric.label.service_name=\"${local.cp_service}\""
                    aggregation = {
                      alignmentPeriod    = "300s"
                      perSeriesAligner   = "ALIGN_MAX"
                      crossSeriesReducer = "REDUCE_MAX"
                    }
                  }
                }
                legendTemplate = "snapshot age"
                plotType       = "LINE"
              }]
              yAxis = { label = "seconds" }
            }
          }
        },
        # Row 5: Canary and errors
        {
          yPos   = 14
          width  = 6
          height = 4
          widget = {
            title = "Canary Outcomes / Hour"
            xyChart = {
              dataSets = [
                for outcome in ["success", "failure"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/e2e.canary.${outcome}\" AND metric.label.service_name=\"${local.cp_service}\""
                      aggregation = {
                        alignmentPeriod    = "3600s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = outcome
                  plotType       = "STACKED_BAR"
                }
              ]
              yAxis = { label = "checks / hour" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 14
          width  = 6
          height = 4
          widget = {
            title = "Endpoint 5xx / 5m"
            xyChart = {
              dataSets = [
                for route in [
                  { path = "/api/v1/runners/allocate", label = "allocate" },
                  { path = "/api/v1/runners/connect", label = "connect/resume" },
                  { path = "/api/v1/runners/release", label = "release" },
                ] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/control_plane.endpoint.requests\" AND metric.label.service_name=\"${local.cp_service}\" AND metric.label.route=\"${route.path}\" AND metric.label.status_class=\"5xx\""
                      aggregation = {
                        alignmentPeriod    = "300s"
                        perSeriesAligner   = "ALIGN_DELTA"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "${route.label} 5xx"
                  plotType       = "STACKED_BAR"
                }
              ]
              yAxis = { label = "errors / 5m" }
            }
          }
        }
      ]
    }
  })
}

# Dashboard for Chunked Runtime and Cache (host-level detail)
resource "google_monitoring_dashboard" "chunked_runtime" {
  count   = var.enable_monitoring ? 1 : 0
  project = local.infra.project_id
  dashboard_json = jsonencode({
    displayName = "Firecracker Chunked Runtime And Cache"
    labels = {
      environment = local.infra.environment
    }
    mosaicLayout = {
      columns = 12
      tiles = [
        # Row 1: Scorecards
        {
          width  = 3
          height = 2
          widget = {
            title = "Chunk Cache Hit Ratio"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked.cache_hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MEAN"
                    crossSeriesReducer = "REDUCE_MEAN"
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
            title = "Disk Cache Used"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked.disk_cache.size\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
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
            title = "Memory Cache Used"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked.mem_cache.size\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
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
            title = "Dirty Chunks"
            scorecard = {
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"${local.metric_prefix}/chunked.dirty_chunks\" AND metric.label.service_name=\"${local.mgr_service}\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_MAX"
                    crossSeriesReducer = "REDUCE_SUM"
                  }
                }
              }
            }
          }
        },
        # Row 2: UFFD and memory cache
        {
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
                      filter = "metric.type=\"${local.metric_prefix}/chunked.page_faults\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "page faults"
                },
                {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.chunk_fetches\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = "chunk fetches (GCS)"
                }
              ]
              yAxis = { label = "latest total" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 2
          width  = 6
          height = 4
          widget = {
            title = "Memory Cache Hits vs Misses"
            xyChart = {
              dataSets = [
                for label in ["cache_hits", "cache_misses"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.${label}\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = replace(label, "cache_", "")
                }
              ]
              yAxis = { label = "latest total" }
            }
          }
        },
        # Row 3: FUSE I/O and disk cache size
        {
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "FUSE Disk IO Totals"
            xyChart = {
              dataSets = [
                for label in ["disk_reads", "disk_writes"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.${label}\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = replace(label, "_", " ")
                }
              ]
              yAxis = { label = "latest total" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 6
          width  = 6
          height = 4
          widget = {
            title = "Disk Cache Used Vs Max"
            xyChart = {
              dataSets = [
                for label in ["size", "max"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.disk_cache.${label}\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = label == "size" ? "used" : "max"
                }
              ]
              yAxis = { label = "bytes" }
            }
          }
        },
        # Row 4: Memory cache size and host CPU
        {
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Memory Cache Used Vs Max"
            xyChart = {
              dataSets = [
                for label in ["size", "max"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/chunked.mem_cache.${label}\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MAX"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = label == "size" ? "used" : "max"
                }
              ]
              yAxis = { label = "bytes" }
            }
          }
        },
        {
          xPos   = 6
          yPos   = 10
          width  = 6
          height = 4
          widget = {
            title = "Host CPU Used Vs Total"
            xyChart = {
              dataSets = [
                for label in ["used", "total"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host.cpu_millicores.${label}\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = label
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "millicores" }
            }
          }
        },
        # Row 5: Host memory
        {
          yPos   = 14
          width  = 6
          height = 4
          widget = {
            title = "Host Memory Used Vs Total"
            xyChart = {
              dataSets = [
                for label in ["used", "total"] : {
                  timeSeriesQuery = {
                    timeSeriesFilter = {
                      filter = "metric.type=\"${local.metric_prefix}/host.memory_mb.${label}\" AND metric.label.service_name=\"${local.mgr_service}\""
                      aggregation = {
                        alignmentPeriod    = "60s"
                        perSeriesAligner   = "ALIGN_MEAN"
                        crossSeriesReducer = "REDUCE_SUM"
                      }
                    }
                  }
                  legendTemplate = label
                  plotType       = "LINE"
                }
              ]
              yAxis = { label = "MB" }
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
      filter          = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/vm.boot.duration\" AND metric.label.service_name=\"${local.mgr_service}\""
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
      filter          = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/control_plane.runners.idle\" AND metric.label.service_name=\"${local.cp_service}\""
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
      filter          = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/control_plane.queue.depth\" AND metric.label.service_name=\"${local.cp_service}\""
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
      filter          = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/snapshot.age\" AND metric.label.service_name=\"${local.cp_service}\""
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
      filter   = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/host.heartbeat.latency\" AND metric.label.service_name=\"${local.mgr_service}\""
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
      filter          = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/chunked.cache_hit_ratio\" AND metric.label.service_name=\"${local.mgr_service}\""
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
  count        = var.enable_monitoring && var.enable_monitoring_alerts ? 1 : 0
  display_name = "E2E Canary Consecutive Failures"
  project      = local.infra.project_id
  combiner     = "OR"

  conditions {
    display_name = "E2E canary failures > 3 in 1h"
    condition_threshold {
      filter          = "resource.type=\"prometheus_target\" AND metric.type=\"${local.metric_prefix}/e2e.canary.failure\" AND metric.label.service_name=\"${local.cp_service}\""
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
