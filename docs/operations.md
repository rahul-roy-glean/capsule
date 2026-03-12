# Operations Guide

This guide is for operators of a running Capsule deployment. It focuses on
runtime behavior, rollout mechanics, failure recovery, and the main places to
inspect when something goes wrong.

## Scope

The current operational model assumes:

- workloads are defined as layered configs
- snapshots are stored in GCS
- the control plane runs in Kubernetes
- hosts run `capsule-manager` and restore Firecracker microVMs

For installation and first deployment, use [setup.md](setup.md). For
request-level recipes, use [HOWTO.md](HOWTO.md).

## Build Pipeline

### Triggering builds

Builds are created from layered configs:

```bash
curl -X POST "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}/build"
```

Layer refreshes are also first-class:

```bash
curl -X POST \
  "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}/layers/${LAYER_NAME}/refresh"
```

### What a build does

For each queued layer build, the control plane:

1. materializes the full layer chain from `layered_configs`
2. launches a nested-virtualization builder VM
3. downloads `snapshot-builder`, `capsule-thaw-agent`, `kernel.bin`, and `rootfs.img`
4. runs `cmd/snapshot-builder` with the resolved layer inputs
5. uploads chunked snapshot metadata keyed by the layer hash
6. updates the workload-key alias if the leaf layer completed successfully

### Build state surfaces

Useful state lives in:

- `snapshot_builds.status` for per-layer queue state
- `snapshot_layers.status` and `current_version` for materialized layer state
- `snapshots.status` for the leaf versions exposed to the fleet

Common layer-build statuses:

- `queued`
- `waiting_parent`
- `running`
- `completed`
- `failed`
- `cancelled`

Common snapshot statuses:

- `ready`
- `active`
- `deprecated`
- `failed`

## Rollout And Host Convergence

### Desired versions

The control plane stores desired workload versions in `version_assignments`.

- `(workload_key, NULL)` is the fleet-wide default
- `(workload_key, host_id)` is a host-specific override

Hosts learn convergence state through heartbeats.

### Heartbeat loop

Each host reports:

- host identity
- gRPC and HTTP addresses
- resource usage
- loaded workload-key versions
- drain state

The control plane responds with:

- `desired_versions`
- `sync_versions`
- optional drain instructions

Check convergence with:

```bash
curl -sS "${CONTROL_PLANE_BASE}/api/v1/versions/fleet?workload_key=${WORKLOAD_KEY}"
```

### Host selection

The scheduler currently considers:

- resource fit for the requested tier
- workload-key cache affinity
- session stickiness when `session_id` is present
- in-memory reservations before the host confirms allocation

## Runner Lifecycle

### Fresh allocation

For a new allocation:

1. the control plane resolves tier, TTL, auto-pause, and network policy
2. it chooses a host
3. the host reuses a pooled VM or restores from chunked snapshot data
4. MMDS is injected before boot or resume completes
5. `capsule-thaw-agent` configures the guest and starts the user service
6. the host proxy exposes service, exec, PTY, and file APIs

### Session resume

If `session_id` is supplied:

1. the control plane checks `session_snapshots`
2. it prefers the original host if that host is still healthy
3. the host resumes from local or GCS-backed session state
4. `capsule-thaw-agent` re-synchronizes MMDS-derived networking and timing

## Session Operations

Session snapshots are incremental and layered.

On pause:

- dirty memory is written as sparse local diffs and may also be uploaded to GCS chunks
- dirty rootfs and extension-drive chunks are persisted
- `session_snapshots` is updated to `suspended`

On resume:

- same-host sessions can restore from local disk state
- cross-host sessions restore from GCS-backed manifests and chunk data
- the original `runner_id` is reused

## Failure Recovery

### Control plane restart

The control plane is stateless apart from PostgreSQL:

- host and runner state is rebuilt from the database plus fresh heartbeats
- queued and historical build state remains in the build tables

### Host restart

On startup, the host agent:

- waits for its data volume
- reconstructs managers and local chunk stores
- reconciles orphaned sockets, workspaces, and network devices
- resumes heartbeats

It does not attempt to adopt already-running Firecracker VMs from a previous
process instance.

### Builder VM failure

If a builder VM terminates before writing the expected artifacts:

- the layer build is marked failed
- children waiting on that layer are cancelled
- terminated builders are cleaned up by the control plane

### Disk pressure

When disk usage is high on a host:

- fresh allocations may be rejected
- idle-pool expansion may be skipped
- orphan cleanup becomes a first-order recovery step

## Observability

### Control plane

Useful endpoints:

- `GET /health`
- `GET /api/v1/hosts`
- `GET /api/v1/runners`
- `GET /api/v1/snapshots`
- `GET /api/v1/layered-configs`

### Host

Useful checks:

```bash
curl http://HOST_IP:8080/health
curl http://HOST_IP:8080/metrics
sudo journalctl -u capsule-manager -f
```

### Guest

Through the host proxy:

- `GET /api/v1/runners/{id}/service-logs`
- `POST /api/v1/runners/{id}/exec`
- `POST /api/v1/runners/{id}/files/read`
- `POST /api/v1/runners/{id}/files/write`
- `GET /api/v1/runners/{id}/proxy/...`

## GCS Layout

The current deployment uses two main storage areas:

```text
gs://snapshot-bucket/
├── v1/build-artifacts/
│   ├── snapshot-builder
│   ├── capsule-thaw-agent
│   ├── kernel.bin
│   └── rootfs.img
├── v1/<layer-hash>/
│   ├── current-pointer.json
│   └── snapshot_state/<version>/chunked-metadata.json
├── v1/<workload-key>/
│   ├── current-pointer.json
│   └── snapshot_state/<version>/chunked-metadata.json
└── v1/chunks/
    ├── disk/...
    └── mem/...
```

Hosts and allocators consume the workload-key alias, not the raw layer hash.

## Recommended Operator Flow

When debugging a production issue, the usual order is:

1. confirm the control plane is healthy
2. inspect the relevant layered config and snapshot status
3. confirm the target hosts have converged to the desired version
4. inspect host logs and metrics
5. inspect guest-facing proxy endpoints if the VM is up but the workload is unhealthy
