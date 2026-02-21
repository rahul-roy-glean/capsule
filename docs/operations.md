# Operations Guide

How snapshots are built, rolled out, rolled back, and how the system recovers from failures.

## Snapshot Build Pipeline

### How a Snapshot Gets Built

A snapshot is a frozen Firecracker VM state containing a warm Bazel cache. It includes kernel, rootfs, memory state, VM state, and a repo-cache-seed disk image. In chunked mode, these are split into 4MB content-addressed blocks stored in GCS.

**Trigger: Automatic (freshness loop)**

The control plane runs `snapshotFreshnessLoop` every 5 minutes. For each repo with a `build_schedule`:

1. Parse the cron expression (e.g., `*/60 * * * *` = every hour)
2. Check snapshot age — if >24h, trigger rebuild
3. Check commit drift — compare the snapshot's `repo_commit` against the branch HEAD via GitHub API (`GET /repos/{owner}/{repo}/compare/{base}...{head}`). If `ahead_by > 0`, trigger rebuild.

**Trigger: Manual**

```bash
# Via API
curl -X POST http://control-plane:8080/api/v1/snapshots \
  -d '{"repo":"https://github.com/org/repo","branch":"main","bazel_version":"7.5"}'

# Via gRPC
grpcurl control-plane:50051 runner.ControlPlane/TriggerSnapshotBuild
```

**Build steps** (runs on a temporary GCE VM):

```
1. Clone repo          (git clone, with GitHub App auth or git-cache)
2. Boot Firecracker    (kernel + rootfs + block devices)
3. Warm Bazel caches   (bazel fetch //... inside the VM)
4. Take snapshot       (pause VM, save memory + disk + state)
5. Chunk snapshot      (split into 4MB SHA256-hashed, zstd-compressed blocks)
6. Upload to GCS       (gs://bucket/<repo-slug>/<version>/)
7. Update pointer      (gs://bucket/<repo-slug>/current-pointer.json)
8. Self-terminate
```

The control plane monitors progress by polling GCS for completion files every 30 seconds. Timeout is 45 minutes.

### Snapshot States

```
building ──► ready ──► validating ──► canary ──► active ──► deprecated
               │                        │
               ▼                        ▼
             failed                  failed     active ──► rolled_back
```

| State | Meaning |
|-------|---------|
| `building` | Builder VM is running |
| `ready` | Build complete, files in GCS |
| `validating` | Auto-validation in progress (test runner boot) |
| `canary` | Running on a subset of hosts |
| `active` | Current production version for this repo |
| `deprecated` | Replaced by a newer active version |
| `failed` | Build, validation, or canary failed |
| `rolled_back` | Was active, replaced by rollback |

## Rollout Pipeline

After a snapshot build completes and reaches `ready`, the pipeline proceeds automatically if the repo has `auto_rollout=true` (default).

### Step 1: Validation

The control plane tests the new snapshot on a real host:

1. Pick a healthy host
2. Push the new version's chunk manifest to that host
3. Allocate a test runner using the new snapshot
4. Wait up to 60 seconds for the thaw-agent health endpoint to respond
5. Release the test runner
6. On success: `ready` → `validating`. On failure: → `failed`.

### Step 2: Canary

A subset of hosts (default 10%, minimum 1) get the new version:

1. Write per-host overrides to `version_assignments`:
   ```
   INSERT INTO version_assignments (repo_slug, host_id, version)
   VALUES ('org-repo', '<host-id>', 'v20260221-...')
   ```
2. Hosts detect the change on their next heartbeat (≤10 seconds):
   - Host sends `loaded_manifests: {"org-repo": "v-old"}`
   - Control plane responds with `sync_versions: {"org-repo": "v-new"}`
   - Host loads the new manifest from GCS in a background goroutine
3. Monitor canary hosts for 5 minutes (configurable):
   - Health check every 30 seconds
   - 3 consecutive failures → abort rollout, revert canary overrides

No host restarts needed. Manifest loading is a ~100KB download.

### Step 3: Full Rollout

