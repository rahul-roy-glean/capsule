# Capsule Python SDK — Gap Analysis vs Control Plane & Runner APIs

Thorough comparison of the Python SDK (`sdk/python/src/capsule_sdk/`) against the control plane HTTP API (`cmd/capsule-control-plane/`) and the host-agent / runner APIs (`cmd/capsule-manager/`, `api/proto/runner.proto`, `pkg/runner/`).

---

## 1. Missing Control Plane Endpoints (no SDK coverage at all)

### 1.1 Snapshot Tag CRUD — `/{workload_key}/tags`

The control plane registers five tag-related routes under `/api/v1/layered-configs/`:

| Method | Route | Purpose |
|--------|-------|---------|
| GET | `/{wk}/tags` | List all tags for a workload key |
| POST | `/{wk}/tags` | Create/update a tag (body: `{tag, version, description}`) |
| GET | `/{wk}/tags/{tag}` | Get a specific tag |
| DELETE | `/{wk}/tags/{tag}` | Delete a tag |
| POST | `/{wk}/promote` | Promote a tagged version to active |

**SDK impact:** The SDK has zero coverage for snapshot tags. This means users cannot:
- List available snapshot versions for a workload
- Pin a workload to a specific version via tags
- Promote a tagged build to the fleet

**Files:** `cmd/capsule-control-plane/snapshot_tags.go`, `cmd/capsule-control-plane/layered_configs.go` (routing in `HandleLayeredConfigs`)

### 1.2 Host Management — `/api/v1/hosts`

| Method | Route | Purpose |
|--------|-------|---------|
| GET | `/api/v1/hosts` | List all hosts with resource usage, runner counts, status |

The response includes `id`, `instance_name`, `zone`, `status`, `idle_runners`, `busy_runners`, `snapshot_version`, `last_heartbeat`, `grpc_address`, `total_cpu_millicores`, `used_cpu_millicores`, `total_memory_mb`, `used_memory_mb`.

**SDK impact:** No `Host` model, no `client.hosts` resource. Users cannot inspect fleet capacity or health from the SDK.

### 1.3 Version / Rollout Observability

| Method | Route | Purpose |
|--------|-------|---------|
| GET | `/api/v1/versions/desired?instance_name=` | Get desired snapshot versions for a host |
| GET | `/api/v1/versions/fleet?workload_key=` | Get fleet convergence state for a workload |

**SDK impact:** Users have no visibility into whether a build has converged across the fleet, which is critical for CD pipelines.

### 1.4 Canary Reporting — `/api/v1/canary/report`

| Method | Route | Purpose |
|--------|-------|---------|
| POST | `/api/v1/canary/report` | Report E2E canary health check results |

**SDK impact:** E2E testing harnesses must use raw HTTP. Minor gap — this may intentionally be infrastructure-only.

---

## 2. Missing Runner / Host-Agent Endpoints (no SDK coverage)

### 2.1 Network Policy Management

The host agent exposes dedicated endpoints that the control plane does not proxy:

| Method | Route (on manager) | Purpose |
|--------|---------------------|---------|
| GET | `/api/v1/runners/network-policy?runner_id=` | Get current network policy |
| POST | `/api/v1/runners/network-policy` | Update network policy at runtime |

The gRPC proto also defines `UpdateNetworkPolicy` and `GetNetworkPolicy` RPCs.

**SDK impact:** Users cannot programmatically inspect or mutate a runner's network policy after allocation. The SDK only supports setting `network_policy_preset` and `network_policy_json` at allocation time.

### 2.2 Checkpoint (Non-Destructive Pause)

| Method | Route (on manager) | Purpose |
|--------|---------------------|---------|
| POST | `/api/v1/runners/{id}/checkpoint` | Checkpoint without destroying the runner |

