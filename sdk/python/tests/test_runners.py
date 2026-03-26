from __future__ import annotations

from unittest.mock import patch
from urllib.parse import parse_qs, urlparse

import httpx
import pytest

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._errors import (
    CapsuleAllocationTimeoutError,
    CapsuleNotFound,
    CapsuleOperationTimeoutError,
    CapsuleRunnerUnavailableError,
)
from capsule_sdk._http import HttpClient
from capsule_sdk.models.layered_config import CreateConfigResponse
from capsule_sdk.models.runner import AllocateRunnerResponse, PauseResult, RunnerStatus
from capsule_sdk.models.workload import ResolvedWorkloadRef
from capsule_sdk.resources.layered_configs import LayeredConfigs
from capsule_sdk.resources.runners import Runners
from capsule_sdk.runner_session import RunnerSession


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    return HttpClient(config)


@pytest.fixture
def runners(http_client: HttpClient) -> Runners:
    return Runners(http_client)


@pytest.fixture
def layered_configs(http_client: HttpClient) -> LayeredConfigs:
    return LayeredConfigs(http_client)


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
        with patch.object(http_client._client, "request", return_value=mock_resp) as request:
            result = runners.allocate("my-workload", labels={"env": "test"}, request_id="req-1")
        assert isinstance(result, AllocateRunnerResponse)
        assert result.runner_id == "r-123"
        assert result.host_address == "10.0.0.1:8080"
        assert result.request_id == "req-1"
        # Host should be cached
        assert runners._host_cache["r-123"] == "10.0.0.1:8080"
        assert request.call_args.kwargs["json"]["request_id"] == "req-1"

    def test_allocate_resolves_display_name(self, http_client: HttpClient, layered_configs: LayeredConfigs) -> None:
        runners = Runners(http_client, layered_configs=layered_configs)
        resp_data = {"runner_id": "r-123", "host_address": "10.0.0.1:8080"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(
            layered_configs,
            "resolve_workload_ref",
            return_value=ResolvedWorkloadRef(display_name="My Sandbox", workload_key="wk-leaf"),
        ) as resolve:
            with patch.object(http_client._client, "request", return_value=mock_resp):
                result = runners.allocate("My Sandbox")
        assert result.runner_id == "r-123"
        resolve.assert_called_once_with("My Sandbox")

    def test_allocate_accepts_create_response(self, http_client: HttpClient, layered_configs: LayeredConfigs) -> None:
        runners = Runners(http_client, layered_configs=layered_configs)
        response = CreateConfigResponse(config_id="c1", leaf_workload_key="wk-leaf")
        mock_resp = httpx.Response(200, json={"runner_id": "r-123", "host_address": "10.0.0.1:8080"})
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runners.allocate(response)
        assert result.runner_id == "r-123"

    def test_allocate_preserves_raw_workload_key_when_name_lookup_misses(
        self,
        http_client: HttpClient,
        layered_configs: LayeredConfigs,
    ) -> None:
        runners = Runners(http_client, layered_configs=layered_configs)
        mock_resp = httpx.Response(200, json={"runner_id": "r-123", "host_address": "10.0.0.1:8080"})
        with patch.object(layered_configs, "resolve_workload_ref", side_effect=CapsuleNotFound("missing")):
            with patch.object(http_client._client, "request", return_value=mock_resp) as request:
                runners.allocate("wk-raw")
        assert request.call_args.kwargs["json"]["workload_key"] == "wk-raw"

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

    def test_resolve_host_uses_cache(self, runners: Runners) -> None:
        runners._host_cache["r-1"] = "cached-host:8080"
        host = runners._resolve_host("r-1")
        assert host == "http://cached-host:8080"

    def test_wait_ready_retries_until_ready(self, runners: Runners) -> None:
        statuses = [
            RunnerStatus(runner_id="r-1", status="pending"),
            RunnerStatus(runner_id="r-1", status="ready"),
        ]
        with patch.object(runners, "status", side_effect=statuses):
            with patch("capsule_sdk.resources.runners.time.sleep"):
                result = runners.wait_ready("r-1", timeout=5.0, poll_interval=0.1)
        assert result.status == "ready"

    def test_wait_ready_raises_on_terminal_status(self, runners: Runners) -> None:
        with patch.object(runners, "status", return_value=RunnerStatus(runner_id="r-1", status="terminated")):
            with pytest.raises(CapsuleRunnerUnavailableError):
                runners.wait_ready("r-1", timeout=1.0, poll_interval=0.1)

    def test_wait_ready_raises_structured_timeout(self, runners: Runners) -> None:
        with patch.object(runners, "status", return_value=RunnerStatus(runner_id="r-1", status="pending")):
            with patch("capsule_sdk.resources.runners.time.sleep"):
                with pytest.raises(CapsuleOperationTimeoutError):
                    runners.wait_ready("r-1", timeout=0.0, poll_interval=0.1)

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

    def test_allocate_ready_waits_for_runner(self, runners: Runners) -> None:
        alloc = AllocateRunnerResponse(
            runner_id="r-42",
            host_address="10.0.0.1:8080",
            session_id="s-1",
            request_id="req-1",
        )
        with patch.object(runners, "allocate", return_value=alloc) as allocate:
            with patch.object(RunnerSession, "wait_ready", return_value=None) as wait_ready:
                session = runners.allocate_ready("wk-1", startup_timeout=5.0)
        assert session.runner_id == "r-42"
        assert session.request_id == "req-1"
        allocate.assert_called_once()
        wait_ready.assert_called_once()

    def test_allocate_ready_converts_wait_timeout(self, runners: Runners) -> None:
        alloc = AllocateRunnerResponse(runner_id="r-42", host_address="10.0.0.1:8080", request_id="req-1")
        with patch.object(runners, "allocate", return_value=alloc):
            with patch.object(RunnerSession, "wait_ready", side_effect=CapsuleOperationTimeoutError("too slow")):
                with pytest.raises(CapsuleAllocationTimeoutError):
                    runners.allocate_ready("wk-1", startup_timeout=1.0)

    def test_from_config_uses_allocate_ready_by_default(self, runners: Runners) -> None:
        session = RunnerSession(runners, "r-42", host_address="10.0.0.1:8080", session_id="s-1", request_id="req-1")
        with patch.object(runners, "allocate_ready", return_value=session) as allocate_ready:
            result = runners.from_config("my-workload", tag="stable")
        assert result is session
        allocate_ready.assert_called_once()

    def test_from_config_without_wait_ready_uses_allocate(self, runners: Runners) -> None:
        alloc = AllocateRunnerResponse(
            runner_id="r-42",
            host_address="10.0.0.1:8080",
            session_id="s-1",
            request_id="req-1",
        )
        with patch.object(runners, "allocate", return_value=alloc) as allocate:
            session = runners.from_config("wk-1", wait_ready=False)
        assert session.runner_id == "r-42"
        assert session.request_id == "req-1"
        allocate.assert_called_once()

    def test_from_config_passes_named_workload_through(
        self,
        http_client: HttpClient,
        layered_configs: LayeredConfigs,
    ) -> None:
        runners = Runners(http_client, layered_configs=layered_configs)
        session = RunnerSession(runners, "r-42", host_address="10.0.0.1:8080", request_id="req-1")
        with patch.object(runners, "allocate_ready", return_value=session) as allocate_ready:
            result = runners.from_config("My Sandbox")
        assert result is session
        assert allocate_ready.call_args.args[0] == "My Sandbox"
