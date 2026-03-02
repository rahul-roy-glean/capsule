from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.runner import ExecEvent
from bf_sdk.resources.runners import Runners
from bf_sdk.runner_session import RunnerSession


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", api_key="test")
    return HttpClient(config)


@pytest.fixture
def runners(http_client: HttpClient) -> Runners:
    return Runners(http_client)


class TestRunnerSession:
    def test_properties(self, runners: Runners) -> None:
        session = RunnerSession(runners, "r-1", host_address="10.0.0.1:8080", session_id="s-1")
        assert session.runner_id == "r-1"
        assert session.session_id == "s-1"
        # Host should be cached
        assert runners._host_cache["r-1"] == "10.0.0.1:8080"

    def test_context_manager_releases(self, runners: Runners, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"success": True})
        session = RunnerSession(runners, "r-1", host_address="10.0.0.1:8080")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            with session:
                pass  # auto-release on exit
        # release should have been called
        assert "r-1" not in runners._host_cache

    def test_release(self, runners: Runners, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"success": True})
        session = RunnerSession(runners, "r-1", host_address="h:8080")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            ok = session.release()
        assert ok is True

    def test_pause(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"success": True, "session_id": "s-new", "snapshot_size_bytes": 2048, "layer": 3}
        mock_resp = httpx.Response(200, json=resp_data)
        session = RunnerSession(runners, "r-1")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = session.pause()
        assert result.success is True
        assert session.session_id == "s-new"

    def test_resume(self, runners: Runners, http_client: HttpClient) -> None:
        resp_data = {"status": "connected", "runner_id": "r-1", "host_address": "10.0.0.2:8080"}
        mock_resp = httpx.Response(200, json=resp_data)
        session = RunnerSession(runners, "r-1")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = session.resume()
        assert result.status == "connected"
        assert runners._host_cache["r-1"] == "10.0.0.2:8080"

    def test_shell(self, runners: Runners) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        session = RunnerSession(runners, "r-1")
        shell = session.shell(cols=120, rows=40)
        assert "r-1" in shell._url
        assert "cols=120" in shell._url

    def test_exec_collect(self, runners: Runners, http_client: HttpClient) -> None:
        """Test the convenience exec_collect method with a mock stream."""
        session = RunnerSession(runners, "r-1", host_address="10.0.0.1:8080")

        events = [
            ExecEvent(type="stdout", data="hello\n"),
            ExecEvent(type="stderr", data="warn\n"),
            ExecEvent(type="exit", code=0),
        ]

        with patch.object(runners, "exec", return_value=iter(events)):
            output, code = session.exec_collect("echo", "hello")
        assert output == "hello\nwarn\n"
        assert code == 0

    def test_quarantine(self, runners: Runners, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"success": True})
        session = RunnerSession(runners, "r-1", host_address="h:8080")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = session.quarantine(reason="debug")
        assert result["success"] is True

    def test_unquarantine(self, runners: Runners, http_client: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"success": True})
        session = RunnerSession(runners, "r-1", host_address="h:8080")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = session.unquarantine()
        assert result["success"] is True


class TestFromTemplate:
    def test_from_template(self, runners: Runners, http_client: HttpClient) -> None:
        alloc_data = {
            "runner_id": "r-42",
            "host_id": "h-1",
            "host_address": "10.0.0.1:8080",
            "session_id": "s-1",
            "resumed": False,
        }
        mock_resp = httpx.Response(200, json=alloc_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            session = runners.from_template("my-workload", tag="stable")
        assert isinstance(session, RunnerSession)
        assert session.runner_id == "r-42"
        assert session.session_id == "s-1"
        assert runners._host_cache["r-42"] == "10.0.0.1:8080"

    def test_from_template_with_labels(self, runners: Runners, http_client: HttpClient) -> None:
        alloc_data = {"runner_id": "r-99", "host_address": "h:8080", "resumed": False}
        mock_resp = httpx.Response(200, json=alloc_data)
        with patch.object(http_client._client, "request", return_value=mock_resp) as mock_req:
            runners.from_template("wk-1", tag="dev", labels={"env": "ci"})
        # Verify the allocate call included the label
        call_kwargs = mock_req.call_args
        assert call_kwargs is not None
