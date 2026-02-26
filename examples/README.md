# Examples

Each subdirectory is a self-contained `onboard.yaml` for a specific use case. Copy the closest example, fill in your project values, and run:

```bash
make onboard CONFIG=<your-config>.yaml
```

## Use cases

| Example | `ci.system` | Session | Description |
|---|---|---|---|
| [ci-github-actions](ci-github-actions/) | `github-actions` | No | Ephemeral GitHub Actions runners with warm Bazel |
| [ai-sandbox](ai-sandbox/) | `none` | Optional | Isolated microVMs for LLM-generated or user-submitted code |
| [dev-environment](dev-environment/) | `none` | Yes | Persistent VS Code Server sessions, cross-host resumable |
| [serverless-function](serverless-function/) | `none` | Optional | High-density function runtime with pool reuse |

## How `ci.system` determines what to configure

### `ci.system: github-actions`

The platform registers Firecracker microVMs as GitHub Actions self-hosted runners. The `bazel.warmup_targets` field controls what gets baked into the golden snapshot. No `workload` block is needed.

### `ci.system: none`

The platform exposes a generic runner API. You must supply:

```yaml
workload:
  snapshot_commands:    # What to bake into the golden snapshot
    - type: "shell" | "git-clone" | "gcp-auth"
      args: [...]
      run_as_root: false
  start_command:        # What the VM runs after each restore
    command: [...]
    port: 8080
    health_path: "/health"
```

Optionally enable session persistence:

```yaml
session:
  enabled: true
  ttl_seconds: 3600
  auto_pause: true    # pause to GCS (true) vs destroy (false) on TTL
```

## `snapshot_commands` reference

| `type` | `args` | Notes |
|---|---|---|
| `git-clone` | `[url, branch]` | Clones repo into `/workspace`; uses GitHub App token if configured |
| `gcp-auth` | `[service-account-email]` | Authenticates `gcloud` as the given service account |
| `shell` | `[command, arg1, ...]` | Runs arbitrary shell command inside the VM |

The `WorkloadKey` (used for pool matching and GCS routing) is a 16-char SHA256 hash of the sorted `snapshot_commands` list. Two configs with the same commands produce the same key and share snapshot chunks in GCS.
