# Rollout Plan

Step-by-step plan for deploying the multi-repo and production hardening changes to a running bazel-firecracker system. Each stage is independently deployable and can be paused or rolled back.

## Prerequisites

- A working bazel-firecracker deployment (single repo, hosts running, jobs flowing)
- Access to the GCP project, GKE cluster, and Cloud SQL instance
- The `multi-repo-rollout-hardening` branch merged to main
- New binaries built (`make build`) and host image baked (Packer)

## Rollout Stages

```
Stage 1: Database migration (zero downtime)
    │
Stage 2: Control plane deploy (rolling restart)
    │
Stage 3: Validate foundation (Phase 0)
    │
Stage 4: Host image roll (rolling MIG update)
    │
Stage 5: Validate host-side changes
    │
Stage 6: Register repos and enable multi-repo
    │
Stage 7: Enable automated rollouts
    │
Stage 8: Enable E2E canary
```

---

## Stage 1: Database Migration

**Risk: None.** All migrations are `ADD COLUMN IF NOT EXISTS` and `CREATE TABLE IF NOT EXISTS` — fully backward compatible with the old control plane binary.

The migrations run automatically in `initSchema` on control plane startup. If you want to apply them ahead of time:

```sql
-- Connect to Cloud SQL
gcloud sql connect firecracker-db --user=postgres --database=firecracker_runner

-- Verify current state
\dt

-- The following will be applied automatically by the new binary,
-- but you can run them manually first to de-risk:

ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS repo VARCHAR(255) DEFAULT '';
ALTER TABLE snapshots ADD COLUMN IF NOT EXISTS repo_slug VARCHAR(255) DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_snapshots_repo_slug ON snapshots(repo_slug);

CREATE TABLE IF NOT EXISTS repos (
    slug VARCHAR(255) PRIMARY KEY,
    url VARCHAR(512) NOT NULL,
    branch VARCHAR(255) DEFAULT 'main',
    bazel_version VARCHAR(32) DEFAULT '',
    warmup_targets VARCHAR(1024) DEFAULT '//...',
    build_schedule VARCHAR(64) DEFAULT '',
    max_concurrent_runners INT DEFAULT 0,
    current_version VARCHAR(255),
    auto_rollout BOOLEAN DEFAULT true,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS version_assignments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_slug VARCHAR(255) NOT NULL,
    host_id UUID REFERENCES hosts(id),
    version VARCHAR(255) NOT NULL,
    status VARCHAR(32) DEFAULT 'assigned',
    assigned_at TIMESTAMP DEFAULT NOW(),
    synced_at TIMESTAMP,
    UNIQUE(repo_slug, host_id)
);

CREATE TABLE IF NOT EXISTS jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    github_workflow_run_id BIGINT,
    github_job_id BIGINT,
    repo VARCHAR(255),
    branch VARCHAR(255),
    commit_sha VARCHAR(40),
    status VARCHAR(20) NOT NULL DEFAULT 'queued',
    runner_id UUID REFERENCES runners(id),
    labels JSONB,
    queued_at TIMESTAMP DEFAULT NOW(),
    started_at TIMESTAMP,
    completed_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

-- Verify
\dt
SELECT COUNT(*) FROM repos;
SELECT COUNT(*) FROM version_assignments;
SELECT COUNT(*) FROM jobs;
```

**Rollback:** Tables are additive. The old binary ignores them.

---

## Stage 2: Deploy Control Plane

Build and push the new control plane image, then do a rolling update on GKE.

```bash
# Build the new binary
make build

# Build and push Docker image
docker build -t us-central1-docker.pkg.dev/${PROJECT_ID}/firecracker/control-plane:v2 \
  -f deploy/docker/Dockerfile.control-plane .
docker push us-central1-docker.pkg.dev/${PROJECT_ID}/firecracker/control-plane:v2

# Update the deployment image (rolling restart, zero downtime with 2 replicas)
kubectl -n firecracker-runner set image deployment/control-plane \
  control-plane=us-central1-docker.pkg.dev/${PROJECT_ID}/firecracker/control-plane:v2

# Watch rollout
kubectl -n firecracker-runner rollout status deployment/control-plane
```

**Validate immediately:**

```bash
CP_URL=$(kubectl -n firecracker-runner get svc control-plane -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

# Health check
curl http://${CP_URL}:8080/health

# New endpoints exist
curl http://${CP_URL}:8080/api/v1/repos
# Should return: {"repos":null,"count":0}

# Existing endpoints still work
curl http://${CP_URL}:8080/api/v1/hosts
curl http://${CP_URL}:8080/api/v1/snapshots

# Jobs table is being used (queue depth should report from DB now)
curl http://${CP_URL}:8080/api/v1/snapshots | jq .current_version
```

