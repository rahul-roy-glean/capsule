from __future__ import annotations

from unittest.mock import patch
from urllib.parse import parse_qs, urlparse

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.runner import AllocateRunnerResponse, ConnectResult, PauseResult, RunnerStatus
from bf_sdk.resources.runners import Runners


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    return HttpClient(config)


@pytest.fixture
def runners(http_client: HttpClient) -> Runners:
    return Runners(http_client)


class TestRunners:
    def test_allocate(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {
            "runner_id": "r-123",
            "host_id": "h-1",
            "host_address": "10.0.0.1:8080",
            "session_id": "s-1",
            "resumed": False,
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.allocate("my-workload", labels={"env": "test"})
        assert isinstance(result, AllocateRunnerResponse)
        assert result.runner_id == "r-123"
        assert result.host_address == "10.0.0.1:8080"
        # Host should be cached
        assert runners._host_cache["r-123"] == "10.0.0.1:8080"

    def test_status_ready(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"runner_id": "r-1", "status": "ready", "host_address": "10.0.0.1:8080"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.status("r-1")
        assert isinstance(result, RunnerStatus)
        assert result.status == "ready"

    def test_status_pending(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"runner_id": "r-1", "status": "pending"}
        mock_resp = httpx.Response(202, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.status("r-1")
        assert result.status == "pending"

    def test_list(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"runners": [{"host_id": "h1", "status": "running"}], "count": 1}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.list()
        assert len(result) == 1

    def test_release(self, runners: Runners, http_client: HttpClient) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        mock_resp = httpx.Response(200, json={"success": True})
        with patch.object(http_client._client, "request", return_value=mock_resp):
            ok = runners.release("r-1")
        assert ok is True
        assert "r-1" not in runners._host_cache

    def test_pause(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"success": True, "session_id": "s-1", "snapshot_size_bytes": 1024, "layer": 2}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.pause("r-1")
        assert isinstance(result, PauseResult)
        assert result.success is True
        assert result.layer == 2

    def test_connect(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"status": "connected", "runner_id": "r-1", "host_address": "10.0.0.2:8080"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.connect("r-1")
        assert isinstance(result, ConnectResult)
        assert result.host_address == "10.0.0.2:8080"
        assert runners._host_cache["r-1"] == "10.0.0.2:8080"

    def test_resolve_host_uses_cache(self, runners: Runners) -> None:
        runners._host_cache["r-1"] = "cached-host:8080"
        host = runners._resolve_host("r-1")
        assert host == "http://cached-host:8080"

    def test_shell_returns_session(self, runners: Runners) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        session = runners.shell("r-1", cols=120, rows=40)
        assert session._url == "ws://10.0.0.1:8080/api/v1/runners/r-1/pty?cols=120&rows=40"

    def test_shell_with_command(self, runners: Runners) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        session = runners.shell("r-1", command="/bin/zsh -lc 'echo hi'")
        parsed = urlparse(session._url)
        assert parse_qs(parsed.query)["command"] == ["/bin/zsh -lc 'echo hi'"]

    def test_quarantine(self, runners: Runners, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"success": True, "quarantine_dir": "/tmp/q"})
        with patch.object(http_client._client, "request", return_value=mock_resp) as request:
            result = runners.quarantine("r-1", reason="test")
        assert result["success"] is True
        request_url = request.call_args.args[1]
        parsed = urlparse(request_url)
        assert parsed.path == "/api/v1/runners/quarantine"
        assert parse_qs(parsed.query)["reason"] == ["test"]

    def test_unquarantine(self, runners: Runners, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"success": True})
        with patch.object(http_client._client, "request", return_value=mock_resp) as request:
            result = runners.unquarantine("r-1")
        assert result["success"] is True
        request_url = request.call_args.args[1]
        parsed = urlparse(request_url)
        assert parsed.path == "/api/v1/runners/unquarantine"
        assert parse_qs(parsed.query)["runner_id"] == ["r-1"]