**SDK vs CP:** The SDK exposes `pause()` (which calls `/api/v1/runners/pause` on the CP, which forwards to the host agent's `PauseRunner` gRPC). But the manager also has a `checkpoint` endpoint that creates a snapshot while keeping the runner running. This is completely absent from the SDK.

### 2.3 Service Logs Streaming

| Method | Route (on manager) | Purpose |
|--------|---------------------|---------|
| GET | `/api/v1/runners/{id}/service-logs` | Stream the runner's service (start_command) logs |

**SDK impact:** No way to tail service logs from the SDK. Users must manually construct the URL via host address.

### 2.4 GCP Token Fetch

| Method | Route (on manager) | Purpose |
|--------|---------------------|---------|
| GET | `/api/v1/runners/{id}/token/gcp` | Get a GCP access token for the runner |

**SDK impact:** Runners with GCP auth proxies cannot fetch tokens through the SDK.

### 2.5 Garbage Collection Trigger

| Method | Route (on manager) | Purpose |
|--------|---------------------|---------|
| POST | `/api/v1/gc` | Trigger garbage collection of orphaned resources |

**SDK impact:** Operational-only endpoint. Low priority for SDK users.

---

## 3. Model / Schema Drift

### 3.1 `Runner` model — missing `workload_key` and `host_name`

The control plane `GET /api/v1/runners` returns:
```json
{
  "runner_id": "...",
  "host_id": "...",
  "host_name": "...",
  "workload_key": "...",
  "status": "..."
}
```

The SDK `Runner` model (`models/runner.py`) has:
```python
class Runner(CapsuleModel):
    runner_id: str | None = None
    host_id: str | None = None
    host_address: str | None = None  # ← CP returns host_name, not host_address
    status: str | None = None
    internal_ip: str | None = None   # ← CP doesn't return this in list
    session_id: str | None = None    # ← CP doesn't return this in list
    resumed: bool | None = None      # ← CP doesn't return this in list
```

**Gaps:**
- `workload_key` is returned by CP but **not modeled** in the SDK `Runner`
- `host_name` is returned by CP but the SDK models `host_address` instead (field name mismatch — the list endpoint returns `host_name`, not `host_address`)
- `internal_ip`, `session_id`, `resumed` exist in the SDK model but are never populated by the list endpoint

### 3.2 `BuildResponse` model — missing `enqueued` field

Control plane build endpoint returns:
```json
{
  "config_id": "...",
  "status": "build_enqueued",
  "enqueued": 1,
  "force": false,
  "clean": false
}
```

SDK `BuildResponse`:
```python
class BuildResponse(CapsuleModel):
    config_id: str
    status: str | None = None
    force: str | None = None    # ← CP returns bool, SDK declares str
    clean: str | None = None    # ← CP returns bool, SDK declares str
```

**Gaps:**
- `enqueued` field missing entirely — users can't tell how many layers were queued
- `force` and `clean` typed as `str | None` in SDK but the CP returns `bool` values

### 3.3 `RefreshResponse` model — missing `enqueued` field

Control plane refresh endpoint returns:
```json
{
  "config_id": "...",
  "layer_name": "...",
  "status": "refresh_enqueued",
  "enqueued": 1
}
```

SDK `RefreshResponse` omits `enqueued`.

### 3.4 `StoredLayeredConfig` — list endpoint omits `network_policy_preset` and `network_policy`

The CP `ListLayeredConfigs` SQL query does not SELECT `network_policy_preset` or `network_policy`, so the list response never includes those fields even though the SDK model declares them. The GET (single config) endpoint does return them.

The SDK model has both fields, which will silently be `None` when populated from the list response. This is a **control plane bug**, not an SDK bug, but the SDK could defensively document this.

### 3.5 `AllocateRunnerResponse` — missing `request_id` from CP response

The CP `HandleAllocateRunner` handler response includes:
```json
{
  "runner_id": "...", "host_id": "...", "host_address": "...",
  "internal_ip": "...", "session_id": "...", "resumed": false
}
```

The SDK model declares `request_id: str | None = None` but the CP **does not include `request_id` in the response**. The SDK populates it from its own input, so this is benign but misleading — the field is never server-confirmed.

### 3.6 `RunnerState` enum vs proto `RunnerState`

| SDK `RunnerState` | Proto `RunnerState` | Notes |
|---|---|---|
| `idle` | `RUNNER_STATE_IDLE` | Match |
| `busy` | `RUNNER_STATE_BUSY` | Match |
| `booting` | `RUNNER_STATE_BOOTING` | Match |
| `initializing` | `RUNNER_STATE_INITIALIZING` | Match |
| `paused` | `RUNNER_STATE_PAUSED` | Match |
| `pausing` | `RUNNER_STATE_PAUSING` | Match |
| `suspended` | `RUNNER_STATE_SUSPENDED` | Match |
| `quarantined` | `RUNNER_STATE_QUARANTINED` | Match |
| `draining` | `RUNNER_STATE_DRAINING` | Match |
| `terminated` | `RUNNER_STATE_TERMINATED` | Match |
| `ready` | _(not in proto)_ | CP-synthesized, OK |
| `pending` | _(not in proto)_ | CP-synthesized, OK |
| `unavailable` | _(not in proto)_ | CP-synthesized, OK |
| _(missing)_ | `RUNNER_STATE_COLD` | **Not in SDK** |
| _(missing)_ | `RUNNER_STATE_RETIRING` | **Not in SDK** |

**Gap:** `cold` and `retiring` runner states exist in the proto but are not represented in the SDK's `RunnerState` enum.

---

## 4. Go cpapi Client vs Python SDK — Feature Parity

The Go `cpapi.Client` (`pkg/cpapi/client.go`) is minimal:

| cpapi Method | SDK Equivalent | Notes |
|---|---|---|
| `AllocateRunner` | `Runners.allocate` | Both present |
| `ReleaseRunner` | `Runners.release` | Both present |
| `PauseRunner` | `Runners.pause` | Both present |
| `WaitReady` | `Runners.wait_ready` | Both present |
| _(none)_ | `Runners.connect` | **Go client missing** |
| _(none)_ | `Runners.list` | **Go client missing** |
| _(none)_ | `Runners.quarantine` | **Go client missing** |
| _(none)_ | `Runners.status` | **Go client has WaitReady polling instead** |

The Python SDK is more complete than the Go client. This is expected — the Go client is for host-agent-to-CP communication, not end-user usage.

---

## 5. Proto RPC Coverage in SDK

The proto defines two gRPC services:

### `HostAgent` service RPCs:

| RPC | SDK Coverage | Notes |
|-----|-------------|-------|
| `AllocateRunner` | Via CP proxy | Not directly called, CP forwards |
| `ReleaseRunner` | Via CP proxy | Not directly called |
| `GetHostStatus` | **None** | No SDK method |
| `Heartbeat` | **None** | Infrastructure-only |
| `ListRunners` | **None** | No SDK method for host-level list |
| `GetRunner` | **None** | No SDK method for host-level get |
| `QuarantineRunner` | Via CP proxy | SDK calls CP, CP proxies to host |
| `UnquarantineRunner` | Via CP proxy | SDK calls CP, CP proxies to host |
| `PauseRunner` | Via CP proxy | SDK calls CP, CP forwards via gRPC |
| `ResumeRunner` | Via CP `/connect` | SDK calls CP, CP forwards via gRPC |
| `UpdateNetworkPolicy` | **None** | **Not in SDK** |
| `GetNetworkPolicy` | **None** | **Not in SDK** |

### `ControlPlane` service RPCs:

| RPC | SDK Coverage | Notes |
|-----|-------------|-------|
| `GetRunner` | **None** | Not used by SDK (HTTP used instead) |
| `ReleaseRunner` | **None** | Not used by SDK (HTTP used instead) |
| `RegisterHost` | **None** | Infrastructure-only |
| `HostHeartbeat` | **None** | Infrastructure-only |
| `GetSnapshot` | **None** | Only gRPC, HTTP list exists |

---

## 6. Specific Code-Level Issues

### 6.1 `DriveSpec` model missing `read_only` and `commands` fields

CP `snapshot.LayeredConfig` layer drive spec includes:
```json
{"drive_id": "", "label": "", "size_gb": 0, "read_only": false, "commands": [], "mount_path": ""}
```

SDK `DriveSpec`:
```python
class DriveSpec(CapsuleModel):
    drive_id: str
    label: str | None = None
    size_gb: int | None = None
    mount_path: str | None = None
```

**Missing:** `read_only: bool | None` and `commands: list[dict] | None`

### 6.2 `RunnerConfig` builder missing `max_concurrent_runners` and `build_schedule`

The CP `StoredLayeredConfig` has `max_concurrent_runners` and `build_schedule` fields (stored in DB), but the `RunnerConfig` fluent builder has no `with_max_concurrent_runners()` or `with_build_schedule()` methods, and `to_create_body()` cannot set these.

### 6.3 `LayeredConfigConfig` missing `max_concurrent_runners` and `build_schedule`

The SDK's `LayeredConfigConfig` model (used in create payloads) does not have `max_concurrent_runners` or `build_schedule` fields, even though the CP stores and returns them.

### 6.4 `Snapshot` model has PascalCase aliases but `SnapshotMetrics` doesn't

The `Snapshot` model handles both `snake_case` and `PascalCase` keys via `AliasChoices`. But `SnapshotMetrics` uses plain field names — if the CP returns `Metrics` with PascalCase sub-fields, they won't parse.

Currently the CP returns:
```json
{"Metrics": {"avg_analysis_time_ms": ..., "cache_hit_ratio": ..., "sample_count": ...}}
```

The nested object uses snake_case, so this works. But it's inconsistent with the parent pattern and fragile.

### 6.5 Snapshot `list()` doesn't expose `current_version`

The CP returns `{"snapshots": [...], "count": N, "current_version": "..."}` but the SDK's `Snapshots.list()` only returns the list:
```python
def list(self) -> list[Snapshot]:
    data = self._http.get("/api/v1/snapshots")
    return SnapshotListResponse.model_validate(data).snapshots
```

The `SnapshotListResponse.current_version` is parsed but discarded. Users have no way to know which snapshot version is currently active.

---

## 7. Summary: Priority-Ranked Gaps

### P0 — Breaks user workflows

| # | Gap | Impact |
|---|-----|--------|
| 1 | `Runner` model missing `workload_key` | Can't identify which workload a runner belongs to from `list()` |
| 2 | `BuildResponse` missing `enqueued` field | Can't tell if build was actually triggered |
| 3 | `BuildResponse.force`/`clean` typed as `str` instead of `bool` | Type mismatch with CP |

### P1 — Missing important features

| # | Gap | Impact |
|---|-----|--------|
| 4 | No snapshot tag CRUD (list/create/get/delete/promote) | Can't manage deployments or pin versions |
| 5 | No host listing | Can't inspect fleet capacity |
| 6 | No fleet convergence / version endpoints | Can't verify build rollout in CI |
| 7 | No network policy get/update at runtime | Can't adjust security posture post-allocation |
| 8 | No checkpoint (non-destructive pause) | Can't snapshot-in-place without destroying runner |
| 9 | No service logs streaming | Can't debug start_command failures |
| 10 | `Snapshots.list()` discards `current_version` | Can't determine active snapshot |

### P2 — Model drift / incompleteness

| # | Gap | Impact |
|---|-----|--------|
| 11 | `Runner` model: `host_address` vs CP's `host_name` field name mismatch | Silent None from list response |
| 12 | `DriveSpec` missing `read_only` and `commands` | Incomplete drive configuration |
| 13 | `RunnerConfig` missing `max_concurrent_runners` / `build_schedule` | Can't set concurrency limits or scheduled builds |
| 14 | `RunnerState` missing `cold` and `retiring` | Incomplete state machine |
| 15 | `RefreshResponse` missing `enqueued` | Minor info gap |
| 16 | CP list configs query omits `network_policy_preset`/`network_policy` | Fields always None from list |
| 17 | `AllocateRunnerResponse.request_id` not server-returned | Benign but misleading |
