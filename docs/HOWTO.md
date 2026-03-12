# How-To Guides

This document collects practical recipes for common Capsule operations once a
deployment is already running.

## Before You Start

Most commands below assume these environment variables are set:

```bash
export CONTROL_PLANE_BASE="http://CONTROL_PLANE:8080"
export API_TOKEN="your-api-token"
```

For commands that use host-side proxy endpoints, you will also need:

```bash
export HOST_HTTP="HOST_IP:8080"
export RUNNER_ID="runner-..."
```

Authenticated control-plane requests should generally include:

```bash
-H "Authorization: Bearer ${API_TOKEN}"
```

## From Zero To Live

For a new deployment, follow [setup.md](setup.md). This file assumes the
control plane and host fleet are already up.

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
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  --data @layered-config.json
```

Useful fields in the response:

- `config_id`
- `leaf_workload_key`
- resolved layer metadata

## Trigger And Inspect Builds

Trigger a build:

```bash
curl -sS -X POST \
  "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}/build" \
  -H "Authorization: Bearer ${API_TOKEN}"
```

Inspect the current state:

```bash
curl -sS \
  "${CONTROL_PLANE_BASE}/api/v1/layered-configs/${CONFIG_ID}" \
  -H "Authorization: Bearer ${API_TOKEN}"
```

Useful variants:

- force rebuild: `POST /api/v1/layered-configs/{config_id}/build?force=true`
- clean rebuild: `POST /api/v1/layered-configs/{config_id}/build?clean=true`
- refresh one layer: `POST /api/v1/layered-configs/{config_id}/layers/{layer_name}/refresh`

## Allocate A Runner

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/allocate" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"workload_key\":\"${WORKLOAD_KEY}\"}"
```

Common allocation fields:

- `workload_key`
- `session_id`
- `snapshot_tag`
- `network_policy_preset`

## Check Runner Status

```bash
curl -sS \
  "${CONTROL_PLANE_BASE}/api/v1/runners/status?runner_id=${RUNNER_ID}" \
  -H "Authorization: Bearer ${API_TOKEN}"
```

Typical responses:

- `202` while the VM is still starting
- `200` once the runner is ready
- `404` after release or when the ID is unknown

## Access The Workload Service

Once you have the host address from allocation, use the host-side proxy path:

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
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"runner_id\":\"${RUNNER_ID}\"}"
```

Reconnect or resume:

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/connect" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"runner_id\":\"${RUNNER_ID}\"}"
```

If you want automatic session pickup on first allocation, pass `session_id`
during `allocate`.

## Release A Runner

```bash
curl -sS -X POST "${CONTROL_PLANE_BASE}/api/v1/runners/release" \
  -H "Authorization: Bearer ${API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"runner_id\":\"${RUNNER_ID}\"}"
```

## Roll Out A Host Image Update

Build a new host image:

```bash
make release-host-image \
  PROJECT_ID="${PROJECT_ID}" \
  REGION="${REGION}" \
  ZONE="${ZONE}" \
  ENV="${ENVIRONMENT}"
```

Then start the rolling update:

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

Apply new host-count bounds through Terraform:

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
curl -sS \
  "${CONTROL_PLANE_BASE}/api/v1/versions/fleet?workload_key=${WORKLOAD_KEY}" \
  -H "Authorization: Bearer ${API_TOKEN}"
```

This reports the desired and current version for each host for the given
workload key.

## Debug Host Health

Inspect recent host logs:

```bash
gcloud compute ssh "${INSTANCE}" \
  --zone="${ZONE}" \
  --project="${PROJECT_ID}" -- \
  "sudo journalctl -u capsule-manager --no-pager -n 100"
```

Useful host checks:

```bash
curl http://HOST_IP:8080/health
curl http://HOST_IP:8080/metrics
```

For deeper runtime and failure-mode guidance, continue with
[operations.md](operations.md).