**Rollback:** `kubectl rollout undo deployment/control-plane -n firecracker-runner`

---

## Stage 3: Validate Foundation (Phase 0)

With the new control plane running, verify the Phase 0 production fixes are working.

### 3a. Job Queue

```bash
# Trigger a test workflow in your repo
gh workflow run <test-workflow> --repo org/repo

# Check that the job appears in the jobs table
psql -c "SELECT id, github_job_id, repo, status, queued_at FROM jobs ORDER BY queued_at DESC LIMIT 5;"

# Should see: status = 'queued' → 'assigned' → 'in_progress' → 'completed'
```

If hosts are at capacity, verify the job stays `queued` and retries:
```bash
# Watch retry loop
kubectl -n firecracker-runner logs deployment/control-plane -f | grep "job_id"
```

### 3b. Job Traceability

```bash
# After a job runs, verify job_id is populated on the runner
psql -c "SELECT r.id, r.job_id, r.repo, r.status FROM runners r WHERE r.job_id IS NOT NULL ORDER BY r.created_at DESC LIMIT 5;"
```

### 3c. Queue Depth Metric

```bash
# Check that queue depth is reported from DB (not hardcoded 0)
curl http://${CP_URL}:8080/api/v1/snapshots | jq .
# In Cloud Monitoring, check custom.googleapis.com/firecracker/control_plane/queue_depth
```

**Checkpoint:** Jobs flow through the queue correctly, `jobs` table is populated, traceability works. Proceed to Stage 4.

---

## Stage 4: Roll Host Image

Build a new host image with the updated `firecracker-manager` binary and roll it across the MIG.

### 4a. Build New Host Image

```bash
# Build linux binary
GOOS=linux GOARCH=amd64 go build -o bin/firecracker-manager-linux ./cmd/firecracker-manager

# Upload to GCS (for Packer or startup script)
gcloud storage cp bin/firecracker-manager-linux \
  gs://${PROJECT_ID}-firecracker-snapshots/bin/firecracker-manager

# Option A: Bake new image with Packer
cd deploy/packer
packer build -var project_id=${PROJECT_ID} firecracker-host.pkr.hcl

# Option B: Just update the binary via startup script (no image rebuild)
# The startup script already downloads from GCS on boot
```

### 4b. Rolling MIG Update

```bash
# If using a new image:
gcloud compute instance-groups managed rolling-action start-update \
  firecracker-hosts-${ENVIRONMENT} \
  --version=template=firecracker-host-template-v2 \
  --zone=${ZONE} \
  --max-unavailable=1 \
  --max-surge=1

# Watch the rollout
watch -n5 'gcloud compute instance-groups managed list-instances \
  firecracker-hosts-${ENVIRONMENT} --zone=${ZONE} \
  --format="table(instance,status,currentAction)"'
```

If using startup script binary download, just restart hosts one at a time:
```bash
# Drain a host, wait for jobs to finish, restart
gcloud compute instances reset <instance-name> --zone=${ZONE}
```

### 4c. Validate Host Changes

```bash
# SSH into a new host
gcloud compute ssh <instance-name> --zone=${ZONE}

# Check the new binary is running
systemctl status firecracker-manager
journalctl -u firecracker-manager --since "5 min ago" | grep -i "reconcile\|orphan\|disk_usage"

# Verify orphan cleanup ran
journalctl -u firecracker-manager | grep "Reconciling orphaned"

# Verify disk usage is reported in heartbeat
journalctl -u firecracker-manager | grep "disk_usage"

# Verify logrotate is configured
cat /etc/logrotate.d/firecracker

# Verify heartbeat includes should_sync_snapshot handling
journalctl -u firecracker-manager | grep "sync_snapshot\|SyncSnapshot"
```

**Checkpoint:** All hosts running new binary, orphan cleanup works, disk pressure management active. Proceed to Stage 5.

---

## Stage 5: Validate Host-Side Multi-Repo

This validates the heartbeat protocol extension and manifest loading without enabling multi-repo yet.

```bash
# Check that heartbeats include loaded_manifests
kubectl -n firecracker-runner logs deployment/control-plane | grep "loaded_manifests"

# Check desired_versions endpoint (should return empty for now)
curl "http://${CP_URL}:8080/api/v1/versions/desired?instance_name=<host-name>"
```

