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


class Runner(CapsuleModel):
    """A runner instance."""

    runner_id: str | None = None
    host_id: str | None = None
    host_address: str | None = None
    status: str | None = None
    internal_ip: str | None = None
    session_id: str | None = None
    resumed: bool | None = None


class AllocateRunnerRequest(CapsuleModel):
    """Request to allocate a runner."""

    workload_key: str
    request_id: str | None = None
    labels: dict[str, str] | None = None
    session_id: str | None = None
    snapshot_tag: str | None = None
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
    attached: bool = False
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


class ConnectResult(CapsuleModel):
    """Result of connecting to a runner."""

    status: str
    runner_id: str
    host_address: str | None = None
    session_id: str | None = None


class ForkSessionResponse(CapsuleModel):
    """Result of forking a live session into a new runner/session."""

    runner_id: str
    host_id: str | None = None
    host_address: str | None = None
    internal_ip: str | None = None
    session_id: str
    resumed: bool = True
    attached: bool = False
    parent_session_id: str | None = None
    source_runner_id: str | None = None


class RunnerListResponse(CapsuleModel):
    """Response from listing runners."""

    runners: list[Runner] = Field(default_factory=_empty_runners)
    count: int | None = None


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
