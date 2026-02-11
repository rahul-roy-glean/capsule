<p align="center">
  <img src="assets/logo.png" width="220" alt="bazel-firecracker logo" />
</p>

# Firecracker-based Bazel Runner Platform

A high-performance GitHub Actions runner platform using Firecracker microVMs on GCP, optimized for Bazel builds with pre-warmed snapshots and sub-second restore times.

## Why

Traditional CI runners (containers or full VMs) start cold every time. Bazel builds pay a heavy penalty for this: downloading externals, rebuilding analysis graphs, and waiting for the Bazel server JVM to warm up. This platform eliminates that overhead by snapshotting a fully warmed Bazel environment and restoring it in ~100-500ms from local NVMe.

**What you get:**

- **Sub-second VM restore** from Firecracker snapshots on local NVMe (~3GB/s)
- **Pre-warmed Bazel** with analysis graphs, fetched externals, and a running Bazel server baked into the snapshot
- **Strong isolation** via dedicated microVMs per job (not shared containers)
- **Two-layer autoscaling** -- GCE hosts via MIG, microVMs per host via the manager

## Architecture

```
                         GitHub (webhook on job event)
                                    |
                                    v
              +---------------------------------------------+
              |           GKE Control Plane                  |
              |  +-------------+ +-----------+ +-----------+ |
              |  | API Service | | Scheduler | | Snapshot  | |
              |  |             | |           | | Manager   | |
              |  +-------------+ +-----------+ +-----------+ |
              |                  Cloud SQL (PostgreSQL)       |
              +---------------------------------------------+
                                    |
                     gRPC (allocate / release / heartbeat)
                                    |
              +---------------------------------------------+
              |      GCE Managed Instance Group (2-20)       |
              |  +----------------------------------------+  |
              |  |   Firecracker Host (n2-standard-64)     |  |
              |  |                                         |  |
              |  |   firecracker-manager (gRPC + HTTP)     |  |
              |  |       |                                 |  |
              |  |       +-- Local NVMe Cache              |  |
              |  |       |   kernel, rootfs, snapshot,     |  |
              |  |       |   git-cache, repo-cache-seed    |  |
              |  |       |                                 |  |
              |  |       +-- MicroVMs (up to 16/host)      |  |
              |  |           +--------+ +--------+         |  |
              |  |           |Runner 1| |Runner N|         |  |
              |  |           |thaw-   | |thaw-   |         |  |
              |  |           |agent   | |agent   |         |  |
              |  |           +--------+ +--------+         |  |
              |  +----------------------------------------+  |
              +---------------------------------------------+
                                    |
              +---------------------------------------------+
              |         GCS Snapshot Bucket                   |
              |  gs://bucket/current/   (latest)             |
              |  gs://bucket/v{date}/   (versioned)          |
              +---------------------------------------------+
```

**Flow:** GitHub webhook -> control plane schedules job -> control plane calls `firecracker-manager` gRPC on selected host -> manager restores microVM from snapshot -> `thaw-agent` configures networking, mounts caches, registers GitHub runner -> runner picks up job -> job completes, runner released.

## Components

| Binary | Description |
|--------|-------------|
| `firecracker-manager` | Host agent. Manages microVM lifecycle, snapshot restore, NAT networking, runner pooling, gRPC API. |
| `control-plane` | GKE service. Runner allocation, scheduling, snapshot version management, GitHub webhooks, host registry. |
| `thaw-agent` | Runs inside each microVM. Configures network from MMDS, mounts overlays, sets up git workspace, registers GitHub runner. |
| `snapshot-builder` | Creates pre-warmed Firecracker snapshots. Boots VM, clones repo, runs `bazel fetch` + analysis, snapshots with Bazel server alive. |
| `git-cache-builder` | Builds ext4 image with git mirrors for fast reference cloning (enables private repo access without network auth). |
| `git-cache-freshness` | Checks if git-cache needs rebuild based on commit drift or age. Used by Cloud Build triggers. |
| `data-snapshot-builder` | Creates GCP disk snapshots containing all artifacts. Enables ~30s host boot vs. minutes downloading from GCS. |
| `onboard` | Interactive setup wizard. Deploys infrastructure, builds images, creates snapshots, configures GitHub. |

## Project Structure

