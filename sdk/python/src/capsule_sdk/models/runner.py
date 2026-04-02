from __future__ import annotations

from enum import Enum

from pydantic import ConfigDict, Field

from capsule_sdk.models.common import CapsuleModel


def _empty_runners() -> list[Runner]:
    return []


class RunnerState(str, Enum):
    """Runner lifecycle states."""

    idle = "idle"
    busy = "busy"
    booting = "booting"
    initializing = "initializing"
    paused = "paused"
    pausing = "pausing"
    suspended = "suspended"
    quarantined = "quarantined"
    draining = "draining"
    terminated = "terminated"
    # Status strings returned by the control plane
    ready = "ready"
    pending = "pending"
    unavailable = "unavailable"


class SessionDetail(CapsuleModel):
    """Session information for a runner."""

    session_id: str
    status: str = "active"
    paused_at: str | None = None
    layer_count: int = 0


class HostDetail(CapsuleModel):
    """Detailed host information."""

    zone: str
    snapshot_version: str | None = None
    last_heartbeat: str | None = None
    last_heartbeat_age_seconds: int | None = None
    is_healthy: bool = False


class ResourceInfo(CapsuleModel):
    """Resource utilization details."""

    cpu_reserved: int = 0
    cpu_used: int | None = None
    memory_reserved_mb: int = 0
    memory_used_mb: int | None = None
    tier: str | None = None


class ConfigInfo(CapsuleModel):
    """Runner configuration details."""

    runner_ttl_seconds: int = 0
    auto_pause: bool = False
    network_policy_preset: str | None = None


class Runner(CapsuleModel):
    """A runner instance."""

    runner_id: str | None = None
    host_id: str | None = None
    host_address: str | None = None
    host_name: str | None = None
    status: str | None = None
    workload_key: str | None = None
    internal_ip: str | None = None
    session_id: str | None = None
    resumed: bool | None = None
    # Enriched fields (available when detail=full)
    created_at: str | None = None
    age_seconds: int | None = None
    uptime_seconds: int | None = None
    idle_for_seconds: int | None = None
    sessions: list[SessionDetail] | None = None
    resources: ResourceInfo | None = None
    host: HostDetail | None = None
    config: ConfigInfo | None = None
    job_id: str | None = None


class AllocateRunnerRequest(CapsuleModel):
    """Request to allocate a runner."""

    workload_key: str
    request_id: str | None = None
    labels: dict[str, str] | None = None
    session_id: str | None = None
    network_policy_preset: str | None = None
    network_policy_json: str | None = None


class AllocateRunnerResponse(CapsuleModel):
    """Response from allocating a runner."""

    runner_id: str
    host_id: str | None = None
    host_address: str | None = None
    internal_ip: str | None = None
    session_id: str | None = None
    resumed: bool = False
    request_id: str | None = None


class RunnerStatus(CapsuleModel):
    """Runner status from the control plane."""

    runner_id: str
    status: str
    host_address: str | None = None
    error: str | None = None


class PauseResult(CapsuleModel):
    """Result of pausing a runner."""

    success: bool
    session_id: str | None = None
    snapshot_size_bytes: int | None = None
    layer: int | None = None


class PaginationInfo(CapsuleModel):
    """Pagination metadata."""

    next_cursor: str | None = None
    has_more: bool = False
    total_count: int | None = None


class RunnerListResponse(CapsuleModel):
    """Response from listing runners."""

    runners: list[Runner] = Field(default_factory=_empty_runners)
    count: int | None = None
    pagination: PaginationInfo | None = None


class ExecRequest(CapsuleModel):
    """Request to execute a command in a runner."""

    command: list[str]
    env: dict[str, str] | None = None
    working_dir: str | None = None
    timeout_seconds: int | None = None


class ExecEvent(CapsuleModel):
    """A single event from an ndjson exec stream."""

    type: str  # "stdout", "stderr", "exit", "error"
    data: str | None = None
    code: int | None = None
    message: str | None = None
    ts: str | None = None


class ExecResult(CapsuleModel):
    """Structured result from exec_collect."""

    model_config = ConfigDict(extra="ignore", frozen=False)

    stdout: str
    stderr: str
    exit_code: int
    duration_ms: float | None = None

    def __iter__(self):  # type: ignore[override]
        # Backwards compat: allows `output, code = r.exec_collect(...)`
        yield self.stdout + self.stderr
        yield self.exit_code
