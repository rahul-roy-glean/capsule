# GCP Setup

This guide covers the supported deployment path for Capsule on GCP using the
repository's `onboard.yaml` workflow.

## Before You Start

You should have:

- a GCP project with billing enabled
- permissions to create GKE, Compute Engine, Cloud SQL, Artifact Registry, and
  GCS resources
- local access to `gcloud`, `kubectl`, `terraform`, `packer`, `docker`, and `make`
- a workload image or a starting point from `examples/`

For local tooling setup, see [DEV_SETUP.md](DEV_SETUP.md).

## Deployment Overview

The `make onboard` flow is designed to take you from a config file to a working
Capsule deployment. It:

1. validates your config and local prerequisites
2. creates or reuses the Terraform state bucket
3. bootstraps Terraform with zero hosts and a temporary Ubuntu host image
4. builds the `capsule-host` image with Packer
5. deploys the control plane to GKE via Helm
6. uploads build artifacts:
   - `snapshot-builder`
   - `capsule-thaw-agent`
   - `rootfs.img`
   - `kernel.bin`
7. registers the workload config with the control plane
8. triggers the layered build and waits for the leaf workload to become active
9. finalizes Terraform with the real control-plane address and custom host image
10. verifies host registration and performs a real allocation/release probe

## Quickstart

1. Copy the root config or one of the example configs.

```bash
cp onboard.yaml my-config.yaml
```

2. Edit the required fields.

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

3. Preview the deployment.

```bash
make onboard-plan CONFIG=my-config.yaml
```

4. Apply the deployment.

```bash
make onboard CONFIG=my-config.yaml
```

5. Verify the control plane and host fleet.

```bash
kubectl -n capsule get pods
terraform -chdir=deploy/terraform output
```

## What Success Looks Like

After a successful run you should have:

- a `capsule` namespace with a healthy control-plane deployment
- a Cloud SQL instance, GKE cluster, GCS snapshot bucket, and Artifact Registry
- a managed instance group of Capsule hosts
- a registered workload config and at least one completed snapshot build
- a successful end-to-end allocation probe from the onboard flow

## Supported Config Surface

`cmd/onboard` translates the wrapper format used by `onboard.yaml` and
`examples/*/onboard.yaml` into the control-plane `LayeredConfig` API.

Supported workload input styles:

- modern layered configs via `workload.base_image`, `workload.layers`, and
  `workload.config`
- legacy `workload.snapshot_commands`, which are translated into a single layer
  named `workload`

Supported top-level sections:

- `platform`
- `microvm`
- `hosts`
- `workload`
- `session`

Credentialed workloads should currently express mounted data and runtime auth
inside:

- `workload.layers[].drives`
- `workload.config.auth`

The top-level `credentials` wrapper is still reserved and is not treated as a
fully supported abstraction by the onboard flow.

## Re-Runs And Change Management

Re-running `make onboard` is the normal way to reconcile changes.

- the Terraform state bucket is reused
- the database password is reused from generated tfvars unless you override it
- layered configs are upserted by content-derived `config_id`
- snapshot builds are re-triggered from the updated workload definition

In practice, you can treat the onboard flow as the supported "apply my current
desired state" path for development and early-stage environments.

## Troubleshooting

Start with the control plane and Terraform outputs:

```bash
kubectl -n capsule get pods
kubectl -n capsule logs deploy/control-plane --tail=100
terraform -chdir=deploy/terraform output
```

If hosts are present but workloads do not start:

```bash
gcloud compute instance-groups managed list-instances capsule-dev-hosts \
  --region=us-central1 \
  --project=YOUR_PROJECT

gcloud compute ssh INSTANCE \
  --zone=us-central1-a \
  --project=YOUR_PROJECT -- \
  sudo journalctl -u capsule-manager -n 100
```

If a workload build appears stuck, inspect:

- control-plane logs for the relevant `config_id` or `workload_key`
- GCS build artifacts under the snapshot bucket
- `GET /api/v1/layered-configs/{config_id}` and `GET /api/v1/snapshots`

For day-2 operations and API recipes, continue with:

- [HOWTO.md](HOWTO.md)
- [operations.md](operations.md)
