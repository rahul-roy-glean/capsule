from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.snapshot import BuildResult, PromoteResult, SnapshotConfig, SnapshotTag
from bf_sdk.resources.snapshot_configs import SnapshotConfigs


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", api_key="test")
    return HttpClient(config)


@pytest.fixture
def sc(http_client: HttpClient) -> SnapshotConfigs:
    return SnapshotConfigs(http_client)


class TestSnapshotConfigs:
    def test_create(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {
            "workload_key": "wk-abc",
            "display_name": "My Config",
            "commands": [{"command": "echo hi"}],
        }
        mock_resp = httpx.Response(201, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.create(display_name="My Config", commands=[{"command": "echo hi"}])
        assert isinstance(result, SnapshotConfig)
        assert result.workload_key == "wk-abc"

    def test_get(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {"workload_key": "wk-abc", "display_name": "Config"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.get("wk-abc")
        assert result.workload_key == "wk-abc"

    def test_list(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {"configs": [{"workload_key": "wk1"}, {"workload_key": "wk2"}], "count": 2}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.list()
        assert len(result) == 2
        assert all(isinstance(c, SnapshotConfig) for c in result)

    def test_trigger_build(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {"workload_key": "wk1", "version": "v2", "status": "building"}
        mock_resp = httpx.Response(202, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.trigger_build("wk1")
        assert isinstance(result, BuildResult)
        assert result.status == "building"

    def test_create_tag(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {"tag": "stable", "workload_key": "wk1", "version": "v1", "description": "prod"}
        mock_resp = httpx.Response(201, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.create_tag("wk1", tag="stable", version="v1", description="prod")
        assert isinstance(result, SnapshotTag)
        assert result.tag == "stable"

    def test_list_tags(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {
            "tags": [
                {"tag": "stable", "workload_key": "wk1", "version": "v1"},
                {"tag": "canary", "workload_key": "wk1", "version": "v2"},
            ],
            "count": 2,
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.list_tags("wk1")
        assert len(result) == 2

    def test_get_tag(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {"tag": "stable", "workload_key": "wk1", "version": "v1"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.get_tag("wk1", "stable")
        assert result.version == "v1"

    def test_delete_tag(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(204, text="")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            sc.delete_tag("wk1", "canary")  # should not raise

    def test_promote(self, sc: SnapshotConfigs, http_client: HttpClient) -> None:
        resp_data = {"workload_key": "wk1", "tag": "stable", "version": "v1", "status": "promoted"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = sc.promote("wk1", tag="stable")
        assert isinstance(result, PromoteResult)
        assert result.status == "promoted"
