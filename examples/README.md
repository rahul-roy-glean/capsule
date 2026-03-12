# Examples

This directory contains deployable Capsule examples. Each subdirectory is a
starting point for a different workload shape, not just sample prose.

## How To Use These Examples

1. Pick the example closest to your use case.
2. Copy its `onboard.yaml` to a working config.
3. Edit your project, region, and workload-specific values.
4. Run:

```bash
make onboard CONFIG=<your-config>.yaml
```

If you are new to the deployment flow, start with [../docs/setup.md](../docs/setup.md).

## Which Example Should I Start From?

| Example | Best for |
|---|---|
| [ai-sandbox](ai-sandbox/) | per-request isolated execution with optional session resume |
| [dev-environment](dev-environment/) | persistent browser IDE or remote dev sessions |
| [ci-git-cache](ci-git-cache/) | fast git checkout and repository-local cache seeding |
| [ci-bazel-remote-exec](ci-bazel-remote-exec/) | Bazel, Buildbarn, and overlay-backed cache patterns |
| [ci-gitlab-runners](ci-gitlab-runners/) | CI runner processes that differ mainly in `start_command` |
| [afs](afs/) | AFS-style sandbox service with a prebuilt image and session support |

## Fields You Will Usually Edit First

Across most examples, the first fields to customize are:

- `platform.gcp_project`
- `platform.region`
- `platform.zone`
- `microvm`
- `hosts`
- `workload.base_image`
- `workload.layers`
- `workload.start_command`
- `session`

## Shared Primitives

Capsule is intentionally generic. Most examples are combinations of the same
core primitives:

### `base_image`

The Docker image Capsule converts into a Firecracker rootfs and augments with
the platform shim.

### `layers`

Warmup or build steps that run during snapshot creation and become part of the
saved VM state.

### `start_command`

The process Capsule launches after restore. This is how CI runners, services,
and IDE servers all fit the same runtime model.

### `drives`

Attached block devices used for caches, credentials, seed content, or writable
per-allocation storage.

### `session`

Controls whether VM state is paused and later resumed across requests or
connections.

## Supported Example Wrapper Surface

The current example wrapper format supports:

- `platform`
- `microvm`
- `hosts`
- `workload.base_image`
- `workload.layers`
- `workload.config`
- `workload.start_command`
- `session`

Credentialed workloads should currently express mounted data and runtime auth
through:

- `workload.layers[].drives`
- `workload.config.auth`

## Notes On Firecracker References

Examples still refer to Firecracker where it describes the underlying runtime,
for example rootfs conversion, MMDS, or microVM behavior. Those references are
technical, not product branding.
