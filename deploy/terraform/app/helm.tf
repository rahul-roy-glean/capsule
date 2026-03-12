# Kubernetes namespace
resource "kubernetes_namespace" "firecracker_runner" {
  metadata {
    name = "firecracker-runner"
    labels = {
      "app.kubernetes.io/name" = "firecracker-runner"
    }
  }
}

# Database credentials secret
resource "kubernetes_secret" "db_credentials" {
  metadata {
    name      = "db-credentials"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  }

  data = {
    host     = local.infra.db_private_ip
    username = "postgres"
    password = var.db_password
  }
}

# GitHub credentials secret
resource "kubernetes_secret" "github_credentials" {
  metadata {
    name      = "github-credentials"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  }

  data = {
    webhook_secret = var.github_webhook_secret
  }
}

# Host bootstrap token secret (conditional)
resource "kubernetes_secret" "host_bootstrap_token" {
  count = var.host_bootstrap_token != "" ? 1 : 0

  metadata {
    name      = "host-bootstrap-token"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  }

  data = {
    token = var.host_bootstrap_token
  }
}

# Deploy control plane via Helm
resource "helm_release" "control_plane" {
  name      = "control-plane"
  chart     = "${path.module}/../../helm/firecracker-runner"
  namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  wait      = true
  timeout   = 600

  set {
    name  = "namespace.create"
    value = "false"
  }

  set {
    name  = "image.repository"
    value = "${local.infra.container_registry}/firecracker-control-plane"
  }

  set {
    name  = "image.tag"
    value = var.control_plane_image_tag
  }

  set {
    name  = "config.gcsBucket"
    value = local.infra.snapshot_bucket
  }

  set {
    name  = "config.gcpProject"
    value = local.infra.project_id
  }

  set {
    name  = "config.environment"
    value = local.infra.environment
  }

  set {
    name  = "gcp.projectId"
    value = local.infra.project_id
  }

  set {
    name  = "gcp.region"
    value = local.infra.region
  }

  set {
    name  = "gcp.zone"
    value = local.infra.zone
  }

  set {
    name  = "gcp.hostMigName"
    value = local.infra.host_instance_group_manager_name
  }

  set {
    name  = "gcp.builderNetwork"
    value = local.infra.vpc_network
  }

  set {
    name  = "gcp.builderSubnet"
    value = local.infra.host_subnet_name
  }

  set {
    name  = "gcp.builderServiceAccount"
    value = local.infra.snapshot_builder_service_account
  }

  set {
    name  = "serviceAccount.annotations.iam\\.gke\\.io/gcp-service-account"
    value = local.infra.control_plane_service_account
  }

  depends_on = [
    kubernetes_secret.db_credentials,
    kubernetes_secret.github_credentials,
  ]
}

# Read the Service LB IP after Helm deploys the control plane
data "kubernetes_service" "control_plane" {
  metadata {
    name      = "control-plane"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  }

  depends_on = [helm_release.control_plane]
}

locals {
  control_plane_ip   = data.kubernetes_service.control_plane.status[0].load_balancer[0].ingress[0].ip
  control_plane_addr = "${local.control_plane_ip}:8080"

  otel_collector_ip   = var.enable_otel_collector ? data.kubernetes_service.otel_collector[0].status[0].load_balancer[0].ingress[0].ip : ""
  otel_collector_addr = var.enable_otel_collector ? "http://${local.otel_collector_ip}:4317" : ""
}

# --- OTel Collector (standalone, for host VMs) ---

resource "kubernetes_config_map" "otel_collector" {
  count = var.enable_otel_collector ? 1 : 0

  metadata {
    name      = "otel-collector-config"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  }

  data = {
    "config.yaml" = yamlencode({
      receivers = {
        otlp = {
          protocols = {
            grpc = {
              endpoint = "0.0.0.0:4317"
            }
          }
        }
      }
      processors = {
        batch = {
          timeout         = "5s"
          send_batch_size = 1024
        }
      }
      exporters = {
        googlecloud = {
          project = local.infra.project_id
          metric = {
            service_resource_labels = true
          }
        }
      }
      service = {
        pipelines = {
          traces = {
            receivers  = ["otlp"]
            processors = ["batch"]
            exporters  = ["googlecloud"]
          }
          metrics = {
            receivers  = ["otlp"]
            processors = ["batch"]
            exporters  = ["googlecloud"]
          }
        }
      }
    })
  }
}

resource "kubernetes_deployment" "otel_collector" {
  count = var.enable_otel_collector ? 1 : 0

  metadata {
    name      = "otel-collector"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
    labels = {
      app = "otel-collector"
    }
  }

  spec {
    replicas = 1

    selector {
      match_labels = {
        app = "otel-collector"
      }
    }

    template {
      metadata {
        labels = {
          app = "otel-collector"
        }
      }

      spec {
        service_account_name = "control-plane"

        container {
          name  = "collector"
          image = "otel/opentelemetry-collector-contrib:latest"
          args  = ["--config=/etc/otelcol/config.yaml"]

          port {
            container_port = 4317
            name           = "otlp-grpc"
          }

          resources {
            requests = {
              cpu    = "100m"
              memory = "128Mi"
            }
            limits = {
              cpu    = "500m"
              memory = "512Mi"
            }
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/otelcol"
          }
        }

        volume {
          name = "config"
          config_map {
            name = kubernetes_config_map.otel_collector[0].metadata[0].name
          }
        }
      }
    }
  }

  depends_on = [helm_release.control_plane]
}

resource "kubernetes_service" "otel_collector" {
  count = var.enable_otel_collector ? 1 : 0

  metadata {
    name      = "otel-collector"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
    labels = {
      app = "otel-collector"
    }
    annotations = {
      "networking.gke.io/load-balancer-type" = "Internal"
    }
  }

  spec {
    type = "LoadBalancer"

    port {
      port        = 4317
      name        = "otlp-grpc"
      target_port = "otlp-grpc"
    }

    selector = {
      app = "otel-collector"
    }
  }
}

data "kubernetes_service" "otel_collector" {
  count = var.enable_otel_collector ? 1 : 0

  metadata {
    name      = "otel-collector"
    namespace = kubernetes_namespace.firecracker_runner.metadata[0].name
  }

  depends_on = [kubernetes_service.otel_collector]
}
