from __future__ import annotations

from typing import Any

from bf_sdk._http import HttpClient
from bf_sdk.models.layered_config import (
    BuildResponse,
    CreateConfigResponse,
    LayeredConfigDetail,
    RefreshResponse,
    StoredLayeredConfig,
)


class LayeredConfigs:
    """Layered configuration management."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def create(self, body: dict[str, Any]) -> CreateConfigResponse:
        data = self._http.post("/api/v1/layered-configs", json_body=body)
        return CreateConfigResponse.model_validate(data)

    def list(self) -> list[StoredLayeredConfig]:
        data = self._http.get("/api/v1/layered-configs")
        return [StoredLayeredConfig.model_validate(c) for c in data.get("configs", [])]

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
