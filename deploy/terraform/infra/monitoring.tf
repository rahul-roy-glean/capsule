# GCP Cloud Monitoring configuration for Firecracker runners
# Dashboards and log-based metrics (infra stage)
#
# Alert policies are in the app/ stage.

locals {
  metric_prefix = "workload.googleapis.com"
  # Service names used in metric.label.service_name filters
  cp_service  = "control-plane"
  mgr_service = "firecracker-manager"
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
          xPos   = 6
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

# Operations Dashboard (snapshot automation & fleet health)
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

# Log-based metric for thaw-agent boot phases (runs inside VM)
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
