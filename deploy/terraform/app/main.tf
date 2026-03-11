terraform {
  required_version = ">= 1.0.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 5.0"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 5.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.25"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.12"
    }
  }

  backend "gcs" {

  }
}

data "terraform_remote_state" "infra" {
  backend = "gcs"
  config = {
    bucket = var.infra_state_bucket
    prefix = var.infra_state_prefix
  }
}

data "google_client_config" "default" {}

locals {
  infra       = data.terraform_remote_state.infra.outputs
  name_prefix = local.infra.name_prefix
  labels      = local.infra.labels
}

provider "google" {
  project = local.infra.project_id
  region  = local.infra.region
}

provider "google-beta" {
  project = local.infra.project_id
  region  = local.infra.region
}

provider "kubernetes" {
  host                   = "https://${local.infra.gke_endpoint}"
  token                  = data.google_client_config.default.access_token
  cluster_ca_certificate = base64decode(local.infra.gke_ca_certificate)
}

provider "helm" {
  kubernetes {
    host                   = "https://${local.infra.gke_endpoint}"
    token                  = data.google_client_config.default.access_token
    cluster_ca_certificate = base64decode(local.infra.gke_ca_certificate)
  }
}
