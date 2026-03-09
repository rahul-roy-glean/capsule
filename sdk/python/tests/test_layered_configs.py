from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.layered_config import (
    BuildResponse,
    CreateConfigResponse,
    LayeredConfigDetail,
    RefreshResponse,
    StoredLayeredConfig,
)
from bf_sdk.resources.layered_configs import LayeredConfigs


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    return HttpClient(config)


@pytest.fixture
def lc(http_client: HttpClient) -> LayeredConfigs:
    return LayeredConfigs(http_client)


class TestLayeredConfigs:
    def test_create(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        resp_data = {
            "config_id": "abc123",
            "leaf_workload_key": "wk-leaf",
            "layers": [{"name": "main", "hash": "h1", "depth": 0, "status": "pending"}],
        }
        mock_resp = httpx.Response(201, json=resp_data)
        body = {
            "display_name": "My Config",
            "layers": [{"name": "main", "init_commands": [{"type": "shell", "args": ["bash", "-lc", "echo hi"]}]}],
        }
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = lc.create(body)
        assert isinstance(result, CreateConfigResponse)
        assert result.config_id == "abc123"
        assert result.leaf_workload_key == "wk-leaf"

    def test_list(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        resp_data = {
            "configs": [
                {"config_id": "c1", "display_name": "First"},
                {"config_id": "c2", "display_name": "Second"},
            ],
            "count": 2,
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = lc.list()
        assert len(result) == 2
        assert all(isinstance(c, StoredLayeredConfig) for c in result)

    def test_get(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        resp_data = {
            "config": {"config_id": "c1", "display_name": "My Config"},
            "layers": [{"name": "main", "layer_hash": "h1", "status": "ready", "depth": 0}],
            "definition": {"display_name": "My Config", "layers": []},
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = lc.get("c1")
        assert isinstance(result, LayeredConfigDetail)
        assert result.config.config_id == "c1"
        assert result.layers is not None
        assert len(result.layers) == 1

    def test_delete(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(204, text="")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            lc.delete("c1")  # should not raise

    def test_build(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        resp_data = {"config_id": "c1", "status": "build_enqueued", "force": "false"}
        mock_resp = httpx.Response(202, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = lc.build("c1")
        assert isinstance(result, BuildResponse)
        assert result.status == "build_enqueued"

    def test_build_clean(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        resp_data = {"config_id": "c1", "status": "build_enqueued", "clean": "true"}
        mock_resp = httpx.Response(202, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = lc.build("c1", clean=True)
        assert isinstance(result, BuildResponse)
        assert result.clean == "true"

    def test_refresh_layer(self, lc: LayeredConfigs, http_client: HttpClient) -> None:
        resp_data = {"config_id": "c1", "layer_name": "deps", "status": "refresh_enqueued"}
        mock_resp = httpx.Response(202, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = lc.refresh_layer("c1", "deps")
        assert isinstance(result, RefreshResponse)
        assert result.layer_name == "deps"
        assert result.status == "refresh_enqueued"