Verify that the existing single-repo flow still works:
```bash
# Trigger a build
gh workflow run <test-workflow> --repo org/repo

# Watch it allocate and complete
psql -c "SELECT status, COUNT(*) FROM jobs GROUP BY status;"
```

**Checkpoint:** Existing single-repo flow is unbroken. Heartbeat protocol carries multi-repo fields. Proceed to Stage 6.

---

## Stage 6: Register Repos and Enable Multi-Repo

This is the point where you register your repositories and start managing them through the system. Start with your primary repo, then add more.

### 6a. Register Primary Repo

```bash
# Register your existing repo (this doesn't change anything operationally yet)
curl -X POST http://${CP_URL}:8080/api/v1/repos \
  -H 'Content-Type: application/json' \
  -d '{
    "url": "https://github.com/org/primary-repo",
    "branch": "main",
    "bazel_version": "7.5",
    "warmup_targets": "//...",
    "build_schedule": "",
    "max_concurrent_runners": 0
  }'

# Verify
curl http://${CP_URL}:8080/api/v1/repos | jq .
```

### 6b. Build First Repo-Scoped Snapshot

```bash
# Trigger a snapshot build for the registered repo
# This will upload to gs://bucket/org-primary-repo/<version>/ instead of gs://bucket/<version>/
curl -X POST http://${CP_URL}:8080/api/v1/snapshots \
  -d '{"repo":"https://github.com/org/primary-repo","branch":"main","bazel_version":"7.5"}'

# Watch the build
psql -c "SELECT version, status, repo_slug, created_at FROM snapshots ORDER BY created_at DESC LIMIT 5;"

# Verify GCS path is repo-scoped
gcloud storage ls gs://${BUCKET}/org-primary-repo/
```

### 6c. Register Second Repo

```bash
curl -X POST http://${CP_URL}:8080/api/v1/repos \
  -H 'Content-Type: application/json' \
  -d '{
    "url": "https://github.com/org/second-repo",
    "branch": "main",
    "bazel_version": "7.5",
    "build_schedule": "",
    "max_concurrent_runners": 10
  }'

# Build snapshot for second repo
curl -X POST http://${CP_URL}:8080/api/v1/snapshots \
  -d '{"repo":"https://github.com/org/second-repo","branch":"main","bazel_version":"7.5"}'
```

### 6d. Validate Multi-Repo Allocation

```bash
# Trigger a job for each repo and verify they get different snapshots
gh workflow run <test-workflow> --repo org/primary-repo
gh workflow run <test-workflow> --repo org/second-repo

# Check that hosts load manifests for both repos
curl "http://${CP_URL}:8080/api/v1/versions/fleet?repo_slug=org-primary-repo" | jq .
curl "http://${CP_URL}:8080/api/v1/versions/fleet?repo_slug=org-second-repo" | jq .

# Verify scheduler prefers warm hosts
kubectl -n firecracker-runner logs deployment/control-plane | grep "cache_affinity\|warm\|Selected host"
```

**Checkpoint:** Multiple repos registered, snapshots built in repo-scoped GCS paths, jobs for different repos are allocated correctly. Proceed to Stage 7.

---

## Stage 7: Enable Automated Rollouts

### 7a. Enable Build Schedule

```bash
# Enable hourly builds for primary repo
curl -X PUT http://${CP_URL}:8080/api/v1/repos/org-primary-repo \
  -H 'Content-Type: application/json' \
  -d '{"build_schedule": "0 * * * *"}'

# Verify freshness loop is active
kubectl -n firecracker-runner logs deployment/control-plane | grep "freshness\|stale\|drift"
```

### 7b. Verify Auto-Rollout Pipeline

Wait for a scheduled build to trigger, or trigger one manually:

```bash
# Watch the full pipeline: building → ready → validating → canary → active
watch -n10 'psql -c "SELECT version, status, repo_slug, created_at FROM snapshots WHERE repo_slug='\''org-primary-repo'\'' ORDER BY created_at DESC LIMIT 5;"'

# Watch fleet convergence during rollout
watch -n5 'curl -s "http://${CP_URL}:8080/api/v1/versions/fleet?repo_slug=org-primary-repo" | jq ".hosts[] | {name: .instance_name, desired: .desired_version, current: .current_version, converged: .converged}"'
```

### 7c. Test Rollback

