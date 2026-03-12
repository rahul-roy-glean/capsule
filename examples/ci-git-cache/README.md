# Example: CI With Git Cache

Use this example when you want faster repository checkout by seeding a git cache
into the VM and cloning with `git clone --reference`.

## What This Example Does

This pattern creates a read-only drive containing one or more mirrored
repositories. At runtime, the workload clones from the network only for objects
that are not already present in the seeded cache.

## Core Idea

The cache drive is declared in config:

```yaml
drives:
  - drive_id: "git_cache"
    label: "GIT_CACHE"
    size_gb: 10
    read_only: true
    mount_path: "/mnt/git-cache"
```

Then the workload uses it from an `init_command`:

```yaml
init_commands:
  - type: "shell"
    args: ["bash", "-c", "git clone --reference /mnt/git-cache/myrepo ..."]
```

## Why Use This Pattern

- faster checkout for repeated builds
- lower network usage
- explicit cache behavior in config rather than hardcoded platform logic
- easy extension to other cache-like mounted content

## What You Need To Edit

Before using this example, update:

- `platform.gcp_project`
- the mirrored repository URLs
- the clone destination
- any additional cache population commands

## Onboard

```bash
cp examples/ci-git-cache/onboard.yaml my-git-cache.yaml
# Edit the fields described above
make onboard CONFIG=my-git-cache.yaml
```

## Good Fit

This example is a good starting point for:

- GitHub Actions or other CI runners that repeatedly clone the same repo
- mono-repo build environments
- workloads that benefit from read-only seed content attached as a drive
