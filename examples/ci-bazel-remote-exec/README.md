# Example: Bazel + Buildbarn Remote Execution

This example demonstrates how the previously-hardcoded Bazel constructs
(`BazelConfig`, `ensureCredentialsImage`, `repo_cache_upper`, Buildbarn certs)
are expressed using only the generic platform primitives.

## What was hardcoded before

The old config used dedicated platform constructs:

```yaml
# OLD — hardcoded Bazel/Buildbarn support
bazel:
  warmup_targets: "//..."
  repo_cache_upper_size_gb: 10
  buildbarn:
    certs_dir: "/etc/glean/ci/certs/buildbarn"
    certs_mount: "/etc/bazel-firecracker/certs/buildbarn"
    certs_image_size_mb: 32
```

This required:
- `BazelConfig` struct (11 fields) in `pkg/runner/types.go`
- `ensureCredentialsImage()` in `pkg/runner/manager.go` (builds ext4 at host startup)
- `createExt4Image()` hardcoded in `AllocateRunner` for `repo_cache_upper`
- `mountBuildbarnCerts()` in thaw-agent
- `setupRepoCacheOverlay()` in thaw-agent
- 6 dedicated flags in `firecracker-manager`
- 8 dedicated flags in `thaw-agent`

## How it works now: just `DriveSpec` + `SnapshotCommand`

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

GitHub Actions registration is a `start_command` — no `IntegrationName`,
no `ci.Adapter`, no `CIConfig`:

```yaml
start_command:
  command: ["/home/runner/config.sh", "--url", "https://github.com/myorg/myrepo",
            "--token", "${CI_RUNNER_TOKEN}", "--ephemeral"]
  env:
    CI_RUNNER_TOKEN: "${ci_runner_token}"
```

## What gets deleted

| Old construct | Replaced by |
|---|---|
| `BazelConfig` struct (11 fields) | `DriveSpec` entries in `LayeredConfig` |
| `ensureCredentialsImage()` | `DriveSpec` with `commands` |
| `createExt4Image()` for repo_cache_upper | `DriveSpec` with `read_only: false` |
| `mountBuildbarnCerts()` in thaw-agent | Generic drive auto-mount + `init_command` |
| `setupRepoCacheOverlay()` in thaw-agent | `init_command` shell overlay mount |
| `setupCredentialSymlinks()` in thaw-agent | `init_command` shell symlinks |
| `CIConfig` struct (9 fields) | `start_command` + MMDS token injection |
| `ci.Adapter` interface | Direct `cigithub.Client` (nilable) |
| 14+ dedicated flags | Zero — all config in `onboard.yaml` |

## Onboard

```bash
cp examples/ci-bazel-remote-exec/onboard.yaml my-bazel.yaml
# Edit: set gcp_project, repository.url, certs paths
make onboard CONFIG=my-bazel.yaml
```