1. Update fleet-wide default in `version_assignments`:
   ```
   INSERT INTO version_assignments (repo_slug, host_id, version)
   VALUES ('org-repo', NULL, 'v20260221-...')
   ON CONFLICT (repo_slug, host_id) DO UPDATE SET version = ...
   ```
   `host_id=NULL` means "all hosts not overridden."
2. Clear canary per-host overrides
3. All remaining hosts converge via heartbeat (≤10 seconds)
4. Mark new snapshot `active`, old snapshot `deprecated`
5. Update GCS pointer file
6. Evict pooled VMs on old snapshot version

### Convergence

The heartbeat protocol drives convergence:

```
Host heartbeat request:
  loaded_manifests: {repo_slug → version}

Control plane compares against version_assignments
  (fleet-wide defaults + per-host overrides)

Control plane response:
  desired_versions: {repo_slug → version}   ← full picture
  sync_versions: {repo_slug → version}      ← delta to sync
```

Check fleet status:
```bash
curl http://control-plane:8080/api/v1/versions/fleet?repo_slug=org-repo
```
```json
{
  "repo_slug": "org-repo",
  "hosts": [
    {"host_id": "abc", "instance_name": "fc-host-1", "desired_version": "v2", "current_version": "v2", "converged": true},
    {"host_id": "def", "instance_name": "fc-host-2", "desired_version": "v2", "current_version": "v1", "converged": false}
  ]
}
```

## Rollback

```bash
curl -X POST http://control-plane:8080/api/v1/repos/org-repo/rollback
```

What happens:

1. Find the most recent `deprecated` version for this repo
2. Mark current `active` → `rolled_back`
3. Set previous version back to `active`
4. Update fleet-wide `version_assignments` to previous version
5. Clear all per-host overrides
6. Hosts converge via heartbeat (≤10 seconds)
7. Pool eviction flushes VMs on the rolled-back version

No host restarts. Full fleet convergence within one heartbeat cycle.

### Pin a specific host (for testing)

```bash
curl -X POST http://control-plane:8080/api/v1/repos/org-repo/pin \
  -d '{"host_id": "abc-123", "version": "v20260221-experimental"}'
```

This inserts a per-host override in `version_assignments` that takes precedence over the fleet-wide default. Remove the pin by deleting the override.

## Steady-State Operation

### Job Flow

1. **GitHub sends webhook** (`workflow_job.action=queued`) to `POST /webhook/github`
2. **Control plane enqueues**: INSERT into `jobs` table with `status=queued`. Returns HTTP 200 immediately — no synchronous allocation.
3. **Job retry loop** (every 2 seconds): queries `SELECT * FROM jobs WHERE status='queued' ORDER BY queued_at LIMIT 10`. For each:
   - Derives `repo_slug` from the repo URL
   - Checks per-repo fairness (`max_concurrent_runners`)
   - Scores hosts (capacity, heartbeat freshness, **warm cache affinity**)
   - Sends gRPC `AllocateRunner` to the best host
   - On success: `status=assigned`
   - On failure (no capacity): stays `queued`, retried next cycle
   - On timeout (>5 min queued): `status=failed`
4. **Host agent allocates a runner**:
   - Idempotency check: if same `RequestID` seen recently, return existing runner
   - Disk pressure check: if >85% disk, reject
   - Pool check: if a paused VM matches (same snapshot version + repo), resume it (~10ms)
   - Otherwise: restore from chunked snapshot (UFFD lazy mem + FUSE lazy disk), inject MMDS data, resume VM
5. **Thaw-agent wakes up** inside the microVM:
   - Reads MMDS metadata from `169.254.169.254`
   - Configures network (IP, gateway, DNS)
   - Mounts block devices (repo cache overlay, credentials, git cache)
   - Runs GitHub's `runner.sh` which registers with GitHub over HTTPS
6. **GitHub dispatches the job** to the now-registered runner (pull-based, not push-based)
7. **Job runs** inside the microVM with warm Bazel caches
8. **Job completes**: GitHub sends `workflow_job.action=completed` webhook
   - Control plane marks `jobs.status=completed`
   - Calls `ReleaseRunner` on the host
   - If `try_recycle` + `finished_cleanly`: VM is paused and added to pool for reuse
   - Otherwise: VM is destroyed, resources freed

