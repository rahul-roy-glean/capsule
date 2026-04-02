# Capsule API Reference

Complete API reference for all Capsule services: Access Plane, Control Plane, Host Agent (capsule-manager), and Thaw Agent.

---

## Access Plane

External credential-injecting proxy and policy engine. One instance per tenant.

**Base:** `http://<access-plane>:8080` | **Proxy:** `<access-plane>:3128`

### Authentication

All endpoints except `/healthz`, `/v1/phantom-env`, and `/v1/providers/update-token` require:
```
Authorization: Bearer <hmac-attestation-token>
```
Token format: `base64(json_claims).base64(hmac_sha256(claims, secret))`

### Endpoints

#### `GET /healthz`
Health check. No auth.

**Response:** `{"status": "ok"}`

---

#### `POST /v1/resolve`
Evaluate policy for a tool operation. Returns which execution lane to use.

**Request:**
```json
{
  "actor": {"user_id": "alice", "agent_id": "agent-1"},
  "runner": {"session_id": "s1", "runner_id": "r1", "turn_id": "t1"},
  "tool_family": "github_rest",
  "logical_action": "read_repo",
  "target": {"resource": "repos/org/repo"},
  "risk_hint": "low",
  "write_intent": false
}
```

**Response (200):**
```json
{
  "decision": "allow",
  "selected_lane": "direct_http",
  "decision_reason": "manifest allows operation",
  "implementation_state": "implemented"
}
```

**Response (403):** `{"family": "...", "reason": "..."}`

---

#### `POST /v1/execute/http`
Execute an HTTP request with injected credentials (remote_execution lane). The access plane makes the outbound call.

**Request:**
```json
{
  "runner_id": "r1",
  "session_id": "s1",
  "turn_id": "t1",
  "tool_family": "github_rest",
  "method": "GET",
  "url": "https://api.github.com/repos/org/repo",
  "headers": {"Accept": "application/vnd.github.v3+json"},
  "body": ""
}
```

**Response (200):**
```json
{
  "status_code": 200,
  "headers": {"Content-Type": "application/json"},
  "body": "{...}",
  "audit_correlation_id": "exec-s1-t1-1710801234567"
}
```

Max response body: 10 MB.

---

#### `POST /v1/grants/project`
Create a grant and start a credential-injecting forward proxy (direct_http lane).

**Request:**
```json
{
  "runner_id": "r1",
  "session_id": "s1",
  "turn_id": "t1",
  "tool_family": "github_rest",
  "lane": "direct_http",
  "scope": "repo:read"
}
```

**Response (200):**
```json
{
  "grant_id": "550e8400-...",
  "projection_ref": "127.0.0.1:54321",
  "status": "projected"
}
```

Use `projection_ref` as a forward proxy with `X-Target-URL` header.

---

#### `POST /v1/grants/exchange`
Validate a projected grant is active.

**Request:** `{"grant_id": "...", "runner_id": "r1"}`
**Response:** `{"grant_id": "...", "expires_at": "...", "status": "active"}`

---

#### `POST /v1/grants/refresh`
Extend grant lifetime.

**Request:** `{"grant_id": "...", "runner_id": "r1"}`
**Response:** `{"grant_id": "...", "expires_at": "...", "status": "refreshed"}`

---

#### `POST /v1/grants/revoke`
Revoke grant and stop its proxy.

**Request:** `{"grant_id": "...", "runner_id": "r1", "reason": "turn completed"}`
**Response:** `{"grant_id": "...", "status": "revoked"}`

---

#### `POST /v1/providers/update-token`
Push a delegated credential token. No auth required (host-local communication).

Tokens can be scoped per-session using `session_id` (preferred), per-source using `source_ip`, or global (no scope key). The proxy resolves tokens in order: session_id > source_ip > global.

**Request:**
```json
{
  "provider": "github",
  "session_id": "sess-123",
  "token": "ghs_installation_token_...",
  "expires_at": "2026-04-02T10:00:00Z",
  "identity": {
    "user_email": "alice@company.com",
    "headers": {"X-Request-ID": "req-123"}
  }
}
```

| Field | Required | Description |
|---|---|---|
| `provider` | Yes | Provider name (must be a `delegated` type) |
| `token` | Yes | Credential token to inject |
| `session_id` | No | Scope token to a specific session (from attestation token claims) |
| `source_ip` | No | Deprecated: scope by source IP. Prefer `session_id` |
| `expires_at` | No | Token expiry (tokens rejected after this time) |
| `identity` | No | User identity to inject as headers |

**Response:** `{"status": "updated", "provider": "github"}`

---

#### `GET /v1/phantom-env`
Get phantom environment variables for CLI tools. No auth required.

