# How-To Guides

Operational recipes for common tasks.

## Table of Contents

- [Deploy from scratch](#deploy-from-scratch)
- [Build and update host images](#build-and-update-host-images)
- [Create a new snapshot](#create-a-new-snapshot)
- [Enable GitHub Actions runner registration](#enable-github-actions-runner-registration)
- [Roll out a host image update](#roll-out-a-host-image-update)
- [Automate snapshot freshness checks](#automate-snapshot-freshness-checks)
- [Scale the cluster](#scale-the-cluster)
- [Monitor and debug](#monitor-and-debug)
- [Inject credentials into microVMs](#inject-credentials-into-microvms)

---

## Deploy from scratch

The `onboard` tool automates the full deployment. If you prefer manual steps, follow the sections below in order.

### Using onboard (recommended)

```bash
# 1. Copy and edit the config
cp onboard.yaml my-config.yaml
vim my-config.yaml   # fill in project, repo, GitHub app details

# 2. Validate (dry run)
make onboard-validate CONFIG=my-config.yaml

# 3. Deploy
make onboard CONFIG=my-config.yaml
```

### Manual deployment

```bash
# 1. Set environment variables
export PROJECT_ID=your-project-id
export DB_PASSWORD=$(openssl rand -base64 24)

# 2. Deploy GCP infrastructure
make terraform-init
make terraform-apply PROJECT_ID=$PROJECT_ID DB_PASSWORD=$DB_PASSWORD

# 3. Build the host VM image
make packer-build PROJECT_ID=$PROJECT_ID

# 4. Build and push control plane container
make docker-build PROJECT_ID=$PROJECT_ID
make docker-push PROJECT_ID=$PROJECT_ID

# 5. Deploy control plane to GKE
gcloud container clusters get-credentials \
  firecracker-runner-dev-control-plane \
  --region us-central1 --project $PROJECT_ID
make k8s-deploy

# 6. Create initial Firecracker snapshot
make snapshot-builder
./bin/snapshot-builder \
  --repo-url=https://github.com/your-org/your-repo \
  --repo-branch=main \
  --gcs-bucket=$PROJECT_ID-firecracker-snapshots

# 7. Roll out hosts
make mig-rolling-update PROJECT_ID=$PROJECT_ID
```

---

## Build and update host images

The host image is a GCE image built with Packer. It contains the `firecracker-manager` binary, Firecracker, and the Linux kernel.

```bash
# Cross-compile the manager for Linux and build the GCE image
make release-host-image PROJECT_ID=$PROJECT_ID
```

This runs `make firecracker-manager-linux` then `packer build`. The image is published to the `firecracker-host` image family.

---

## Create a new snapshot

Snapshots contain the microVM state (memory + disk) with a fully warmed Bazel environment.

```bash
# Build the snapshot-builder binary
make snapshot-builder

# Create a snapshot and upload to GCS
./bin/snapshot-builder \
  --repo-url=https://github.com/your-org/your-repo \
  --repo-branch=main \
  --gcs-bucket=$PROJECT_ID-firecracker-snapshots \
  --vcpus=4 \
  --memory-mb=8192

# For private repos, provide GitHub App credentials
./bin/snapshot-builder \
  --repo-url=https://github.com/your-org/private-repo \
  --repo-branch=main \
  --gcs-bucket=$PROJECT_ID-firecracker-snapshots \
  --github-app-id=$GITHUB_APP_ID \
  --github-app-secret=projects/$PROJECT_ID/secrets/github-app-key/versions/latest
```

The snapshot is uploaded to `gs://$BUCKET/current/` and a versioned copy at `gs://$BUCKET/v{date}-{hash}/`.

---

## Enable GitHub Actions runner registration

Runners can auto-register with GitHub when a microVM boots.

### 1. Create a GitHub App

1. Go to your org's GitHub settings -> Developer settings -> GitHub Apps
2. Create a new app with these permissions:
   - **Repository permissions:** Administration (Read & Write) -- for repo-level registration
   - **Organization permissions:** Self-hosted runners (Read & Write) -- for org-level registration
3. Install the app on your organization or repository
4. Note the App ID and download the private key

### 2. Store the private key in Secret Manager

```bash
gcloud secrets create github-app-key \
  --project=$PROJECT_ID \
  --data-file=path/to/private-key.pem
```

### 3. Enable in Terraform

```bash
terraform apply \
  -var="project_id=$PROJECT_ID" \
  -var="db_password=$DB_PASSWORD" \
  -var="github_runner_enabled=true" \
  -var="github_repo=your-org/your-repo" \
  -var="github_org=your-org" \
  -var="github_app_id=$GITHUB_APP_ID" \
  -var="github_app_secret=projects/$PROJECT_ID/secrets/github-app-key/versions/latest" \
  -var="github_runner_labels=self-hosted,firecracker,Linux,X64,bazel"
```

### 4. Use in workflows

```yaml
jobs:
  build:
    runs-on: [self-hosted, firecracker]
    steps:
      - uses: actions/checkout@v4
      - run: bazel build //...
```

---

## Roll out a host image update

After building a new host image with Packer:

```bash
# Start a rolling update (zero downtime: max-surge=1, max-unavailable=0)
make mig-rolling-update PROJECT_ID=$PROJECT_ID ENV=dev

# Monitor progress
gcloud compute instance-groups managed list-instances \
  firecracker-runner-dev-hosts \
  --region=us-central1
```

The rolling update replaces hosts one at a time. Running jobs complete on old hosts before they are drained.

---

## Automate snapshot freshness checks

Cloud Build can periodically check if snapshots or git-caches are stale and trigger rebuilds.

### Enable in Terraform

```bash
terraform apply \
  -var="project_id=$PROJECT_ID" \
  -var="db_password=$DB_PASSWORD" \
  -var="enable_snapshot_automation=true" \
  -var="snapshot_freshness_schedule=0 */4 * * *" \
  -var="snapshot_max_age_hours=24" \
  -var="snapshot_max_commit_drift=50"
```

This creates a Cloud Scheduler job that triggers the freshness check every 4 hours. If the snapshot is older than 24 hours or more than 50 commits behind HEAD, a rebuild is triggered via Cloud Build.

The Cloud Build pipelines are in `deploy/cloudbuild/`:

| Pipeline | Purpose |
|----------|---------|
| `snapshot-build.yaml` | Build Firecracker snapshot |

---

## Scale the cluster

### Change host count

```bash
# Update MIG bounds
terraform apply \
  -var="project_id=$PROJECT_ID" \
  -var="db_password=$DB_PASSWORD" \
  -var="min_hosts=4" \
  -var="max_hosts=40"
```

### Change microVM density

```bash
# Fewer, larger VMs per host
terraform apply \
  -var="project_id=$PROJECT_ID" \
  -var="db_password=$DB_PASSWORD" \
  -var="max_runners_per_host=8" \
  -var="vcpus_per_runner=8" \
  -var="memory_per_runner_mb=16384"
```

After changing microVM configuration, rebuild the host image and roll out:

```bash
make release-host-image PROJECT_ID=$PROJECT_ID
make mig-rolling-update PROJECT_ID=$PROJECT_ID
```

---

## Monitor and debug

### Check host health

```bash
# List hosts and their status
gcloud compute instance-groups managed list-instances \
  firecracker-runner-dev-hosts --region=us-central1

# SSH to a host
gcloud compute ssh INSTANCE_NAME --zone=us-central1-a

# Check firecracker-manager logs
sudo journalctl -u firecracker-manager -f

# Check manager health endpoint
curl http://INSTANCE_IP:8080/health
```

### Check control plane

```bash
# Port-forward the control plane
kubectl port-forward svc/control-plane 8080:8080

# Health check
curl http://localhost:8080/health
```

### Debug a microVM

If you have SSH access to a host:

```bash
# List running VMs
ls /var/run/firecracker/

# Check thaw-agent health inside a VM (from the host, via the VM's internal IP)
curl http://172.16.0.2:8080/health
curl http://172.16.0.2:8080/debug
curl http://172.16.0.2:8080/connectivity
curl http://172.16.0.2:8080/mmds-diag
```

### View metrics

The `firecracker-manager` exposes Prometheus metrics on its HTTP port:

```bash
curl http://INSTANCE_IP:8080/metrics
```

---

## Inject credentials into microVMs

Credentials (secrets, certificates) are packaged into an ext4 image and attached as a read-only block device to each microVM.

### From GCP Secret Manager

Add to `onboard.yaml`:

```yaml
credentials:
  secrets:
    - name: "artifactory-netrc"
      secret_name: "projects/my-proj/secrets/artifactory-netrc/versions/latest"
      target: "netrc"
    - name: "gcp-sa-key"
      secret_name: "projects/my-proj/secrets/ci-sa-key/versions/latest"
      target: "gcp-service-account.json"
  env:
    GOOGLE_APPLICATION_CREDENTIALS: "/mnt/credentials/gcp-service-account.json"
```

### From host directories

```yaml
credentials:
  host_dirs:
    - name: "buildbarn-certs"
      host_path: "/etc/glean/ci/certs/buildbarn"
      target: "certs/buildbarn"
```

Credentials are mounted at `/mnt/credentials/` inside the microVM by default. The thaw agent sets up any environment variables specified in `credentials.env`.