### Heartbeat Protocol

Every 10 seconds, each host agent sends a heartbeat to the control plane:

```json
{
  "instance_name": "fc-host-abc",
  "zone": "us-central1-a",
  "grpc_address": "10.0.1.5:50051",
  "http_address": "10.0.1.5:8080",
  "total_slots": 16,
  "used_slots": 5,
  "idle_runners": 3,
  "busy_runners": 2,
  "snapshot_version": "v20260221-...",
  "disk_usage": 0.42,
  "loaded_manifests": {"org-repo-a": "v1", "org-repo-b": "v2"},
  "draining": false
}
```

The control plane responds with directives:

```json
{
  "acknowledged": true,
  "should_drain": false,
  "should_sync_snapshot": false,
  "desired_versions": {"org-repo-a": "v1", "org-repo-b": "v3"},
  "sync_versions": {"org-repo-b": "v3"}
}
```

Host actions on response:
- `should_drain=true`: Remove runner labels from GitHub, drain idle runners, stop accepting new jobs. The downscaler will eventually terminate this host.
- `should_sync_snapshot=true`: Trigger `SyncSnapshot` for legacy single-repo mode.
- `sync_versions` entries: For each repo needing sync, load the new chunk manifest from GCS in a background goroutine (~100KB download). New runners allocated after sync will use the new version.

### Autoscale Loop

Every 2 seconds, each host checks if it has enough idle runners:

```
if not draining AND idle_runners < idle_target AND can_add_runner:
    if disk_usage > 0.85:
        log warning, skip
    else:
        allocate a new runner (from pool or fresh snapshot restore)
```

This ensures there are always pre-warmed VMs ready to accept jobs.

### Scheduler Scoring

When multiple hosts can accept a runner, the scheduler picks the best one:

| Factor | Score |
|--------|-------|
| Per idle runner | +10 |
| Per available slot | +5 |
| Heartbeat < 30s old | +20 |
| Utilization > 80% | -30 |
| **Warm cache for target repo** | **+100** |

The +100 warm cache bonus strongly prefers hosts that already have the repo's chunk manifest loaded, avoiding a manifest download on allocation.

## Failure Recovery

### Control Plane Restart

PostgreSQL is the source of truth. On restart:

- `LoadFromDB` rebuilds in-memory host and runner state
- `jobRetryLoop` picks up any `status=queued` jobs and retries them
- `snapshotFreshnessLoop` resumes checking repo freshness
- Hosts continue heartbeating; control plane re-learns the fleet state within one heartbeat cycle (10s)

### Host Agent Crash

On restart:

1. **Wait for data mount**: Block until `/mnt/data` is mounted (startup script may not have run yet)
2. **Create manager**: Initialize snapshot cache, network, and chunk store
3. **Reconcile orphans** (background goroutine):
   - Scan `/var/run/firecracker/*.sock` for orphaned sockets — remove them
   - Scan workspace dir for orphaned directories — remove them
   - Scan `/sys/class/net/` for orphaned `veth-*` and `tap-*` devices — delete them
4. **Start servers**: Accept gRPC and HTTP traffic
5. **Heartbeat**: Re-register with the control plane
6. **Autoscale**: Start allocating idle runners to meet target

Orphaned VMs from the crash are killed and cleaned up. The system does not attempt to re-adopt them because VM state (MMDS, runner registration) cannot be recovered. New VMs are allocated from scratch.

### Job Queue Failures

| Scenario | Behavior |
|----------|----------|
| All hosts at capacity | Job stays `queued`, retried every 2 seconds |
| Job queued > 5 minutes | Marked `failed`, logged as alert |
| Network timeout + retry (duplicate request) | Idempotency check: `recentRequests` map with 5-min TTL returns existing runner |
| Host rejects (draining, disk full) | Job stays `queued`, different host tried next cycle |
| Runner allocated but webhook `completed` never arrives | Runner eventually times out via ephemeral runner behavior |

