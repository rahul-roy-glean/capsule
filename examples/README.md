# Examples

Each subdirectory is a self-contained `onboard.yaml` for a specific use case. Copy the closest example, fill in your project values, and run:

```bash
make onboard CONFIG=<your-config>.yaml
```

## Use cases

| Example | Description |
|---|---|
| [ci-github-actions](ci-github-actions/) | Ephemeral GitHub Actions runners with warm Bazel cache |
| [ci-gitlab-runners](ci-gitlab-runners/) | GitLab CI runners — validates CI-agnostic design |
| [ci-bazel-remote-exec](ci-bazel-remote-exec/) | Bazel + Buildbarn with artifact cache overlay and credentials drive |
| [ci-git-cache](ci-git-cache/) | Git-cache reference cloning for fast `actions/checkout` |
| [ai-sandbox](ai-sandbox/) | Isolated microVMs for LLM-generated or user-submitted code |
| [dev-environment](dev-environment/) | Persistent VS Code Server sessions, cross-host resumable |
| [serverless-function](serverless-function/) | High-density function runtime with pool reuse |

## How the platform handles different workloads

The platform has no knowledge of CI systems, build tools, or application
frameworks. It provides four generic primitives:

### 1. `snapshot_commands` / `init_commands` — what to bake and what to run

```yaml
snapshot_commands:
  - type: "git-clone"
    args: ["https://github.com/myorg/myrepo", "main"]
  - type: "shell"
    args: ["bazel", "fetch", "//..."]
```

These run inside the VM during snapshot building. The result is frozen into the
snapshot. `init_commands` run after each snapshot restore.

### 2. `start_command` — what the VM does after restore

```yaml
start_command:
  command: ["/home/runner/config.sh", "--token", "${CI_RUNNER_TOKEN}", "--ephemeral"]
  env:
    CI_RUNNER_TOKEN: "${ci_runner_token}"
```

This is how CI runner registration, user services, and function runtimes all
work. The platform starts the command and waits for the health check.

### 3. `drives` — block devices attached to the VM

```yaml
drives:
  - drive_id: "git_cache"
    label: "GIT_CACHE"
    size_gb: 10
    read_only: true
    mount_path: "/mnt/git-cache"
    commands:
      - type: "shell"
        args: ["git", "clone", "--mirror", "https://github.com/myorg/myrepo", "/mnt/git-cache/myrepo"]
```

Drives are created by snapshot-builder, chunked for lazy loading, and attached
to every VM. `read_only: false` drives get a fresh copy per allocation.

### 4. `credentials` — secrets injected into the VM

```yaml
credentials:
  secrets:
    - name: "api-key"
      secret_name: "projects/my-project/secrets/api-key/versions/latest"
      target: "api.key"
```

Secrets are fetched from GCP Secret Manager and placed on a read-only
credentials drive inside the VM.

## `snapshot_commands` reference

| `type` | `args` | Notes |
|---|---|---|
| `git-clone` | `[url, branch]` | Clones repo into `/workspace`; uses token if configured |
| `gcp-auth` | `[service-account-email]` | Authenticates `gcloud` as the given service account |
| `shell` | `[command, arg1, ...]` | Runs arbitrary shell command inside the VM |
| `exec` | `[binary, arg1, ...]` | Runs a binary directly (no shell) |

The `WorkloadKey` (used for pool matching and GCS routing) is a 16-char SHA256 hash of the sorted `snapshot_commands` and `drives` list. Two configs with the same commands and drives produce the same key and share snapshot chunks in GCS.
