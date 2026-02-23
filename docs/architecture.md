# Architecture

System architecture for bazel-firecracker: self-hosted GitHub Actions runners on Firecracker microVMs with multi-repo support and automated snapshot rollouts.

## Components

```
                          ┌──────────────────────┐
                          │     GitHub.com        │
                          │                       │
                          │  workflow_job webhook ─┼──────────────┐
                          │                       │              │
                          │  Actions job dispatch◄┼──── pull ──┐ │
                          └──────────────────────┘            │ │
                                                              │ │
                                                              │ ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Control Plane  (GKE pod, cmd/control-plane)                          │
│                                                                       │
│  HTTP API:                          Background loops:                  │
│   /webhook/github                    jobRetryLoop (2s)                │
│   /api/v1/repos                      snapshotFreshnessLoop (5m)      │
│   /api/v1/hosts/heartbeat            HealthCheckLoop (30s)            │
│   /api/v1/versions/desired           startDownscaler (30s)            │
│   /api/v1/versions/fleet             controlPlaneMetricsLoop (30s)    │
│   /api/v1/canary/report                                               │
│                                                                       │
│  gRPC: ControlPlane service (RegisterHost, Heartbeat, etc.)           │
│                                                                       │
│  State: PostgreSQL ──► hosts, runners, jobs, snapshots,               │
│                        repos, version_assignments                     │
└──────────┬─────────────────────────────────────┬──────────────────────┘
           │ gRPC AllocateRunner                 │ HTTP heartbeat (10s)
           ▼                                     ▼
┌──────────────────────────┐      ┌──────────────────────────┐
│  Host VM  (GCE, MIG)     │      │  Host VM  (GCE, MIG)     │
│  cmd/firecracker-manager │      │  cmd/firecracker-manager │
│                          │      │                          │
│  Loops:                  │      │  Loops:                  │
│   autoscaleLoop (2s)     │      │   autoscaleLoop (2s)     │
│   heartbeatLoop (10s)    │      │   heartbeatLoop (10s)    │
│                          │      │                          │
│  ┌────────┐  ┌────────┐  │      │  ┌────────┐  ┌────────┐  │
│  │microVM │  │microVM │  │      │  │microVM │  │microVM │  │
│  │repo-a  │  │idle    │  │      │  │repo-b  │  │repo-a  │  │
│  │        │  │        │  │      │  │        │  │        │  │
│  │thaw-   │  │thaw-   │  │      │  │thaw-   │  │thaw-   │  │
│  │agent + │  │agent + │  │      │  │agent + │  │agent + │  │
│  │runner  │  │runner  │  │      │  │runner  │  │runner  │  │
│  └────────┘  └────────┘  │      └────────┘  └────────┘  │
└──────────────────────────┘      └──────────────────────────┘
```

### Control Plane (`cmd/control-plane`)

Runs in GKE. Manages the fleet of host VMs and the lifecycle of snapshots, repos, and jobs. Stateless except for PostgreSQL. Key responsibilities:

- Receive GitHub webhooks and queue jobs
- Schedule runner allocation across hosts (repo-aware scoring)
- Manage snapshot builds, validation, and rollouts
- Track fleet convergence via heartbeat protocol
- Per-repo fairness enforcement

### Host Agent (`cmd/firecracker-manager`)

Runs on each GCE VM in a Managed Instance Group (MIG). Manages Firecracker microVMs on that host. Key responsibilities:

- Maintain an idle pool of pre-booted VMs (autoscale loop)
- Allocate/release runners via gRPC from the control plane
- Heartbeat to control plane with host status and loaded manifests
- Sync snapshot manifests on demand (multi-repo)
- VM pooling (pause/resume for reuse)
- Disk pressure management and orphan cleanup

### Thaw Agent (`cmd/thaw-agent`)

Runs inside each Firecracker microVM after snapshot restore. It is the first process that runs when a VM wakes from a snapshot. Key responsibilities:

- Read MMDS metadata (network config, job info, runner token)
- Configure networking (IP, gateway, DNS)
- Mount block devices (repo cache, credentials, git cache)
- Register as a GitHub Actions runner via `runner.sh`
- Signal readiness to the host agent

### Snapshot Builder (`cmd/snapshot-builder`)

Runs as a one-off GCE VM to build a new snapshot. Key responsibilities:

- Clone the repository
- Boot a Firecracker VM and warm up Bazel caches (`bazel fetch`)
- Take a memory + disk snapshot
- Chunk the snapshot into 4MB content-addressed blocks
- Upload chunks and metadata to GCS

## Runner Lifecycle

The runner lifecycle involves coordination between GitHub, the control plane, host agents, and the thaw agent:

