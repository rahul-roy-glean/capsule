# GCP Setup

The recommended path for a clean deployment is the unified `onboard.yaml` flow.

## Quickstart

1. Copy the root config or an example config:

```bash
cp onboard.yaml my-config.yaml
```

2. Edit the supported fields:

- `platform.gcp_project`
- `platform.region`
- `platform.zone`
- `platform.environment`
- `microvm`
- `hosts`
- `workload.base_image`
- `workload.layers`
- `workload.config`
- `workload.start_command`
- `session`

Optional advanced fields:

- `platform.terraform_state_bucket`
- `platform.terraform_state_prefix`
- `platform.db_password`
- `platform.github_webhook_secret`

3. Preview the full deployment:

```bash
make onboard-plan CONFIG=my-config.yaml
```

4. Run it:

```bash
make onboard CONFIG=my-config.yaml
```

## What `make onboard` Does

The onboard pipeline runs these steps:

1. validate config and local prerequisites
2. create or reuse the Terraform state bucket
3. bootstrap Terraform with `min_hosts=0` and `use_custom_host_image=false`
4. build the `firecracker-host` image with Packer
5. deploy the control plane to GKE via Helm
6. build and upload builder artifacts:
   - `snapshot-builder`
   - `thaw-agent`
   - `rootfs.img`
   - `kernel.bin`
7. register the workload config with the control plane
8. trigger the layered build and wait for the leaf workload to become active
9. finalize Terraform with the custom host image and the real control-plane address
10. verify host registration and perform a real runner allocation/release probe

## Supported Wrapper Schema

`cmd/onboard` now translates the example wrapper format into the control-plane's
`LayeredConfig` API.

Two input styles are accepted:

- modern layered configs via `workload.base_image`, `workload.layers`, and
  `workload.config`
- legacy `workload.snapshot_commands`, which are translated into a single layer named
  `workload`

## Current Scope

The onboard flow is definitive for the common deployment path, but its scope is explicit:

- it supports layered workloads, session settings, Terraform bootstrap/finalize, control
  plane deployment, and snapshot builds
- it uses the Helm chart as the canonical Kubernetes deployment surface
- it rejects unsupported top-level `credentials` translation rather than silently
  pretending to support it

For credentialed workloads today, express mounted data and runtime auth inside
`workload.layers[].drives` and `workload.config.auth`.

## Re-Runs

Re-running `make onboard` is supported and should be the normal way to reconcile changes:

- the Terraform state bucket is reused
- the DB password is reused from existing tfvars unless you explicitly override it
- the layered config is upserted by content-derived `config_id`
- the build step re-registers and rebuilds the workload definition

## Troubleshooting

Use these commands first:

```bash
kubectl -n firecracker-runner get pods
kubectl -n firecracker-runner logs deploy/control-plane --tail=100
terraform -chdir=deploy/terraform output
```

If the host fleet is up but workloads fail to start:

```bash
gcloud compute instance-groups managed list-instances fc-runner-dev-hosts --region=us-central1 --project=YOUR_PROJECT
gcloud compute ssh INSTANCE --zone=us-central1-a --project=YOUR_PROJECT -- sudo journalctl -u firecracker-manager -n 100
```

Operational recipes for the deployed system live in [docs/HOWTO.md](HOWTO.md) and
[docs/operations.md](operations.md).
