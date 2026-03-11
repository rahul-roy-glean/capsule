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
}
