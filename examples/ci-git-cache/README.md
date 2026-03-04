# Example: CI with Git-Cache Reference Cloning

This example demonstrates how the previously-hardcoded git-cache construct is
expressed using only `DriveSpec` + `SnapshotCommand`. No platform code changes
needed.

## What was hardcoded before

The old config used dedicated platform constructs:

```yaml
# OLD — 7 dedicated fields in BazelConfig + MMDS GitCache struct
bazel:
  git_cache:
    enabled: true
    dir: "/mnt/data/git-cache"
    image: "/mnt/data/git-cache.img"
    mount: "/mnt/git-cache"
    repos:
      github.com/myorg/myrepo: myrepo
    workspace_dir: "/mnt/ephemeral/workdir"
    pre_cloned_path: "/workspace/myorg/myrepo"
```

This required:
- `BazelConfig.GitCache*` (7 fields) in `pkg/runner/types.go`
- `Manager.gitCacheImage` field in `pkg/runner/manager.go`
- `MMDSData.Latest.GitCache` struct (5 fields) — duplicated in both `pkg/runner/types.go` and `cmd/thaw-agent/main.go`
- `mountGitCache()` — 30 lines of drive mounting code
- `setupWorkspaceFromGitCache()` — 80 lines of `git clone --reference` logic
- `findGitCacheReference()` — 40 lines of cache lookup
- `setupGitAlternates()` — 10 lines of fallback logic
- 3 dedicated `thaw-agent` flags (`--git-cache-device`, `--git-cache-mount`, `--git-cache-label`)
- 8 dedicated `firecracker-manager` flags
- `buildMMDSData` populates `Latest.GitCache.*` from `BazelConfig`

## How it works now: just `DriveSpec`

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

### What about MMDS `GitCache` metadata?

Not needed. The drive's `mount_path` tells thaw-agent where to mount it. The
`init_commands` know where the cache is because the config author wrote the
paths. There's no need for a 5-field MMDS struct to pass information that's
already in the config.

## Why this is better

1. **Adding a new cache type** (e.g., Docker layer cache, npm cache) is just
   another `DriveSpec` — no platform code changes
2. **Adding repos to the cache** is editing `commands` — no flag changes
3. **The workspace setup logic is visible and editable** — it's a shell command
   in the config, not buried in 80 lines of Go in `cmd/thaw-agent/main.go`
4. **No GitHub URL hardcoding** — the old `setupWorkspaceFromGitCache` hardcoded
   `https://github.com/` in the remote URL rewrite

## Onboard

```bash
cp examples/ci-git-cache/onboard.yaml my-git-cache.yaml
# Edit: set gcp_project, repository.url, adjust git_cache repos
make onboard CONFIG=my-git-cache.yaml
```