**Query params:**
- `families` (optional, comma-separated: `gcp_cli_read,github_rest`)
- `session_id` (optional) — scope to the session's allowed families. If provided, only families in the session's policy are included.

**Response (200):**
```json
{
  "CLOUDSDK_AUTH_ACCESS_TOKEN": "phantom",
  "CLOUDSDK_CORE_PROJECT": "phantom"
}
```

---

#### `POST /v1/sessions/{id}/policy`
Set a session's allowed families and credentials. Called by the control plane after runner allocation.

**Request:**
```json
{
  "families": {
    "github_rest": {"token": "ghs_xxxxxxxxxxxx"},
    "gcp_cli_read": {"credential_ref": "sm:my-project/gcp-sa-key"},
    "slack_api": {}
  }
}
```

Per-family value shapes:

| Shape | Meaning |
|---|---|
| `{"token": "..."}` | Delegated — use this token for credential injection |
| `{"credential_ref": "sm:..."}` | Managed — access plane resolves the secret ref |
| `{}` | Auto-minting — access plane provider handles token generation |

**Response (200):** `{"status": "ok", "session_id": "sess-1"}`

**Error codes:** 400 (unknown family name), 404 (session not found)

---

#### `GET /v1/sessions/{id}/policy`
Get a session's current policy.

**Response (200):**
```json
{
  "session_id": "sess-1",
  "families": ["github_rest", "gcp_cli_read", "slack_api"]
}
```

**Error codes:** 404 (no policy set)

---

#### `DELETE /v1/sessions/{id}/policy`
Revoke a session's policy. The session loses access to all families.

**Response (200):** `{"status": "revoked", "session_id": "sess-1"}`

---

#### `GET /v1/families`
List all available families (YAML base + API-created dynamic).

**Response (200):**
```json
{
  "families": [
    {"name": "github_rest", "source": "yaml"},
    {"name": "custom_api", "source": "api"}
  ]
}
```

---

#### `GET /v1/families/{name}`
Get a single family definition (full manifest JSON).

---

#### `POST /v1/families`
Create or update a dynamic family. Cannot override YAML-defined families.

**Request:**
```json
{
  "family": "custom_api",
  "destinations": [{"host": "api.custom.com", "port": 443}],
  "provider": {"type": "delegated", "name": "custom"}
}
```

**Response (201):** `{"family": "custom_api", "status": "created"}`

**Error codes:** 400 (validation error), 409 (conflicts with YAML base family)

---

#### `DELETE /v1/families/{name}`
Remove an API-created family. Returns 409 if the family is YAML-defined.

**Response (204):** No content

---

#### `GET /v1/credentials/gcs`
Get a short-lived GCS access token scoped to the tenant's project.

**Response (200):**
```json
{
  "access_token": "ya29.c.abc...",
  "token_type": "Bearer",
  "expires_in": 3600
}
```

---

#### `GET /v1/ca.pem`
Get the CONNECT proxy's CA certificate in PEM format. VMs fetch this at boot to install in their trust store for SSL bump.

No auth required. Returns `503` if the CONNECT proxy is not enabled.

**Response (200):** PEM-encoded certificate (`Content-Type: application/x-pem-file`)

---

#### CONNECT Proxy (`:3128`)
Transparent HTTPS proxy with selective SSL bump for credential injection.

- Set `HTTPS_PROXY=http://bearer:ATTESTATION_TOKEN@<access-plane>:3128` in the VM
- The thaw agent embeds the attestation token in the proxy URL for session identification
- Proxy extracts `session_id` from the `Proxy-Authorization` header (Basic auth decoded from URL credentials)
- Validates target host against manifest destinations
- SSRF protection (blocks private/loopback IPs)
- SSL bump (MITM) for hosts with credential providers — injects Authorization header
- Raw tunnel for allowed hosts without providers
- Rejects unknown hosts with 403
- Method/path validation against manifest constraints (e.g. `GET /repos/**` allowed, `DELETE /**` denied)

---

## Control Plane

Orchestrator that schedules runners across hosts. Single instance per deployment.

**Base:** `http://<control-plane>:8080`

### Authentication

Bearer token required on all endpoints except `/health` and `/metrics`:
- API token: general management operations
- Host bootstrap token: host heartbeat

---

#### `GET /health`
**Response:** `OK` (plain text)

#### `GET /metrics`
Prometheus/OTel metrics endpoint.

---

#### `POST /api/v1/runners/allocate`
Allocate a runner. Handles scheduling, session resume, and host selection.

