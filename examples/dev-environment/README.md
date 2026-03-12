# Example: Dev Environment

Use this example when you want a persistent, session-bound development
environment that can resume quickly with its workspace, editor state, terminal
history, and tooling already warm.

## What This Example Does

This pattern is designed for browser-based or remotely accessed dev
environments. A typical flow is:

1. clone the repository during snapshot build
2. install the toolchain and editor dependencies
3. warm the language server and workspace indexes
4. restore a dev VM on demand and expose the IDE through the host proxy

## What You Need To Edit

Before running this example, update:

- `platform.gcp_project`
- `platform.region`
- `platform.zone`
- `workload.base_image`
- the repo URL in `workload.layers`
- your toolchain setup script
- your LSP warmup script

## Session Model

This example is intentionally session-heavy:

```yaml
session:
  enabled: true
  ttl_seconds: 3600
  auto_pause: true
```

When the user disconnects or the session idles out, Capsule pauses the VM to
GCS. On reconnect, the scheduler can resume it on any compatible host.

## Access Pattern

After restore, the guest agent launches `code-server` and the host proxy
exposes it through:

```text
http://<host-http-address>/api/v1/runners/<runner-id>/proxy/
```

## Onboard

```bash
cp examples/dev-environment/onboard.yaml my-devenv.yaml
# Edit the fields described above
make onboard CONFIG=my-devenv.yaml
```

## Security Note

The example uses `code-server --auth=none` because it assumes authentication is
handled outside the guest, for example by the control plane, a host proxy, or an
upstream ingress layer. Do not copy that setting directly into an
internet-exposed deployment without adding an outer auth layer.

## Good Fit

This pattern works well for:

- browser IDE environments
- persistent remote coding sessions
- tooling-heavy repos with expensive initial indexing
- session-bound developer sandboxes
