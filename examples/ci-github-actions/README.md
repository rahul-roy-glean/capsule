# Example: CI Runners (GitHub Actions)

This example configures the platform as ephemeral GitHub Actions self-hosted runners with a pre-warmed Bazel snapshot.

## How it works (no special CI plumbing needed)

GitHub Actions runner registration is just a `start_command` — the runner binary
is installed during snapshot building, and the registration token is injected via
MMDS at allocation time. The platform doesn't need to know it's running GitHub
Actions; it just starts the command and waits for it to become healthy.

## What the snapshot contains

The golden snapshot is built by:
1. Cloning the repository at the configured branch
2. Running `bazel fetch //...` to warm the analysis cache and fetch externals
3. Freezing the VM — the running Bazel server, analysis graph, and fetched externals are preserved

Each CI job resumes from this frozen state in ~200ms instead of waiting for a full Bazel warm-up.

## Workflow

```
GitHub Actions job triggered
  → Control plane receives webhook
  → Allocates runner from idle pool (~10ms) or restores from golden snapshot (~200ms)
  → thaw-agent runs start_command (config.sh --ephemeral)
  → Runner registers with GitHub, picks up the queued job
  → Job runs with warm Bazel cache
  → VM recycled back to paused pool on completion
```

## Key insight: no CI-specific config

The snapshot *is* a GitHub Actions runner because:
1. The base image has the runner binary installed
2. The `start_command` runs `config.sh` with the registration token
3. The control-plane injects the token via MMDS (`ci_runner_token`)

Swapping to GitLab CI or Buildkite is just changing the `start_command` and
base image -- no platform code changes needed.

## Configuration

```yaml
workload:
  base_image: "us-docker.pkg.dev/my-project/images/ci-runner:latest"

  layers:
    - name: "workspace"
      init_commands:
        - type: "shell"
          args: ["bash", "-c", "git clone --depth=1 -b main https://github.com/myorg/myrepo /workspace"]
        - type: "shell"
          args: ["bazel", "fetch", "//..."]

  start_command:
    command: ["/home/runner/config.sh", "--url", "https://github.com/myorg/myrepo",
              "--token", "${CI_RUNNER_TOKEN}", "--ephemeral", "--unattended",
              "--labels", "self-hosted,firecracker,bazel"]
    env:
      CI_RUNNER_TOKEN: "${ci_runner_token}"
    run_as: "runner"
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
# Edit my-ci.yaml: set platform.gcp_project and workload/start_command values
make onboard CONFIG=my-ci.yaml
```