```
bazel-firecracker/
+-- cmd/
|   +-- firecracker-manager/    Host agent
|   +-- control-plane/          GKE control plane
|   +-- thaw-agent/             In-VM initialization
|   +-- snapshot-builder/       Snapshot creation
|   +-- git-cache-builder/      Git mirror image builder
|   +-- git-cache-freshness/    Cache freshness checker
|   +-- data-snapshot-builder/  GCP disk snapshot builder
|   +-- onboard/                Setup wizard
|   +-- snapshot-converter/     Legacy format converter
+-- pkg/
|   +-- firecracker/            Firecracker socket API client
|   +-- runner/                 Runner lifecycle, types, pooling
|   +-- snapshot/               GCS sync, local cache, LRU, chunked loading
|   +-- network/                TAP devices, NAT, bridge setup
|   +-- metrics/                Prometheus host metrics
|   +-- telemetry/              GCP Cloud Monitoring integration
|   +-- ci/                     CI system adapters (GitHub Actions)
|   +-- github/                 GitHub App token generation
|   +-- fuse/                   FUSE driver for lazy disk loading
|   +-- uffd/                   userfaultfd handler for lazy memory loading
|   +-- vsock/                  VM-to-host communication
|   +-- util/                   Helpers (hashing, bounded stacks, etc.)
+-- api/proto/                  gRPC service definitions
+-- deploy/
|   +-- terraform/              GCP infrastructure (VPC, GKE, MIG, Cloud SQL)
|   +-- kubernetes/             Control plane manifests
|   +-- helm/                   Helm charts (alternative to kubectl)
|   +-- packer/                 GCE host VM image
|   +-- docker/                 Dockerfiles for control-plane, snapshot-builder
|   +-- cloudbuild/             Cloud Build pipelines (snapshot/cache automation)
+-- images/microvm/             MicroVM rootfs build scripts
+-- docs/                       Additional documentation
```

## Quick Start

See [docs/DEV_SETUP.md](docs/DEV_SETUP.md) for local development setup and [docs/HOWTO.md](docs/HOWTO.md) for operational guides.

### Prerequisites

- GCP project with billing enabled
- `gcloud` CLI configured
- Go >= 1.24
- Terraform >= 1.0
- Packer >= 1.9

### Automated Setup

The `onboard` tool handles end-to-end deployment:

```bash
# Edit the config
cp onboard.yaml my-config.yaml
vim my-config.yaml

# Dry run
make onboard-validate CONFIG=my-config.yaml

# Deploy
make onboard CONFIG=my-config.yaml
```

### Manual Setup

```bash
# 1. Deploy GCP infrastructure
cd deploy/terraform
terraform init
terraform apply -var="project_id=$PROJECT_ID" -var="db_password=$DB_PASSWORD"

# 2. Build host VM image
make packer-build PROJECT_ID=$PROJECT_ID

# 3. Build and deploy control plane
make docker-build PROJECT_ID=$PROJECT_ID
make docker-push PROJECT_ID=$PROJECT_ID
make k8s-deploy

# 4. Create initial snapshot
make snapshot-builder
./bin/snapshot-builder \
  --repo-url=https://github.com/your-org/your-repo \
  --repo-branch=main \
  --gcs-bucket=$PROJECT_ID-firecracker-snapshots

# 5. (Optional) Build data snapshot for fast host boot
make data-snapshot-build PROJECT_ID=$PROJECT_ID GIT_CACHE_REPOS=github.com/org/repo:name

# 6. Rolling update hosts
make mig-rolling-update PROJECT_ID=$PROJECT_ID
```

### Use in GitHub Actions

```yaml
jobs:
  build:
    runs-on: [self-hosted, firecracker]
    steps:
      - uses: actions/checkout@v4
      - name: Build
        run: bazel build //...
```

## Configuration

### Terraform Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `project_id` | (required) | GCP project ID |
| `region` | `us-central1` | GCP region |
| `host_machine_type` | `n2-standard-64` | Host VM instance type |
| `min_hosts` | `2` | Minimum hosts in MIG |
| `max_hosts` | `20` | Maximum hosts in MIG |
| `max_runners_per_host` | `16` | MicroVMs per host |
| `vcpus_per_runner` | `4` | vCPUs per microVM |
| `memory_per_runner_mb` | `8192` | Memory per microVM |
| `use_data_snapshot` | `false` | Use pre-built disk snapshot for fast host boot |
| `git_cache_enabled` | `false` | Enable git-cache reference cloning |
| `github_runner_enabled` | `false` | Auto-register GitHub runners |
| `enable_monitoring` | `true` | Enable Cloud Monitoring dashboards |

See `deploy/terraform/variables.tf` for the full list.

## Observability

### Metrics (Prometheus)

The `firecracker-manager` exposes metrics on its HTTP port:

- `firecracker_host_total_slots` -- total runner slots on this host
- `firecracker_host_used_slots` -- currently used slots
- `firecracker_host_idle_runners` -- idle runners ready for work
- `firecracker_host_busy_runners` -- runners actively executing jobs

### GCP Cloud Monitoring

When `--telemetry-enabled=true`, the manager writes structured metrics to Cloud Monitoring including boot duration, snapshot restore latency, and runner lifecycle events.

### Alerts (Terraform-managed)

Enable with `enable_monitoring_alerts = true`:

- **HostUnhealthy** -- host not responding to heartbeats
- **NoIdleRunners** -- all runner slots consumed
- **SnapshotSyncFailure** -- failed to sync from GCS
- **SlowVMBoot** -- VM boot P95 exceeds threshold

## Development

```bash
# Install toolchain dependencies
make dev-setup

# Build all binaries
make build

# Run tests
make test-unit          # unit tests (macOS + Linux)
make test-race          # with race detector
make test-cover         # with coverage report
make test-integration   # integration tests (Linux + KVM only)

# Lint
make lint

# Pre-commit check (build + unit tests)
make check

# Run locally
make run-control-plane
sudo make run-host-agent
```

Run `make help` for the full list of targets.

## License

Apache 2.0
