# Example: AI Sandboxes

This example configures the platform to serve isolated microVM sandboxes for running untrusted or LLM-generated code. Each request gets a hardware-isolated Firecracker VM restored from a snapshot that has the model weights and runtime pre-loaded.

## What the snapshot contains

The golden snapshot is built by:
1. Installing Python dependencies (`pip3 install -r /app/requirements.txt`)
2. Running `preload_weights.py` to load model weights into memory
3. Freezing the VM — the Python runtime with weights resident in RAM is preserved

Each new sandbox restores from this frozen state in ~300ms. No cold model-loading on every request.

## Workflow

```
API request: POST /exec  {session_id?, code, timeout}
  → Control plane selects a warm VM from the idle pool
  → If pool empty: restore from golden snapshot (~300ms)
  → If session_id set: resume session from GCS (~300ms, cross-host)
  → Code runs inside isolated microVM
  → On completion: VM recycled to pool (pool reuse ~10ms) or paused for session
```

## Key isolation properties

- **Hardware isolation:** KVM hypervisor, no shared kernel with host or other VMs
- **Network isolation:** VM gets a dedicated tap interface; egress can be blocked
- **Fresh state:** Each non-session allocation starts from the golden snapshot
- **No container escape:** No Docker, no containerd, no shared namespace

## Session persistence for multi-turn conversations

Enable `session` to preserve sandbox state across requests (useful for REPL-style interactions):

```yaml
session:
  enabled: true
  ttl_seconds: 1800   # Pause after 30min idle
  auto_pause: true    # Pause to GCS instead of destroying
```

The sandbox's full memory state (loaded weights, interpreter state, variables) is saved to GCS on pause and restored on resume. Cross-host resume means any host in the fleet can pick up the session.

## Onboard

```bash
cp examples/ai-sandbox/onboard.yaml my-sandbox.yaml
# Edit my-sandbox.yaml: set platform.gcp_project and image/workload values
# Adjust microvm.memory_mb and workload.config.tier for your model size
make onboard CONFIG=my-sandbox.yaml
```

## Sizing guidance

| Model size | Recommended `memory_mb` | `vcpus` |
|---|---|---|
| <1B params | 4096 | 2 |
| 1–7B params | 16384 | 4 |
| 7–13B params | 32768 | 8 |
| 70B+ params | 131072 | 16 |

Set `idle_target` to the number of warm VMs you want ready per host (reduces p50 latency at the cost of memory).
