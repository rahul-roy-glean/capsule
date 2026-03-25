from __future__ import annotations

from pydantic import Field

from capsule_sdk.models.common import CapsuleModel


def _empty_hosts() -> list[Host]:
    return []


class Host(CapsuleModel):
    """A host machine running the capsule-manager agent."""

    id: str | None = None
    instance_name: str | None = None
    zone: str | None = None
    status: str | None = None
    idle_runners: int | None = None
    busy_runners: int | None = None
    snapshot_version: str | None = None
    last_heartbeat: str | None = None
    grpc_address: str | None = None
    total_cpu_millicores: int | None = None
    used_cpu_millicores: int | None = None
    total_memory_mb: int | None = None
    used_memory_mb: int | None = None


class HostListResponse(CapsuleModel):
    """Response from listing hosts."""

    hosts: list[Host] = Field(default_factory=_empty_hosts)
    count: int | None = None
