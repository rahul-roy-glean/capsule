from __future__ import annotations

from typing import Any, cast

from pydantic import Field, field_validator

from capsule_sdk._snapshot_commands import normalize_snapshot_commands
from capsule_sdk.models.common import CapsuleModel


def _empty_configs() -> list[StoredLayeredConfig]:
    return []


class DriveSpec(CapsuleModel):
    """Drive specification for a layer."""

    drive_id: str
    label: str | None = None
    size_gb: int | None = None
    mount_path: str | None = None


class LayerDef(CapsuleModel):
    """Definition of a single layer in a layered config."""

    name: str
    init_commands: list[dict[str, Any]]
    refresh_commands: list[dict[str, Any]] | None = None
    drives: list[DriveSpec] | None = None
    refresh_interval: str | None = None

    @field_validator("init_commands", "refresh_commands", mode="before")
    @classmethod
    def _normalize_commands(cls, value: Any) -> Any:
        if value is None:
            return None
        if not isinstance(value, list):
            return value
        commands = cast(list[str | dict[str, Any]], value)
        return normalize_snapshot_commands(commands)


class LayeredConfigConfig(CapsuleModel):
    """Runtime configuration nested inside a LayeredConfig."""

    auto_pause: bool | None = None
    ttl: int | None = None
    tier: str | None = None
    auto_rollout: bool | None = None
    session_max_age_seconds: int | None = None
    rootfs_size_gb: int | None = None
    runner_user: str | None = None
    workspace_size_gb: int | None = None
    network_policy_preset: str | None = None
    network_policy: Any | None = None
    auth: Any | None = None


class StoredLayeredConfig(CapsuleModel):
    """Server-side stored representation of a layered config."""

    config_id: str
    display_name: str | None = None
    leaf_layer_hash: str | None = None
    leaf_workload_key: str | None = None
    tier: str | None = None
    start_command: dict[str, Any] | None = None
    runner_ttl_seconds: int | None = None
    session_max_age_seconds: int | None = None
    auto_pause: bool | None = None
    auto_rollout: bool | None = None
    max_concurrent_runners: int | None = None
    build_schedule: str | None = None
    network_policy_preset: str | None = None
    network_policy: Any | None = None
    created_at: str | None = None
    updated_at: str | None = None


class LayerStatus(CapsuleModel):
    """Status of a single layer in a layered config."""

    name: str
    layer_hash: str | None = None
    status: str | None = None
    current_version: str | None = None
    depth: int | None = None
    build_status: str | None = None
    build_version: str | None = None


class LayeredConfigDetail(CapsuleModel):
    """Detailed view of a layered config (GET /layered-configs/{id})."""

    config: StoredLayeredConfig
    layers: list[LayerStatus] | None = None
    definition: Any | None = None


class CreateConfigResponse(CapsuleModel):
    """Response from creating a layered config."""

    config_id: str
    leaf_workload_key: str | None = None
    layers: list[dict[str, Any]] | None = None


class BuildResponse(CapsuleModel):
    """Response from triggering a build."""

    config_id: str
    status: str | None = None
    force: bool | None = None
    clean: bool | None = None
    enqueued: int | None = None


class RefreshResponse(CapsuleModel):
    """Response from triggering a layer refresh."""

    config_id: str
    layer_name: str | None = None
    status: str | None = None


class LayeredConfigListResponse(CapsuleModel):
    """Response from listing layered configs."""

    configs: list[StoredLayeredConfig] = Field(default_factory=_empty_configs)
    count: int | None = None
