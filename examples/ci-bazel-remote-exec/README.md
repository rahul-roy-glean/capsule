# Example: Bazel + Buildbarn Remote Execution

This example demonstrates how Bazel constructs (artifact cache, Buildbarn certs,
repo cache overlay) are expressed using only the generic platform primitives:
`DriveSpec`, `SnapshotCommand`, and `init_commands`.

## How it works: `DriveSpec` + `SnapshotCommand`

### Artifact cache (seed + overlay)

The read-only seed is a `DriveSpec` populated during snapshot building:

```yaml
drives:
  - drive_id: "artifact_cache_seed"
    label: "ARTIFACT_CACHE_SEED"
    size_gb: 20
    read_only: true
    mount_path: "/mnt/artifact-cache-seed"
    commands:
      - type: "shell"
        args: ["bazel", "fetch", "--repository_cache=/mnt/artifact-cache-seed", "//..."]
```

The per-runner writable upper is also a `DriveSpec`:

```yaml
  - drive_id: "artifact_cache_upper"
    label: "ARTIFACT_CACHE_UPPER"
    size_gb: 10
    read_only: false   # → manager creates fresh copy per allocation
    mount_path: "/mnt/artifact-cache-upper"
```

The overlayfs mount is an `init_command`:

```yaml
init_commands:
  - type: "shell"
    args: ["bash", "-c", "mount -t overlay overlay -o lowerdir=/mnt/artifact-cache-seed,upperdir=/mnt/artifact-cache-upper/upper,workdir=/mnt/artifact-cache-upper/work /mnt/ephemeral/caches/repository"]
    run_as_root: true
```

### Credentials (Buildbarn certs, .netrc, git-credentials)

A single `DriveSpec` replaces `ensureCredentialsImage`, `mountBuildbarnCerts`,
and `setupCredentialSymlinks`:

```yaml
drives:
  - drive_id: "credentials"
    label: "CREDENTIALS"
    size_gb: 1
    read_only: true
    mount_path: "/mnt/credentials"
    commands:
      - type: "shell"
        args: ["bash", "-c", "cp -r /host-certs/* /mnt/credentials/"]
        run_as_root: true
```

Symlinks are `init_commands`:

```yaml
init_commands:
  - type: "shell"
    args: ["bash", "-c", "ln -sf /mnt/credentials/netrc ~/.netrc"]
```

### Runner registration

GitHub Actions registration is a `start_command` -- no special CI config needed:

```yaml
start_command:
  command: ["/home/runner/config.sh", "--url", "https://github.com/myorg/myrepo",
            "--token", "${CI_RUNNER_TOKEN}", "--ephemeral"]
  env:
    CI_RUNNER_TOKEN: "${ci_runner_token}"
```

## What the generic primitives replace

| Old construct | Replaced by |
|---|---|
| Hardcoded artifact-cache logic | `DriveSpec` entries in `LayeredConfig` |
| Hardcoded credentials-image builder | `DriveSpec` with `commands` |
| Hardcoded per-runner ext4 creation | `DriveSpec` with `read_only: false` |
| Hardcoded cert-mount in thaw-agent | Generic drive auto-mount + `init_command` |
| Hardcoded overlay setup in thaw-agent | `init_command` shell overlay mount |
| Hardcoded symlink setup in thaw-agent | `init_command` shell symlinks |
| Hardcoded CI config struct | `start_command` + MMDS token injection |
| Dedicated CLI flags | Zero -- all config in `onboard.yaml` |

## Onboard

```bash
cp examples/ci-bazel-remote-exec/onboard.yaml my-bazel.yaml
# Edit my-bazel.yaml: set platform.gcp_project and workload values
make onboard CONFIG=my-bazel.yaml
```
