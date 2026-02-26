# Example: Serverless Functions

This example configures the platform as a serverless function runtime. Functions are snapshotted with their runtime and dependencies pre-initialized, achieving ~200ms cold starts (vs 1–5s for container cold starts) and ~10ms warm starts via pool reuse.

## What the snapshot contains

The golden snapshot is built by:
1. Installing runtime dependencies (`npm install`)
2. Running a warm-up script that starts the function runtime, exercises the hot path, then freezes it

The frozen VM has Node.js/Python/Go initialized and modules loaded. Restoring it is O(working set), not O(installed packages).

## Startup path comparison

| Path | Latency | When |
|---|---|---|
| Pool reuse (same-host paused VM) | ~10ms | Warm: matching paused VM in local pool |
| Golden snapshot restore (UFFD) | ~200ms | Cold: no pooled VM available |
| Session resume (cross-host GCS) | ~300ms | Stateful function resuming from GCS |

## Configuration

Key parameters for high-density serverless:

```yaml
microvm:
  vcpus: 2
  memory_mb: 1024    # Small per-function footprint
  max_per_host: 64   # Pack 64 function VMs per host
  idle_target: 10    # Keep 10 warm VMs ready per host
```

The `idle_target` determines how many pre-warmed VMs the host maintains in the idle pool. Higher values reduce cold-start frequency at the cost of host RAM.

## Scale-to-zero

Paused VMs (in the local pool) hold their memory mapped on the host but consume minimal CPU. Suspended VMs (state in GCS) consume no host resources at all. The platform supports scale-to-zero for infrequently-invoked functions by suspending idle VMs to GCS after a TTL.

To enable:

```yaml
session:
  enabled: true
  ttl_seconds: 300    # Suspend to GCS after 5min idle
  auto_pause: true
```

## Snapshot commands

Adjust for your runtime:

**Node.js:**
```yaml
snapshot_commands:
  - type: "shell"
    args: ["npm", "install", "--prefix", "/app"]
  - type: "shell"
    args: ["bash", "/app/scripts/warm-runtime.sh"]
```

**Python:**
```yaml
snapshot_commands:
  - type: "shell"
    args: ["pip3", "install", "-r", "/app/requirements.txt"]
    run_as_root: true
  - type: "shell"
    args: ["python3", "-c", "import app.handler; app.handler.main({}, {})"]
```

**Go:**
```yaml
snapshot_commands:
  - type: "shell"
    args: ["bash", "/app/scripts/build.sh"]
    run_as_root: false
```

## Onboard

```bash
cp examples/serverless-function/onboard.yaml my-functions.yaml
# Edit my-functions.yaml: set platform.gcp_project, repository.url
# Adjust snapshot_commands for your runtime
make onboard CONFIG=my-functions.yaml
```