**Request:**
```json
{
  "request_id": "req-abc",
  "workload_key": "wk-123",
  "labels": {"env": "prod"},
  "session_id": "sess-1",
  "network_policy_preset": "agent-sandbox",
  "network_policy_json": "",
  "family_tokens": {
    "github_rest": "ghs_xxxxxxxxxxxx"
  }
}
```

| Field | Required | Description |
|---|---|---|
| `request_id` | Yes | Idempotency key (deduplicates within 5 min) |
| `workload_key` | Yes | Workload to allocate |
| `session_id` | No | Existing session ID for resume |
| `network_policy_preset` | No | Named network policy preset |
| `network_policy_json` | No | Full network policy JSON override |
| `family_tokens` | No | Per-family delegated tokens (e.g. `{"github_rest": "ghs_..."}`) |

When `family_tokens` is provided, the control plane merges them with the workload's configured families and pushes the session policy to the access plane (`POST /v1/sessions/{session_id}/policy`). Families with `credential_ref` in the config are forwarded as-is for the access plane to resolve.

**Response (200):**
```json
{
  "runner_id": "runner-xyz",
  "host_id": "host-1",
  "host_address": "10.0.0.5:50051",
  "internal_ip": "172.16.0.2",
  "session_id": "sess-1",
  "resumed": true
}
```

---

#### `GET /api/v1/runners/status?runner_id=<id>`
Get runner readiness status.

**Response codes:**
- `200` — runner ready (includes `host_address`)
- `202` — runner pending (still booting)
- `404` — runner not found
- `503` — runner unavailable

---

#### `POST /api/v1/runners/release`
**Request:** `{"runner_id": "runner-xyz"}`
**Response:** `{"success": true}`

#### `POST /api/v1/runners/pause`
Snapshot session and pause VM.

**Request:** `{"runner_id": "runner-xyz"}`
**Response:** `{"success": true}`

#### `POST /api/v1/runners/quarantine`
**Request:** `{"runner_id": "runner-xyz", "reason": "suspicious activity"}`
**Response:** `{"success": true, "quarantine_dir": "/var/quarantine/runner-xyz"}`

#### `POST /api/v1/runners/unquarantine`
**Request:** `{"runner_id": "runner-xyz"}`
**Response:** `{"success": true}`

---

#### `GET /api/v1/runners`
List all runners across all hosts.

**Response:** `{"runners": [...], "count": 42}`

#### `GET /api/v1/hosts`
List all registered hosts with resource utilization.

**Response:** Array of host objects with `host_id`, `instance_name`, `state`, `total_slots`, `used_slots`, CPU/memory usage.

---

#### `GET /api/v1/layered-configs/`
Query layered configuration registry.

#### `POST /api/v1/layered-configs`
Register or update a layered snapshot configuration.

**Request includes `project_id`** to link the workload to a project's access plane:
```json
{
  "display_name": "my-sandbox",
  "project_id": "my-project",
  "base_image": "ubuntu:22.04",
  "layers": [],
  "config": {
    "tier": "xs",
    "auto_rollout": true,
    "rootfs_size_gb": 2,
    "families": {
      "github_rest": {},
      "gcp_cli_read": {"credential_ref": "sm:my-project/gcp-sa-key"},
      "slack_api": {"credential_ref": "sm:my-project/slack-token"}
    }
  }
}
```

`config.families` declares which API families the workload uses:

| Family value | Meaning |
|---|---|
| `{}` | Delegated (token from caller at allocation) or auto-minting (access plane provider handles it) |
| `{"credential_ref": "sm:..."}` | Managed — access plane resolves the secret ref at runtime |

**Response (201):**
```json
{
  "config_id": "my-sandbox",
  "leaf_workload_key": "bf583f0c952897a5",
  "layers": [{"name": "_platform", "hash": "...", "status": "active", "depth": 0}]
}
```

#### `GET /api/v1/versions/desired`
Get desired snapshot versions for fleet rollout.

#### `GET /api/v1/versions/fleet`
Get fleet version convergence status.

---

#### `GET /api/v1/access-planes`
List all registered project access planes.

**Response:**
```json
{
  "access_planes": [
    {
      "project_id": "my-project",
      "access_plane_addr": "http://10.0.16.7:8080",
      "proxy_addr": "10.0.16.7:3128",
      "attestation_secret_ref": "secret-uuid-...",
      "ca_cert_pem": "",
      "tenant_id": "my-tenant",
      "created_at": "2026-04-02T06:14:06Z",
      "updated_at": "2026-04-02T06:14:06Z"
    }
  ],
  "count": 1
}
```

#### `POST /api/v1/access-planes`
Register or update a project's access plane configuration. Used by the scheduler to resolve access plane info when allocating runners.

