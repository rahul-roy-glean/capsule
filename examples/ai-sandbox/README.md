# Example: AI Sandbox

Use this example when you want per-request isolated execution for untrusted,
user-supplied, or model-generated code, while still benefiting from a warm
snapshot and optional session resume.

## What This Example Does

This example assumes you have a sandbox service image that already contains your
runtime and model dependencies. Capsule then:

1. converts that image into a Firecracker guest rootfs
2. runs warmup commands during snapshot build
3. optionally preloads model weights or runtime state before freezing the VM
4. restores a fresh or session-backed sandbox on each allocation

## What You Need To Edit

Before running this example, update:

- `platform.gcp_project`
- `platform.region`
- `platform.zone`
- `workload.base_image`
- `workload.layers`
- `workload.start_command`
- `microvm.memory_mb`
- `microvm.vcpus`

## What The Snapshot Typically Contains

The snapshot for this pattern is usually built by:

1. installing Python or runtime dependencies
2. loading model weights, data, or interpreter state
3. freezing the VM once the runtime is warm

This is what makes restore latency much lower than cold-starting the full stack
on every request.

## Isolation Properties

- hardware isolation through Firecracker microVMs
- fresh state for non-session allocations
- optional network policy enforcement per runner
- no shared kernel with the host or other guests

## Optional Session Persistence

Enable `session` if you need multi-turn interactions that preserve in-VM state:

```yaml
session:
  enabled: true
  ttl_seconds: 1800
  auto_pause: true
```

With sessions enabled, Capsule can pause the sandbox state to GCS and later
resume it on any compatible host.

## Onboard

```bash
cp examples/ai-sandbox/onboard.yaml my-sandbox.yaml
# Edit the fields described above
make onboard CONFIG=my-sandbox.yaml
```

## Sizing Guidance

| Model size | Recommended `memory_mb` | `vcpus` |
|---|---|---|
| <1B params | 4096 | 2 |
| 1-7B params | 16384 | 4 |
| 7-13B params | 32768 | 8 |
| 70B+ params | 131072 | 16 |

Increase `idle_target` if you want more warm sandboxes ready per host.

## What This Example Does Not Configure For You

This example does not automatically provide:

- secret injection for external model providers
- workload-specific auth or API gateway policy
- internet-exposed ingress hardening
- application-level sandbox policy inside your own service

Treat it as a strong starting point for the runtime shape, not a complete
production security policy for every AI workload.
