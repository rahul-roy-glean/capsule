# Cloud Scheduler for automated snapshot freshness checks
# Only created when enable_snapshot_automation is true

resource "google_cloud_scheduler_job" "snapshot_freshness" {
  count    = var.enable_snapshot_automation ? 1 : 0
  name     = "${var.project_id}-snapshot-freshness"
  schedule = var.snapshot_freshness_schedule
  region   = var.region

  description = "Check snapshot freshness and trigger rebuild if stale"

  http_target {
    uri         = "https://cloudbuild.googleapis.com/v1/projects/${var.project_id}/triggers/${google_cloudbuild_trigger.snapshot_rebuild[0].trigger_id}:run"
    http_method = "POST"
    body        = base64encode(jsonencode({
      substitutions = {
        _GCS_BUCKET       = google_storage_bucket.snapshots.name
        _MAX_AGE_HOURS    = tostring(var.snapshot_max_age_hours)
        _MAX_COMMIT_DRIFT = tostring(var.snapshot_max_commit_drift)
        _ZONE             = var.zone
      }
    }))

    oauth_token {
      service_account_email = google_service_account.scheduler[0].email
    }
  }

  depends_on = [google_cloudbuild_trigger.snapshot_rebuild]
}

# Service account for Cloud Scheduler
resource "google_service_account" "scheduler" {
  count        = var.enable_snapshot_automation ? 1 : 0
  account_id   = "firecracker-scheduler"
  display_name = "Firecracker Snapshot Scheduler"
  project      = var.project_id
}

resource "google_project_iam_member" "scheduler_cloudbuild" {
  count   = var.enable_snapshot_automation ? 1 : 0
  project = var.project_id
  role    = "roles/cloudbuild.builds.editor"
  member  = "serviceAccount:${google_service_account.scheduler[0].email}"
}

# Cloud Build trigger for snapshot rebuild
resource "google_cloudbuild_trigger" "snapshot_rebuild" {
  count    = var.enable_snapshot_automation ? 1 : 0
  name     = "snapshot-rebuild"
  project  = var.project_id
  location = var.region

  description = "Full snapshot rebuild pipeline (freshness check -> build -> validate -> deploy)"

  # Manual trigger only (invoked by Cloud Scheduler or manually)
  source_to_build {
    uri       = "https://github.com/${var.github_repo}"
    ref       = "refs/heads/main"
    repo_type = "GITHUB"
  }

  git_file_source {
    path      = "deploy/cloudbuild/snapshot-rebuild.yaml"
    uri       = "https://github.com/${var.github_repo}"
    revision  = "refs/heads/main"
    repo_type = "GITHUB"
  }

  substitutions = {
    _GCS_BUCKET       = google_storage_bucket.snapshots.name
    _ZONE             = var.zone
    _MAX_AGE_HOURS    = tostring(var.snapshot_max_age_hours)
    _MAX_COMMIT_DRIFT = tostring(var.snapshot_max_commit_drift)
  }
}
