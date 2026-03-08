# Operations Guide

This guide describes the runtime behavior that exists today: layered config builds,
workload-key rollouts, runner allocation, and session pause/resume.

## Build Pipeline

### Triggering Builds

Builds are created from layered configs:

```bash
curl -X POST "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}/build"
```

Layer refreshes are also first-class:

```bash
curl -X POST "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}/layers/${LAYER_NAME}/refresh"
```

### What A Build Does

For each queued layer build, the control plane:

1. materializes the layer chain from `layered_configs`
2. launches a nested-virtualization builder VM
3. downloads `snapshot-builder`, `thaw-agent`, `kernel.bin`, and `rootfs.img` from
   `gs://BUCKET/v1/build-artifacts/`
4. runs `cmd/snapshot-builder` with the materialized layer commands
5. uploads chunked snapshot metadata keyed by layer hash
6. if the layer is the leaf, creates the workload-key alias and updates
   `current-pointer.json`

### Build States

The DB surfaces build progress through:

- `snapshot_builds.status` for per-layer queue state
- `snapshot_layers.status` and `current_version` for active layer state
- `snapshots.status` for leaf workload versions exposed to the fleet

Useful layer-build statuses:

- `queued`
- `waiting_parent`
- `running`
- `completed`
- `failed`
- `cancelled`

Useful snapshot statuses:

- `ready`
- `active`
- `deprecated`
- `failed`

## Rollout And Host Convergence

### Desired Versions

The control plane stores desired workload versions in `version_assignments`.

- a `(workload_key, NULL)` row is the fleet-wide default
- a `(workload_key, host_id)` row is a host-specific override

Hosts learn convergence state through heartbeats.

### Heartbeat Loop

Each host sends:

- host identity
- gRPC and HTTP addresses
- resource usage
- loaded workload-key versions
- draining state

The control plane responds with:

- `desired_versions`
- `sync_versions`
- drain instructions

Check current convergence:

```bash
curl -sS "${CONTROL_PLANE_BASE}/api/v1/versions/fleet?workload_key=${WORKLOAD_KEY}"
```

### Host Selection

The scheduler chooses hosts using:

- capacity and resource fit for the requested tier
- workload-key cache affinity
- session stickiness when `session_id` is present
- optimistic in-memory reservations before the host confirms allocation

## Runner Lifecycle

### Fresh Allocation

For a new allocation:

1. the control plane resolves `tier`, `start_command`, TTL, and network policy from
   `layered_configs`
2. it chooses a host
3. the host either reuses a pooled VM or restores from chunked snapshot data
4. MMDS is injected before the resume/boot transition
5. `thaw-agent` configures the guest and starts the user service
6. the host proxy exposes the service, exec, PTY, and file APIs

### Session Resume

If `session_id` is supplied:

1. the control plane checks `session_snapshots`
2. it prefers the original host if still healthy
3. the host resumes from local or GCS-backed session state
4. `thaw-agent` re-syncs MMDS time and networking after restore

## Session Operations

Session snapshots are incremental and layered.

On pause:

- dirty memory is written as sparse local diffs and optionally uploaded to GCS chunks
- dirty rootfs and extension-drive chunks are persisted
- `session_snapshots` is updated to `suspended`

On resume:

- local sessions use the on-host snapshot chain
- GCS-backed sessions restore with layered UFFD handlers and FUSE-backed disks
- the original `runner_id` is reused

## Failure Recovery

### Control Plane Restart

The control plane is stateless apart from PostgreSQL:

- host and runner state is rebuilt from DB plus fresh heartbeats
- queued jobs remain in `jobs`
- build queue state remains in `snapshot_builds`

### Host Restart

On startup, the host agent:

- waits for `/mnt/data`
- reconstructs managers and chunk stores
- reconciles orphan sockets, workspaces, and network devices
- resumes heartbeats

It does not attempt to re-adopt already-running Firecracker VMs from a previous process.

### Builder VM Failure

If a builder VM terminates before it writes the expected artifacts:

- the layer build is marked failed
- children waiting on that layer are cancelled
- terminated builder VMs are garbage-collected by the control plane

### Disk Pressure

When disk usage is high:

- fresh allocations may be rejected
- autoscaling skips idle-pool expansion
- orphan cleanup becomes critical for recovery

## Observability

### Control Plane

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
sudo journalctl -u firecracker-manager -f
```

### Guest

From the host proxy:

- `GET /api/v1/runners/{id}/service-logs`
- `POST /api/v1/runners/{id}/exec`
- `POST /api/v1/runners/{id}/files/read`
- `POST /api/v1/runners/{id}/files/write`
- `GET /api/v1/runners/{id}/proxy/...`

## GCS Layout

The current deployment uses two main GCS areas:

```text
gs://snapshot-bucket/
├── v1/build-artifacts/
│   ├── snapshot-builder
│   ├── thaw-agent
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

The workload-key alias is what hosts and allocators actually consume at runtime.
