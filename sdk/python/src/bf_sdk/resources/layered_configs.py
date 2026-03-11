from __future__ import annotations

from typing import TYPE_CHECKING, Any, cast

from bf_sdk._errors import BFConflict, BFNotFound
from bf_sdk._http import HttpClient
from bf_sdk.models.layered_config import (
    BuildResponse,
    CreateConfigResponse,
    LayeredConfigDetail,
    RefreshResponse,
    StoredLayeredConfig,
)

if TYPE_CHECKING:
    from bf_sdk.models.workload import WorkloadSummary
    from bf_sdk.runner_config import RunnerConfig


class LayeredConfigs:
    """Advanced low-level layered config management.

    This is kept as a compatibility escape hatch. Most users should prefer
    `BFClient.workloads`, which wraps these control-plane details in a more
    ergonomic workload-first API.
    """

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def create(self, body: dict[str, Any]) -> CreateConfigResponse:
        data = self._http.post("/api/v1/layered-configs", json_body=body)
        return CreateConfigResponse.model_validate(data)

    def list(self) -> list[StoredLayeredConfig]:
        data = self._http.get("/api/v1/layered-configs")
        raw_configs_value = data.get("configs")
        raw_configs: list[dict[str, Any]]
        if isinstance(raw_configs_value, list):
            raw_configs = []
            for item in cast(list[Any], raw_configs_value):
                if isinstance(item, dict):
                    raw_configs.append(cast(dict[str, Any], item))
        else:
            raw_configs = []
        return [StoredLayeredConfig.model_validate(c) for c in raw_configs]

    def get(self, config_id: str) -> LayeredConfigDetail:
        data = self._http.get(f"/api/v1/layered-configs/{config_id}")
        return LayeredConfigDetail.model_validate(data)

    def delete(self, config_id: str) -> None:
        self._http.delete(f"/api/v1/layered-configs/{config_id}")

    def build(self, config_id: str, *, force: bool = False, clean: bool = False) -> BuildResponse:
        url = f"/api/v1/layered-configs/{config_id}/build"
        params: list[str] = []
        if force:
            params.append("force=true")
        if clean:
            params.append("clean=true")
        if params:
            url += "?" + "&".join(params)
        data = self._http.post(url)
        return BuildResponse.model_validate(data)

    def refresh_layer(self, config_id: str, layer_name: str) -> RefreshResponse:
        data = self._http.post(
            f"/api/v1/layered-configs/{config_id}/layers/{layer_name}/refresh",
        )
        return RefreshResponse.model_validate(data)

    def resolve_workload_key(
        self,
        config_ref: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
        ),
    ) -> str:
        """Resolve a user-facing config reference into a control-plane workload key."""
        direct = self._extract_direct_workload_key(config_ref)
        if direct:
            return direct

        if isinstance(config_ref, LayeredConfigDetail):
            return self._resolve_from_stored_config(config_ref.config)

        if isinstance(config_ref, StoredLayeredConfig):
            return self._resolve_from_stored_config(config_ref)

        ref_value = self._extract_reference_value(config_ref)
        configs = self.list()

        workload_matches = [cfg for cfg in configs if cfg.leaf_workload_key == ref_value]
        if workload_matches:
            return workload_matches[0].leaf_workload_key  # type: ignore[return-value]

        config_id_matches = [cfg for cfg in configs if cfg.config_id == ref_value]
        if config_id_matches:
            return self._resolve_from_stored_config(config_id_matches[0])

        display_name_matches = [cfg for cfg in configs if cfg.display_name == ref_value]
        if len(display_name_matches) > 1:
            raise BFConflict(
                f"Multiple layered configs share the display name {ref_value!r}. "
                "Use a config_id or workload key to disambiguate."
            )
        if len(display_name_matches) == 1:
            return self._resolve_from_stored_config(display_name_matches[0])

        raise BFNotFound(
            f"Could not resolve workload {ref_value!r}. "
            "Pass a display name, config_id, create response, or workload key."
        )

    @staticmethod
    def _extract_reference_value(
        config_ref: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
        ),
    ) -> str:
        if isinstance(config_ref, str):
            return config_ref

        if hasattr(config_ref, "display_name"):
            value = cast(Any, config_ref).display_name
            if isinstance(value, str) and value:
                return value

        if hasattr(config_ref, "config_id"):
            value = cast(Any, config_ref).config_id
            if isinstance(value, str) and value:
                return value

        raise BFNotFound("Could not determine how to resolve the requested workload reference.")

    @staticmethod
    def _extract_direct_workload_key(
        config_ref: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
        ),
    ) -> str | None:
        if hasattr(config_ref, "workload_key"):
            value = cast(Any, config_ref).workload_key
            if isinstance(value, str) and value:
                return value
        if hasattr(config_ref, "leaf_workload_key"):
            value = cast(Any, config_ref).leaf_workload_key
            if isinstance(value, str) and value:
                return value
        return None

    def _resolve_from_stored_config(self, cfg: StoredLayeredConfig) -> str:
        if cfg.leaf_workload_key:
            return cfg.leaf_workload_key
        detail = self.get(cfg.config_id)
        if detail.config.leaf_workload_key:
            return detail.config.leaf_workload_key
        raise BFNotFound(f"Layered config {cfg.config_id!r} does not expose a workload key yet.")
