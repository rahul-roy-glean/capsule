from __future__ import annotations

from collections.abc import Mapping
from pathlib import Path
from typing import TYPE_CHECKING, Any, cast

import yaml

from capsule_sdk._errors import CapsuleNotFound
from capsule_sdk.models.layered_config import (
    BuildResponse,
    CreateConfigResponse,
    LayeredConfigDetail,
    StoredLayeredConfig,
)
from capsule_sdk.models.workload import ResolvedWorkloadRef, WorkloadSummary

if TYPE_CHECKING:
    from os import PathLike

    from capsule_sdk.models.runner import AllocateRunnerResponse
    from capsule_sdk.resources.layered_configs import LayeredConfigs
    from capsule_sdk.resources.runners import Runners
    from capsule_sdk.runner_config import RunnerConfig
    from capsule_sdk.runner_session import RunnerSession

class Workloads:
    """High-level workload onboarding and runtime API."""

    def __init__(self, layered_configs: LayeredConfigs, runners: Runners) -> None:
        self._layered_configs = layered_configs
        self._runners = runners

    def onboard(
        self,
        spec: RunnerConfig | dict[str, Any] | str | PathLike[str],
        *,
        name: str | None = None,
        build: bool = True,
        force: bool = False,
        clean: bool = False,
    ) -> WorkloadSummary:
        """Register a workload from a RunnerConfig, dict, YAML string, or YAML file."""
        body = self._normalize_spec(spec, name=name)
        created = self._layered_configs.create(body)
        if build:
            self._layered_configs.build(created.config_id, force=force, clean=clean)
        return WorkloadSummary(
            display_name=body["display_name"],
            config_id=created.config_id,
            workload_key=created.leaf_workload_key,
        )

    def onboard_yaml(
        self,
        yaml_spec: str | PathLike[str],
        *,
        name: str | None = None,
        build: bool = True,
        force: bool = False,
        clean: bool = False,
    ) -> WorkloadSummary:
        """Register a workload from YAML text or a YAML file path."""
        return self.onboard(yaml_spec, name=name, build=build, force=force, clean=clean)

    def list(self) -> list[WorkloadSummary]:
        return [self._to_summary(cfg) for cfg in self._layered_configs.list()]

    def _resolve_ref(
        self,
        workload: (
            str
            | WorkloadSummary
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
        ),
    ) -> ResolvedWorkloadRef:
        if isinstance(workload, WorkloadSummary):
            return ResolvedWorkloadRef(
                display_name=workload.display_name,
                config_id=workload.config_id,
                workload_key=workload.workload_key,
            )
        if isinstance(workload, LayeredConfigDetail):
            return ResolvedWorkloadRef(
                display_name=workload.config.display_name,
                config_id=workload.config.config_id,
                workload_key=workload.config.leaf_workload_key,
            )
        if isinstance(workload, StoredLayeredConfig):
            return ResolvedWorkloadRef(
                display_name=workload.display_name,
                config_id=workload.config_id,
                workload_key=workload.leaf_workload_key,
            )
        if isinstance(workload, CreateConfigResponse):
            return ResolvedWorkloadRef(
                config_id=workload.config_id,
                workload_key=workload.leaf_workload_key,
            )
        return self._layered_configs.resolve_workload_ref(workload)

    def get(
        self,
        workload: (
            str
            | WorkloadSummary
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
        ),
    ) -> WorkloadSummary:
        if isinstance(workload, WorkloadSummary):
            return workload
        resolved = self._resolve_ref(workload)
        return self._summary_from_resolved(resolved)

    def build(
        self,
        workload: (
            str
            | WorkloadSummary
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
        ),
        *,
        force: bool = False,
        clean: bool = False,
    ) -> BuildResponse:
        summary = self.get(workload)
        if not summary.config_id:
            raise CapsuleNotFound(f"Workload {summary.display_name!r} does not have a config_id.")
        return self._layered_configs.build(summary.config_id, force=force, clean=clean)

    def delete(
        self,
        workload: (
            str
            | WorkloadSummary
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
        ),
    ) -> None:
        summary = self.get(workload)
        if not summary.config_id:
            raise CapsuleNotFound(f"Workload {summary.display_name!r} does not have a config_id.")
        self._layered_configs.delete(summary.config_id)

    def start(
        self,
        workload: (
            str
            | WorkloadSummary
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
        ),
        **kwargs: Any,
    ) -> RunnerSession:
        resolved = self._resolve_ref(workload)
        return self._runners.from_config(resolved, **kwargs)

    def allocate(
        self,
        workload: (
            str
            | WorkloadSummary
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
        ),
        **kwargs: Any,
    ) -> AllocateRunnerResponse:
        resolved = self._resolve_ref(workload)
        return self._runners.allocate(resolved, **kwargs)

    @staticmethod
    def _to_summary(cfg: StoredLayeredConfig) -> WorkloadSummary:
        return WorkloadSummary(
            display_name=cfg.display_name or cfg.leaf_workload_key or cfg.config_id,
            config_id=cfg.config_id,
            workload_key=cfg.leaf_workload_key,
        )

    @staticmethod
    def _summary_from_resolved(ref: ResolvedWorkloadRef) -> WorkloadSummary:
        display_name = ref.display_name or ref.workload_key or ref.config_id
        if not display_name:
            raise CapsuleNotFound("Resolved workload reference is missing a display name and identifiers.")
        return WorkloadSummary(
            display_name=display_name,
            config_id=ref.config_id,
            workload_key=ref.workload_key,
        )

    def _normalize_spec(
        self,
        spec: RunnerConfig | dict[str, Any] | str | PathLike[str],
        *,
        name: str | None = None,
    ) -> dict[str, Any]:
        if hasattr(spec, "to_create_body"):
            body = cast(dict[str, Any], spec.to_create_body())  # type: ignore[no-any-return]
            return self._ensure_display_name(body, provided_name=name)

        if isinstance(spec, Mapping):
            return self._normalize_mapping(spec, provided_name=name)

        raw_spec = str(spec)
        if "\n" in raw_spec:
            yaml_path = None
            source_name = None
            raw_text = raw_spec
        else:
            yaml_path = Path(raw_spec)
            try:
                exists = yaml_path.exists()
            except OSError:
                exists = False
            source_name = yaml_path.stem if exists else None
            raw_text = yaml_path.read_text() if exists else raw_spec
        loaded = yaml.safe_load(raw_text)
        if not isinstance(loaded, Mapping):
            raise ValueError("Workload YAML must parse to a mapping/object.")
        return self._normalize_mapping(
            cast(Mapping[str, Any], loaded),
            provided_name=name,
            source_name=source_name,
        )

    def _normalize_mapping(
        self,
        spec: Mapping[str, Any],
        *,
        provided_name: str | None = None,
        source_name: str | None = None,
    ) -> dict[str, Any]:
        raw = dict(spec)
        workload_spec = raw.get("workload")
        body: dict[str, Any] = (
            dict(cast(Mapping[str, Any], workload_spec))
            if isinstance(workload_spec, Mapping)
            else raw
        )
        return self._ensure_display_name(body, provided_name=provided_name, source_name=source_name)

    @staticmethod
    def _ensure_display_name(
        body: Mapping[str, Any],
        *,
        provided_name: str | None = None,
        source_name: str | None = None,
    ) -> dict[str, Any]:
        normalized = dict(body)
        display_name = (
            normalized.get("display_name")
            or normalized.get("name")
            or provided_name
            or source_name
        )
        if not isinstance(display_name, str) or not display_name:
            raise ValueError(
                "Workload specs must provide a display name via `display_name`, `name`, or the `name=` argument."
            )
        normalized["display_name"] = display_name
        normalized.pop("name", None)
        return normalized
