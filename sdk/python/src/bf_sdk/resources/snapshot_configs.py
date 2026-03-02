from __future__ import annotations

from typing import Any

from bf_sdk._http import HttpClient
from bf_sdk.models.snapshot import (
    BuildResult,
    PromoteResult,
    SnapshotConfig,
    SnapshotTag,
)


class SnapshotConfigs:
    """Snapshot configuration management."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def create(
        self,
        *,
        display_name: str,
        commands: list[dict[str, Any]],
        incremental_commands: list[dict[str, Any]] | None = None,
        build_schedule: str | None = None,
        max_concurrent_runners: int | None = None,
        ci_system: str | None = None,
        start_command: dict[str, Any] | None = None,
        runner_ttl_seconds: int | None = None,
        session_max_age_seconds: int | None = None,
        auto_pause: bool | None = None,
        tier: str | None = None,
        network_policy_preset: str | None = None,
        network_policy: Any | None = None,
    ) -> SnapshotConfig:
        body: dict[str, Any] = {
            "display_name": display_name,
            "commands": commands,
        }
        if incremental_commands is not None:
            body["incremental_commands"] = incremental_commands
        if build_schedule is not None:
            body["build_schedule"] = build_schedule
        if max_concurrent_runners is not None:
            body["max_concurrent_runners"] = max_concurrent_runners
        if ci_system is not None:
            body["ci_system"] = ci_system
        if start_command is not None:
            body["start_command"] = start_command
        if runner_ttl_seconds is not None:
            body["runner_ttl_seconds"] = runner_ttl_seconds
        if session_max_age_seconds is not None:
            body["session_max_age_seconds"] = session_max_age_seconds
        if auto_pause is not None:
            body["auto_pause"] = auto_pause
        if tier is not None:
            body["tier"] = tier
        if network_policy_preset is not None:
            body["network_policy_preset"] = network_policy_preset
        if network_policy is not None:
            body["network_policy"] = network_policy

        data = self._http.post("/api/v1/snapshot-configs", json_body=body)
        return SnapshotConfig.model_validate(data)

    def get(self, workload_key: str) -> SnapshotConfig:
        data = self._http.get(f"/api/v1/snapshot-configs/{workload_key}")
        return SnapshotConfig.model_validate(data)

    def list(self) -> list[SnapshotConfig]:
        data = self._http.get("/api/v1/snapshot-configs")
        return [SnapshotConfig.model_validate(c) for c in data.get("configs", [])]

    def trigger_build(self, workload_key: str, *, incremental: bool = False) -> BuildResult:
        url = f"/api/v1/snapshot-configs/{workload_key}/build"
        if incremental:
            url += "?incremental=true"
        data = self._http.post(url)
        return BuildResult.model_validate(data)

    def create_tag(
        self,
        workload_key: str,
        *,
        tag: str,
        version: str,
        description: str | None = None,
    ) -> SnapshotTag:
        body: dict[str, Any] = {"tag": tag, "version": version}
        if description is not None:
            body["description"] = description
        data = self._http.post(f"/api/v1/snapshot-configs/{workload_key}/tags", json_body=body)
        return SnapshotTag.model_validate(data)

    def list_tags(self, workload_key: str) -> list[SnapshotTag]:
        data = self._http.get(f"/api/v1/snapshot-configs/{workload_key}/tags")
        return [SnapshotTag.model_validate(t) for t in data.get("tags", [])]

    def get_tag(self, workload_key: str, tag: str) -> SnapshotTag:
        data = self._http.get(f"/api/v1/snapshot-configs/{workload_key}/tags/{tag}")
        return SnapshotTag.model_validate(data)

    def delete_tag(self, workload_key: str, tag: str) -> None:
        self._http.delete(f"/api/v1/snapshot-configs/{workload_key}/tags/{tag}")

    def promote(self, workload_key: str, *, tag: str) -> PromoteResult:
        data = self._http.post(
            f"/api/v1/snapshot-configs/{workload_key}/promote",
            json_body={"tag": tag},
        )
        return PromoteResult.model_validate(data)
