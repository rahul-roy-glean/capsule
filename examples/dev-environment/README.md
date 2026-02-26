# Example: Dev Environments

This example configures the platform as persistent, session-bound cloud development environments. A developer gets a VS Code Server (code-server) instance backed by a snapshot of their repo with the toolchain, LSP servers, and workspace pre-warmed.

## What the snapshot contains

The golden snapshot is built by:
1. Cloning the repository
2. Installing the toolchain (compiler, language servers, formatters)
3. Running a warm-up script that starts the LSP server and indexes the codebase
4. Freezing the VM — the LSP index, open editor state, and running processes are preserved

Each developer resumes their environment in ~300ms instead of waiting for LSP indexing.

## Workflow

```
Developer opens browser tab
  → AllocateRunner{session_id: "<user-id>"}
  → If session exists in GCS: resume (~300ms, any host)
  → If no session: restore from golden snapshot (~300ms)
  → thaw-agent starts code-server, waits for /healthz 200
  → Host proxies traffic to port 8443
  → Developer works in browser IDE

Developer closes browser tab (or idle timeout fires)
  → AutoPause: VM paused to GCS (~50ms diff snapshot)
  → Session ID preserved; next open resumes exact state
```

## Session persistence

The `session` block is required for this use case:

```yaml
session:
  enabled: true
  ttl_seconds: 3600   # Pause after 1h idle
  auto_pause: true    # Pause to GCS (preserves open files, terminal history, LSP state)
```

**Cross-host mobility:** The session is stored in GCS. When the developer reconnects, the scheduler picks any host with available capacity — not necessarily the same host as before. The VM resumes with identical state regardless of which host runs it.

## Snapshot commands

Customize `workload.snapshot_commands` for your stack:

```yaml
snapshot_commands:
  - type: "git-clone"
    args: ["https://github.com/myorg/myrepo", "main"]
  - type: "shell"
    args: ["bash", "/setup/install-toolchain.sh"]
    run_as_root: true
  - type: "shell"
    args: ["bash", "/setup/warm-lsp.sh"]
    run_as_root: false
```

The `install-toolchain.sh` script should install your language toolchain, extensions, and any IDE dependencies. The `warm-lsp.sh` script should start the language server and trigger an initial index so it's warm at restore time.

## Onboard

```bash
cp examples/dev-environment/onboard.yaml my-devenv.yaml
# Edit my-devenv.yaml: set platform.gcp_project, repository.url
# Customize workload.snapshot_commands for your toolchain
make onboard CONFIG=my-devenv.yaml
```

## Accessing the environment

After onboarding, allocate a session via the API:

```bash
curl -X POST https://<control-plane>/v1/runners/allocate \
  -H "Content-Type: application/json" \
  -d '{"session_id": "user-alice", "workload_key": "<your-key>", "resources": {"vcpus": 4, "memory_mb": 8192}}'
```

The response includes the host IP and port to connect code-server.
