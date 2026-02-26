# Example: CI Runners (GitHub Actions)

This example configures the platform as ephemeral GitHub Actions self-hosted runners backed by a pre-warmed Bazel snapshot.

## What the snapshot contains

The golden snapshot is built by:
1. Cloning the repository at the configured branch
2. Running `bazel fetch //...` (configurable via `bazel.warmup_targets`)
3. Freezing the VM — the running Bazel server, analysis graph, and fetched externals are preserved in the snapshot

Each CI job resumes from this frozen state in ~200ms instead of waiting for a full Bazel warm-up.

## Workflow

```
GitHub Actions job triggered
  → Control plane receives webhook
  → Allocates runner from idle pool (~10ms) or restores from golden snapshot (~200ms)
  → thaw-agent registers GitHub runner, signals ready
  → Job runs with warm Bazel cache
  → VM recycled back to paused pool on completion
```

## Configuration

```yaml
ci:
  system: "github-actions"
  github:
    repo: "myorg/myrepo"      # repo-level runner
    # org: "myorg"            # or org-level runner (comment out repo)
    labels:
      - "self-hosted"
      - "firecracker"
      - "bazel"
    ephemeral: true
```

In your workflow file:

```yaml
jobs:
  build:
    runs-on: [self-hosted, firecracker, bazel]
    steps:
      - uses: actions/checkout@v4
      - run: bazel build //...
```

## Pool reuse

When a job finishes, if the VM's `WorkloadKey` matches a waiting paused VM in the local pool, the next job resumes that paused VM in ~10ms (no GCS fetch required). The Bazel server stays warm between jobs.

## Onboard

```bash
cp examples/ci-github-actions/onboard.yaml my-ci.yaml
# Edit my-ci.yaml: set platform.gcp_project, repository.url, ci.github.repo
make onboard CONFIG=my-ci.yaml
```
