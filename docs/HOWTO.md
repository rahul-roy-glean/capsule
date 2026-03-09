# How-To Guides

Operational recipes for the current layered-workload runtime.

## Deploy From Scratch

Follow [docs/setup.md](setup.md). That is the supported zero-to-live path.

## Register A Layered Config

Create a config payload:

```bash
cat > layered-config.json <<'EOF'
{
  "display_name": "my-workload",
  "base_image": "ubuntu:22.04",
  "layers": [
    {
      "name": "deps",
      "init_commands": [
        {"type": "shell", "args": ["bash", "-lc", "apt-get update && apt-get install -y curl"]}
      ]
    }
  ],
  "config": {
    "tier": "m",
    "auto_pause": true,
    "ttl": 300,
    "auto_rollout": true
  },
  "start_command": {
    "command": ["bash", "-lc", "python3 -m http.server 8080"],
    "port": 8080,
    "health_path": "/"
  }
}
EOF

curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/layered-configs" \
  -H "Content-Type: application/json" \
  --data @layered-config.json
```

This returns:

- `config_id`
- `leaf_workload_key`
- materialized layer metadata

## Trigger A Build

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}/build"
```

Inspect the current state:

```bash
curl -sS "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}"
```

Useful variants:

- force rebuild: `POST /api/v1/layered-configs/{config_id}/build?force=true`
- clean rebuild: `POST /api/v1/layered-configs/{config_id}/build?clean=true`
- refresh one layer: `POST /api/v1/layered-configs/{config_id}/layers/{layer_name}/refresh`

## Allocate A Runner

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/allocate" \
  -H "Content-Type: application/json" \
  -d "{\"workload_key\":\"${WORKLOAD_KEY}\"}"
```

Fields supported by the current API include:

- `workload_key`
- `session_id`
- `snapshot_tag`
- `network_policy_preset`

## Check Runner Status

```bash
curl -sS "${CONTROL_PLANE_BASE}/api/v1/runners/status?runner_id=${RUNNER_ID}"
```

Expected responses:

- `202` while the VM is still starting
- `200` once the runner is ready
- `404` after release or if the ID is unknown

## Access The User Service

The allocate API returns the host HTTP address. Use the host proxy path:

```bash
curl -sS "http://${HOST_HTTP}/api/v1/runners/${RUNNER_ID}/proxy/"
```

Other useful host-side endpoints:

- `POST /api/v1/runners/{id}/exec`
- `GET /api/v1/runners/{id}/service-logs`
- `POST /api/v1/runners/{id}/pause`
- `POST /api/v1/runners/{id}/connect`
- `POST /api/v1/runners/{id}/checkpoint`

## Pause And Resume A Session

Pause:

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/pause" \
  -H "Content-Type: application/json" \
  -d "{\"runner_id\":\"${RUNNER_ID}\"}"
```

Resume or reconnect:

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/connect" \
  -H "Content-Type: application/json" \
  -d "{\"runner_id\":\"${RUNNER_ID}\"}"
```

For auto-resume on first allocation, pass `session_id` during allocate.

## Release A Runner

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/release" \
  -H "Content-Type: application/json" \
  -d "{\"runner_id\":\"${RUNNER_ID}\"}"
```

## Roll Out A Host Image Update

Build a new image:

```bash
make release-host-image \
  PROJECT_ID="${PROJECT_ID}" \
  REGION="${REGION}" \
  ZONE="${ZONE}" \
  ENV="${ENVIRONMENT}"
```

Then start a MIG rolling update:

```bash
make mig-rolling-update \
  PROJECT_ID="${PROJECT_ID}" \
  REGION="${REGION}" \
  ENV="${ENVIRONMENT}"
```

Monitor progress:

```bash
gcloud compute instance-groups managed list-instances "${HOST_MIG_NAME}" \
  --region="${REGION}" \
  --project="${PROJECT_ID}"
```

## Scale The Fleet

Update Terraform with new bounds:

```bash
terraform -chdir=deploy/terraform apply \
  -var="project_id=${PROJECT_ID}" \
  -var="region=${REGION}" \
  -var="zone=${ZONE}" \
  -var="environment=${ENVIRONMENT}" \
  -var="db_password=${DB_PASSWORD}" \
  -var="min_hosts=4" \
  -var="max_hosts=20"
```

## Check Fleet Convergence

```bash
curl -sS "${CONTROL_PLANE_BASE}/api/v1/versions/fleet?workload_key=${WORKLOAD_KEY}"
```

This reports the desired and current version for each host for the specified workload key.

## Debug Host Health

```bash
gcloud compute ssh "${INSTANCE}" --zone="${ZONE}" --project="${PROJECT_ID}" -- \
  "sudo journalctl -u firecracker-manager --no-pager -n 100"
```

Useful host checks:

```bash
curl http://HOST_IP:8080/health
curl http://HOST_IP:8080/metrics
```

## Enable GitHub Webhooks Later

GitHub integration is optional. If you need it later:

1. create a GitHub App and store the private key in Secret Manager
2. update the host metadata inputs in Terraform (`github_app_id`, `github_app_secret`, `github_repo`, `github_org`)
3. create a real `github-credentials` Kubernetes secret with the webhook secret
4. configure the GitHub webhook to hit `${CONTROL_PLANE_BASE}/webhook/github`

The runtime itself remains workload-key based either way.
