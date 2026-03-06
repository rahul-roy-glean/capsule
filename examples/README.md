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
frameworks. It provides five generic primitives:

### 1. `base_image` — bring your own Docker image

```yaml
workload:
  base_image: "us-docker.pkg.dev/my-project/images/my-runtime:latest"
```

Any Docker image — from public registries, Artifact Registry, or your own. The
platform converts it to a Firecracker rootfs and installs the system components
(thaw-agent, systemd, networking). Each workload can specify a different image.

Two workloads using the same `base_image` share the platform layer (same hash);
different images get their own. The hash chain ensures changing the image
triggers a rebuild while keeping user layer hashes stable.

### 2. `layers` — what to bake into the snapshot

```yaml
layers:
  - name: "workspace"
    init_commands:
      - type: "shell"
        args: ["bash", "-c", "git clone --depth=1 -b main https://github.com/myorg/myrepo /workspace"]
      - type: "shell"
        args: ["bazel", "fetch", "//..."]
    refresh_interval: "on_push"
```

Layers run inside the VM during snapshot building. The result is frozen into the
snapshot. Multiple layers form a hash chain — changing an earlier layer triggers
rebuilds of all downstream layers. `refresh_interval` controls automatic
rebuilds (`"on_push"`, `"6h"`, `"daily"`).

### 3. `start_command` — what the VM does after restore

```yaml
start_command:
  command: ["/home/runner/config.sh", "--token", "${CI_RUNNER_TOKEN}", "--ephemeral"]
  port: 8080
  health_path: "/health"
  env:
    CI_RUNNER_TOKEN: "${ci_runner_token}"
```

This is how CI runner registration, user services, and function runtimes all
work. The platform starts the command and waits for the health check.

### 4. `drives` — block devices attached to the VM

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
to every VM. `read_only: false` drives get a fresh copy per allocation. A
default 50GB workspace drive is auto-injected if no drives are declared.

### 5. `credentials` — secrets injected into the VM

```yaml
credentials:
  secrets:
    - name: "api-key"
      secret_name: "projects/my-project/secrets/api-key/versions/latest"
      target: "api.key"
```

Secrets are fetched from GCP Secret Manager and placed on a read-only
credentials drive inside the VM.

## `init_commands` reference

| `type` | `args` | Notes |
|---|---|---|
| `shell` | `[command, arg1, ...]` | Runs arbitrary shell command inside the VM |
| `gcp-auth` | `[service-account-email]` | Authenticates `gcloud` as the given service account |
| `exec` | `[binary, arg1, ...]` | Runs a binary directly (no shell) |

The `WorkloadKey` (used for pool matching and GCS routing) is derived from the
leaf layer hash. The hash chain includes `base_image`, all layer commands, and
drives — so two configs with the same image and commands produce the same key
and share snapshot chunks in GCS.