```bash
# Verify current active version
psql -c "SELECT version, status FROM snapshots WHERE repo_slug='org-primary-repo' AND status='active';"

# Rollback
curl -X POST http://${CP_URL}:8080/api/v1/repos/org-primary-repo/rollback

# Verify rollback
psql -c "SELECT version, status FROM snapshots WHERE repo_slug='org-primary-repo' AND status IN ('active','rolled_back') ORDER BY created_at DESC LIMIT 3;"

# Verify fleet converges to previous version
curl "http://${CP_URL}:8080/api/v1/versions/fleet?repo_slug=org-primary-repo" | jq .
```

### 7d. Disable Auto-Rollout for a Repo (Optional)

If you want manual promotion for a specific repo:

```bash
curl -X PUT http://${CP_URL}:8080/api/v1/repos/org-sensitive-repo \
  -H 'Content-Type: application/json' \
  -d '{"auto_rollout": false}'
```

Builds will still happen on schedule, but they'll stop at `validated` status and wait for manual promotion.

**Checkpoint:** Automated build → validate → canary → rollout pipeline works. Rollback works. Proceed to Stage 8.

---

## Stage 8: Enable E2E Canary

### 8a. Set Control Plane URL

The E2E canary workflow needs to know where to report results. Set it as a repo-level Actions variable:

```bash
gh variable set CONTROL_PLANE_URL \
  --repo org/primary-repo \
  --body "http://${CP_URL}:8080"
```

### 8b. Enable the Workflow

The workflow file `.github/workflows/e2e-canary.yml` is already in the repo. It runs on `schedule: '*/15 * * * *'`. Verify it runs:

```bash
# Check recent runs
gh run list --repo org/primary-repo --workflow=e2e-canary.yml --limit=5

# Check canary reports in control plane
kubectl -n firecracker-runner logs deployment/control-plane | grep "canary"
```

### 8c. Enable Monitoring Alert

```bash
cd deploy/terraform

# Enable alerts
terraform apply -var enable_monitoring_alerts=true \
  -var monitoring_notification_channels='["projects/${PROJECT_ID}/notificationChannels/<channel-id>"]'
```

Verify the alert policy exists:
```bash
gcloud monitoring policies list --project=${PROJECT_ID} | grep -i canary
```

**Checkpoint:** E2E canary runs every 15 minutes, reports to control plane, monitoring alert configured.

---

## Rollback Plan (Full System)

If the entire deployment needs to be reverted:

### Quick Rollback (Control Plane Only)

```bash
kubectl -n firecracker-runner rollout undo deployment/control-plane
```

The old binary ignores the new tables. Jobs in the `jobs` table will be orphaned but harmless. The old binary allocates synchronously from webhooks again.

### Full Rollback (Hosts + Control Plane)

```bash
# 1. Rollback control plane
kubectl -n firecracker-runner rollout undo deployment/control-plane

# 2. Rollback host image
gcloud compute instance-groups managed rolling-action start-update \
  firecracker-hosts-${ENVIRONMENT} \
  --version=template=firecracker-host-template-v1 \
  --zone=${ZONE} \
  --max-unavailable=1

# 3. Clean up new tables (optional, they're harmless)
psql -c "DROP TABLE IF EXISTS version_assignments;"
psql -c "DROP TABLE IF EXISTS repos;"
# Don't drop jobs table — it may have useful historical data
```

---

## Monitoring Checklist

After each stage, verify these metrics in Cloud Monitoring:

| Metric | Expected |
|--------|----------|
| `control_plane/queue_depth` | Near 0 (jobs processed quickly) |
| `control_plane/hosts_total{status=ready}` | Same as before rollout |
| `vm/allocations_total{result=success}` | Steady rate |
| `vm/boot_duration_seconds` | No regression |
| `host/heartbeat_latency_seconds` | < 1s |
| `e2e/canary_success_total` | Incrementing every 15 min |
| `snapshot/age_seconds` | < 86400 (24h) per repo |

## Post-Rollout Cleanup

After the system is stable for a few days:

1. Remove old non-repo-scoped snapshots from GCS:
   ```bash
   # List old-style snapshots (not under a repo slug)
   gcloud storage ls gs://${BUCKET}/v20*
   # Delete after confirming they're not needed
   ```

2. Run chunk GC for the first time:
   ```bash
   # The system does this automatically after deprecating snapshots,
   # but you can trigger manually if old chunks are accumulating
   ```

3. Set per-repo `max_concurrent_runners` based on observed usage:
   ```bash
   curl -X PUT http://${CP_URL}:8080/api/v1/repos/org-primary-repo \
     -d '{"max_concurrent_runners": 30}'
   ```
