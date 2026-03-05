# Example: CI with Git-Cache Reference Cloning

This example demonstrates how a git-cache for fast `actions/checkout` is
expressed using only `DriveSpec` + `SnapshotCommand`. No platform code changes
needed.

## How it works: `DriveSpec`

### The drive

```yaml
drives:
  - drive_id: "git_cache"
    label: "GIT_CACHE"
    size_gb: 10
    read_only: true
    mount_path: "/mnt/git-cache"
    commands:
      - type: "shell"
        args: ["bash", "-c", "git clone --mirror https://github.com/myorg/myrepo /mnt/git-cache/myrepo"]
```

That's it. Snapshot-builder creates the ext4 image, runs `git clone --mirror`
inside it during warmup, chunks it, and stores it in GCS. At runtime, the
manager attaches it as an extension drive. Thaw-agent auto-mounts it by label at
`/mnt/git-cache`.

### The workspace setup

```yaml
init_commands:
  - type: "shell"
    args: ["bash", "-c", "git clone --reference /mnt/git-cache/myrepo ..."]
    run_as_root: true
```

This replaces `setupWorkspaceFromGitCache()`. The `git clone --reference` command
is the same one the old function ran — it's just expressed as a command instead
of being hardcoded in Go.

### What about drive metadata?

Not needed. The drive's `mount_path` tells thaw-agent where to mount it. The
`init_commands` know where the cache is because the config author wrote the
paths. There's no need for a separate metadata struct to pass information that's
already in the config.

## Why this is better

1. **Adding a new cache type** (e.g., Docker layer cache, npm cache) is just
   another `DriveSpec` — no platform code changes
2. **Adding repos to the cache** is editing `commands` — no flag changes
3. **The workspace setup logic is visible and editable** — it's a shell command
   in the config, not buried in 80 lines of Go in `cmd/thaw-agent/main.go`
4. **No URL hardcoding** -- the old approach hardcoded `https://github.com/`
   in the remote URL rewrite

## Onboard

```bash
cp examples/ci-git-cache/onboard.yaml my-git-cache.yaml
# Edit: set gcp_project, repository.url, adjust git_cache repos
make onboard CONFIG=my-git-cache.yaml
```