```
GitHub webhook           Control Plane              Host Agent              microVM
"job queued"                  │                          │                     │
     │                        │                          │                     │
     ├──► POST /webhook ──►   │                          │                     │
     │                   INSERT jobs                     │                     │
     │                   status=queued                   │                     │
     │                        │                          │                     │
     │                   jobRetryLoop                    │                     │
     │                   selects best host               │                     │
     │                        │                          │                     │
     │                        ├── gRPC AllocateRunner ──►│                     │
     │                        │                          │                     │
     │                        │                    restore from snapshot       │
     │                        │                    (UFFD mem + FUSE disk)      │
     │                        │                    inject MMDS data            │
     │                        │                    resume VM ─────────────────►│
     │                        │                          │               thaw-agent:
     │                        │                          │               configure net
     │                        │                          │               mount drives
     │                        │                          │               register runner
     │                        │                          │                     │
     │                        │                          │               runner.sh ──► GitHub
     │                        │                          │               "I'm available"
     │                        │                          │                     │
GitHub dispatches          (webhook                      │              ◄── job dispatch
job to runner              "in_progress")                │                (pull-based)
     │                        │                          │                     │
     │                   UPDATE jobs                     │               run workflow
     │                   status=in_progress              │                     │
     │                        │                          │                     │
     │                   (webhook                        │                     │
     │                    "completed")                   │                     │
     │                        │                          │                     │
     │                   UPDATE jobs                     │                     │
     │                   status=completed                │                     │
     │                        │                          │                     │
     │                        ├── ReleaseRunner ────────►│                     │
     │                        │                    try_recycle?                │
     │                        │                    yes → pause VM (pool)       │
     │                        │                    no  → destroy VM            │
```

**Job dispatch is pull-based, not push-based.** The webhook tells the control plane "a job was queued" so it can pre-allocate a runner VM. Inside the VM, `thaw-agent` runs GitHub's `runner.sh` which connects to GitHub over HTTPS and registers as an available runner. GitHub then dispatches the job to that runner directly. The control plane never pushes jobs to runners.

## Multi-Repo

Each repository is identified by a deterministic slug derived from its URL (`pkg/repo/slug.go`):

```
https://github.com/org/repo  →  org-repo
git@github.com:co/project.git  →  co-project
```

The slug is used as a namespace throughout:

| Resource | Path |
|----------|------|
| GCS snapshots | `gs://bucket/<slug>/<version>/` |
| GCS pointer | `gs://bucket/<slug>/current-pointer.json` |
| GCS chunks | `gs://bucket/chunks/<hash>.zst` (shared) |
| Local kernel | `/mnt/data/snapshots/<slug>/kernel.bin` |
| DB snapshots | `WHERE repo_slug = '<slug>'` |
| DB assignments | `version_assignments.repo_slug` |

Chunks are content-addressed and shared across repos. Only manifests and kernels are per-repo.

### UFFD Mode (Required for Multi-Repo)

In traditional mode, each repo's snapshot requires downloading an 8GB memory file per host. With N repos, that's N x 8GB. UFFD (userfaultfd) lazy loading eliminates this:

- Per-repo cost on a host: ~100KB chunk manifest + ~30MB kernel
- Memory pages are fetched on demand via page faults
- Disk blocks are fetched on demand via FUSE
- An LRU chunk cache provides fast access to hot pages

## Database Schema

```
repos                          snapshots
┌─────────────────────┐       ┌─────────────────────────┐
│ slug (PK)           │       │ version (PK)            │
│ url                 │       │ status                  │
│ branch              │       │ repo_slug ──────────────┼──► repos.slug
│ bazel_version       │       │ gcs_path                │
│ build_schedule      │       │ bazel_version           │
│ max_concurrent      │       │ repo_commit             │
│ current_version     │       │ created_at              │
│ auto_rollout        │       └─────────────────────────┘
└─────────────────────┘

hosts                          runners
┌─────────────────────┐       ┌─────────────────────────┐
│ id (PK)             │       │ id (PK)                 │
│ instance_name       │◄──┐   │ host_id ────────────────┼──► hosts.id
│ status              │   │   │ status                  │
│ total_slots         │   │   │ job_id                  │
│ used/idle/busy      │   │   │ repo                    │
│ snapshot_version    │   │   └─────────────────────────┘
│ last_heartbeat      │   │
└─────────────────────┘   │   jobs
                          │   ┌─────────────────────────┐
version_assignments       │   │ id (PK)                 │
┌─────────────────────┐   │   │ github_job_id           │
│ repo_slug           │   │   │ repo                    │
│ host_id ────────────┼───┘   │ status                  │
│ version             │       │ runner_id               │
│ status              │       │ queued_at               │
│ UNIQUE(slug,host)   │       └─────────────────────────┘
└─────────────────────┘
```

`version_assignments` is the mechanism for rollouts. A row with `host_id=NULL` is a fleet-wide default. A row with a specific `host_id` is a per-host override (used during canary).
