# Example: AFS Sandbox Service

This example models an AFS-style sandbox service on top of Capsule. `AFS` here
is an example workload name, not a special Capsule subsystem.

## What This Example Shows

The AFS example demonstrates a small service-oriented workload with:

- a prebuilt application image
- a lightweight runtime layer for per-user directories
- a web service launched through `uvicorn`
- session-backed pause and resume
- an explicit `runner_user` and workspace sizing model

## Files In This Directory

- `onboard.yaml`: full service example with `start_command`
- `onboard-native.yaml`: the same workload shape without the service launch step

Use `onboard.yaml` when you want the end-to-end hosted service behavior. Use
`onboard-native.yaml` when you want the same base image and runtime layer as a
starting point for your own launch process.

## What You Need To Edit

Before using this example, update:

- `platform.gcp_project`
- `platform.region`
- `platform.zone`
- `workload.base_image`
- the `start_command` if your service entrypoint differs
- any session TTL or workspace sizing values

## Notable Config Choices

This example intentionally includes:

- `runner_user: "user"` to match the image's expected runtime user
- `workspace_size_gb` for user-managed files
- `session.enabled: true` with `auto_pause: true` for resumable sessions
- a small tier by default for lightweight service testing

## Onboard

```bash
cp examples/afs/onboard.yaml my-afs.yaml
# Edit the fields described above
make onboard CONFIG=my-afs.yaml
```

## Good Fit

Start from this example when you want a small, stateful service workload that is
closer to an application sandbox than to a CI runner or browser IDE.
