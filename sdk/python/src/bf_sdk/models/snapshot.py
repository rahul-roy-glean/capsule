from __future__ import annotations

from typing import Any

from bf_sdk.models.common import BFModel


class SnapshotConfig(BFModel):
    """A snapshot configuration keyed by workload_key."""

    workload_key: str
    display_name: str | None = None
    commands: list[dict[str, Any]] | None = None
    incremental_commands: list[dict[str, Any]] | None = None
    build_schedule: str | None = None
    max_concurrent_runners: int | None = None
    current_version: str | None = None
    auto_rollout: bool | None = None
    ci_system: str | None = None
    start_command: dict[str, Any] | None = None
    runner_ttl_seconds: int | None = None
    session_max_age_seconds: int | None = None
    auto_pause: bool | None = None
    tier: str | None = None
    network_policy: Any | None = None
    network_policy_preset: str | None = None
    created_at: str | None = None


class CreateSnapshotConfigRequest(BFModel):
    """Request to create or update a snapshot config."""

    display_name: str
    commands: list[dict[str, Any]]
    incremental_commands: list[dict[str, Any]] | None = None
    build_schedule: str | None = None
    max_concurrent_runners: int | None = None
    ci_system: str | None = None
    start_command: dict[str, Any] | None = None
    runner_ttl_seconds: int | None = None
    session_max_age_seconds: int | None = None
    auto_pause: bool | None = None
    tier: str | None = None
    network_policy_preset: str | None = None
    network_policy: Any | None = None


class Snapshot(BFModel):
    """A snapshot version."""

    version: str | None = None
    status: str | None = None
    gcs_path: str | None = None
    size_bytes: int | None = None
    created_at: str | None = None


class SnapshotTag(BFModel):
    """A named version tag for a snapshot config."""

    tag: str
    workload_key: str
    version: str
    description: str | None = None
    created_at: str | None = None


class BuildResult(BFModel):
    """Result of triggering a snapshot build."""

    workload_key: str
    version: str
    status: str
    incremental: str | None = None


class PromoteResult(BFModel):
    """Result of promoting a tag."""

    workload_key: str
    tag: str
    version: str
    status: str
