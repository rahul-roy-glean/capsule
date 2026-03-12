# Example: Bazel + Buildbarn Remote Execution

Use this example when you want a CI or build workload that combines:

- Bazel repository or artifact cache seeding
- Buildbarn credentials or certificate material
- writable per-runner overlay storage

The important point is that all of this is expressed with Capsule's generic
primitives rather than special-purpose Bazel logic in the platform.

## What This Example Shows

This example combines:

- read-only seeded drives for reusable cache state
- writable per-allocation drives for ephemeral build output
- `init_commands` for overlay and filesystem setup
- `start_command` for the actual runner or build process

## Key Patterns

### Seeded artifact cache

A read-only drive is populated during snapshot build and attached to each VM:

```yaml
drives:
  - drive_id: "artifact_cache_seed"
    label: "ARTIFACT_CACHE_SEED"
    size_gb: 20
    read_only: true
    mount_path: "/mnt/artifact-cache-seed"
```

### Writable upper layer

A second writable drive is created fresh per allocation:

```yaml
drives:
  - drive_id: "artifact_cache_upper"
    label: "ARTIFACT_CACHE_UPPER"
    size_gb: 10
    read_only: false
    mount_path: "/mnt/artifact-cache-upper"
```

### Overlay setup

The overlay itself is assembled with `init_commands`, which keeps the behavior
explicit in config instead of hidden in Go code.

### Credentials drive

Buildbarn certificates, `.netrc`, or other auth material can be staged through a
small read-only drive and then symlinked into the expected runtime paths.

## What You Need To Edit

Before using this example, update:

- `platform.gcp_project`
- `workload.base_image`
- any Buildbarn host, cert, or credential paths
- cache sizes and mount paths
- the final `start_command`

## Onboard

```bash
cp examples/ci-bazel-remote-exec/onboard.yaml my-bazel.yaml
# Edit the fields described above
make onboard CONFIG=my-bazel.yaml
```

## Why This Example Matters

This example is a good reference when you want to express complex build runtime
state using only:

- `base_image`
- `layers`
- `drives`
- `init_commands`
- `start_command`

If your workload needs similar cache or credential behavior, this is usually the
best example to borrow from first.