### Snapshot Build Failures

| Scenario | Behavior |
|----------|----------|
| Builder VM dies | Monitor detects VM not running, marks snapshot `failed` |
| Build exceeds 45 minutes | Timeout fires, marks `failed`, deletes builder VM |
| Validation fails (test runner won't boot) | Marks `failed`, no rollout |
| Canary fails (3 consecutive health check failures) | Rollout aborted, canary overrides cleared |
| Canary host goes unhealthy during canary | Counted as failure; 3 failures → abort |

### Disk Pressure

- **>85% disk usage**: `autoscaleLoop` stops allocating new runners (logged as warning)
- **Orphaned workspaces**: Cleaned at startup by `CleanupOrphanedWorkspaces`
- **Log rotation**: `/var/log/firecracker/*.log` rotated daily, 7-day retention, compressed
- **Chunk GC**: After a snapshot is deprecated, `GarbageCollect` removes unreferenced chunks from GCS (keep last 3 versions per repo)

### Host Health

The control plane's `HealthCheckLoop` (every 30 seconds) marks hosts `unhealthy` if their last heartbeat was >90 seconds ago. Unhealthy hosts are:

- Excluded from `GetAvailableHosts()` — no new jobs scheduled
- Candidates for termination by the downscaler

### GCP Token Expiry (Long Jobs)

GCP access tokens injected via MMDS expire after ~1 hour. For builds longer than that:

- The thaw-agent can call back to the host agent's token refresh endpoint: `GET /api/v1/runners/{id}/token/gcp`
- The host agent fetches a fresh token from the GCE metadata server and returns it
- The thaw-agent updates the gcloud credential in-place

## E2E Canary

A GitHub Actions workflow (`.github/workflows/e2e-canary.yml`) runs every 15 minutes:

```yaml
runs-on: [self-hosted, firecracker]
steps:
  - run: bazel version
  - run: bazel info server_pid
  - run: curl -X POST $CONTROL_PLANE/api/v1/canary/report -d '{"status":"success"}'
```

This exercises the full stack: webhook → allocation → VM boot → runner registration → job dispatch → Bazel execution. The control plane emits success/failure metrics to Cloud Monitoring. A Terraform-managed alert fires if 3+ canary failures occur within 1 hour.

## GCS Layout

```
gs://snapshot-bucket/
├── org-repo-a/
│   ├── current-pointer.json          {"version": "v20260221-120000-..."}
│   ├── v20260221-120000-.../
│   │   ├── kernel.bin
│   │   ├── rootfs.img
│   │   ├── snapshot.mem
│   │   ├── snapshot.state
│   │   ├── repo-cache-seed.img
│   │   ├── metadata.json
│   │   └── chunked-metadata.json
│   └── v20260220-080000-.../
│       └── ...
├── org-repo-b/
│   ├── current-pointer.json
│   └── v20260221-.../
└── chunks/                            ← shared across all repos
    ├── a1b2c3d4e5f6....zst
    ├── f6e5d4c3b2a1....zst
    └── ...
```

Chunks are content-addressed (SHA256 hash of uncompressed data, stored zstd-compressed). Chunks are shared across repos and versions via deduplication — if two repos have identical memory pages or disk blocks, only one copy is stored.

## API Reference

### Repo Management

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/repos` | List all registered repos |
| `POST` | `/api/v1/repos` | Register a new repo |
| `GET` | `/api/v1/repos/{slug}` | Get repo details |
| `PUT` | `/api/v1/repos/{slug}` | Update repo config |

### Version Management

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/v1/versions/desired?instance_name=X` | Get desired versions for a host |
| `GET` | `/api/v1/versions/fleet?repo_slug=X` | Fleet convergence status |

### Monitoring

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/canary/report` | E2E canary health report |
| `GET` | `/api/v1/hosts` | List all hosts with status |
| `GET` | `/api/v1/runners` | List all runners |
| `GET` | `/api/v1/snapshots` | List all snapshots |