**Request:**
```json
{
  "project_id": "my-project",
  "access_plane_addr": "http://10.0.16.7:8080",
  "proxy_addr": "10.0.16.7:3128",
  "attestation_secret_ref": "secret-uuid-here",
  "ca_cert_pem": "",
  "tenant_id": "my-tenant"
}
```

**Response (201):** `{"project_id": "my-project", "status": "ok"}`

#### `GET /api/v1/access-planes/{project_id}`
Get a specific project's access plane configuration.

#### `DELETE /api/v1/access-planes/{project_id}`
Remove a project's access plane configuration.

---

## Host Agent (capsule-manager)

Per-host VM lifecycle manager. Runs on each compute node.

### gRPC Service: HostAgent (`:50051`)

| RPC | Purpose |
|---|---|
| `AllocateRunner` | Create/resume a runner on this host |
| `ReleaseRunner` | Release runner (destroy or recycle) |
| `PauseRunner` | Snapshot session and pause VM |
| `ResumeRunner` | Restore VM from session snapshot |
| `GetHostStatus` | Host resource utilization |
| `ListRunners` | List runners on this host |
| `GetRunner` | Get runner details |
| `QuarantineRunner` | Isolate runner for debugging |
| `UnquarantineRunner` | Remove isolation |
| `UpdateNetworkPolicy` | Dynamically update iptables rules |
| `GetNetworkPolicy` | Get current network policy |
| `Heartbeat` | Periodic health signal |

### HTTP Endpoints (`:8080`)

#### `GET /health`
**Response:** `{"status": "ok"}`

#### `GET /ready`
**Response:** `{"ready": true}` (false when draining)

#### `POST /api/v1/runners/quarantine`
**Request:** `{"runner_id": "...", "reason": "...", "block_egress": true, "pause_vm": false}`

#### `POST /api/v1/runners/network-policy`
**Request:** `{"runner_id": "...", "network_policy_json": "{...}"}`

#### `POST /api/v1/gc`
Garbage collect unused runners. `{"dry_run": true}` for preview.

#### `POST /api/v1/runners/{runner_id}/auth/update-token`
**Status:** `410 Gone` — token updates should be sent directly to the access plane.

---

## Thaw Agent

Runs inside each Firecracker microVM. Handles boot, warmup, and command execution.

### Early Health Server (`:10501`)
Started immediately at boot, before MMDS is available.

| Endpoint | Purpose |
|---|---|
| `GET /alive` | Liveness check (plain text) |
| `GET /progress` | Boot progress: `{"step": "configuring-network"}` |
| `GET /logs` | Real-time boot log stream (text/plain) |
| `POST /exec` | Execute command: `{"command": [...], "env": {...}, "timeout_seconds": 30}` |
| `POST /pty` | Interactive PTY session (WebSocket) |
| `GET /service-logs` | Systemd service logs |
| `GET /file/{path}` | Read file from VM filesystem |

### Main Health Server (`:10500`)
Started after MMDS configuration completes.

| Endpoint | Purpose |
|---|---|
| `GET /health` | Health with metadata: `{"status": "healthy", "runner_id": "...", "uptime": "..."}` |
| `GET /warmup-status` | Snapshot build progress (phase, complete, duration) |
| `GET /warmup-logs` | Streaming warmup logs with sequence numbers |
| `GET /mmds-diag` | MMDS connectivity diagnostic |
| `GET /network` | Network config: configured IP, gateway, routes |
| `GET /connectivity` | Internet/DNS reachability test |
| `GET /debug` | Filesystem debug info: mounts, lsblk, df |

---

## Internal Labels (gRPC)

The control plane passes configuration to the host agent via gRPC request labels:

| Label | Value | Purpose |
|---|---|---|
| `_access_plane_config_json` | JSON `accessplane.Config` | Access plane connection info |
| `_attestation_token` | HMAC signed token | Runner's access plane auth token |
| `_network_policy_preset` | Preset name | Network policy preset |
| `_network_policy_json` | Full JSON policy | Network policy override |
| `_migrate_from_workload_key` | Workload key | Base image migration source |
| `_migrate_from_runner_id` | Runner ID | Base image migration source |

---

## Summary

| Service | Protocol | Default Port | Endpoints | Auth |
|---|---|---|---|---|
| Access Plane | HTTP + CONNECT | 8080, 3128 | ~20 + proxy | HMAC bearer token (session-scoped) |
| Control Plane | HTTP | 8080 | ~18 | API bearer token |
| Host Agent | gRPC + HTTP | 50051, 8080 | 12 gRPC + 8 HTTP | Network-scoped |
| Thaw Agent | HTTP | 10500, 10501 | ~13 | None (VM-internal) |
