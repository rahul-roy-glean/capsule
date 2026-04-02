from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, Mock, patch

import httpx
import pytest

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._errors import (
    CapsuleAllocationTimeoutError,
    CapsuleNotFound,
    CapsuleOperationTimeoutError,
    CapsuleRunnerUnavailableError,
)
from capsule_sdk._http_async import AsyncHttpClient
from capsule_sdk.async_runner_session import AsyncRunnerSession
from capsule_sdk.models.layered_config import CreateConfigResponse
from capsule_sdk.models.runner import AllocateRunnerResponse, RunnerStatus
from capsule_sdk.models.workload import ResolvedWorkloadRef
from capsule_sdk.resources.async_layered_configs import AsyncLayeredConfigs
from capsule_sdk.resources.async_runners import AsyncRunners


@pytest.fixture
def http_client() -> AsyncHttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    client = AsyncHttpClient(config)
    yield client
    asyncio.run(client.close())


@pytest.fixture
def runners(http_client: AsyncHttpClient) -> AsyncRunners:
    return AsyncRunners(http_client)


@pytest.fixture
def layered_configs(http_client: AsyncHttpClient) -> AsyncLayeredConfigs:
    return AsyncLayeredConfigs(http_client)


class TestAsyncRunners:
    def test_allocate(self, runners: AsyncRunners, http_client: AsyncHttpClient) -> None:
        resp_data = {
            "runner_id": "r-123",
            "host_id": "h-1",
            "host_address": "10.0.0.1:8080",
            "session_id": "s-1",
            "resumed": False,
        }
        mock_resp = httpx.Response(200, json=resp_data)

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)) as request:
                result = await runners.allocate("my-workload", labels={"env": "test"}, request_id="req-1")
            assert result.runner_id == "r-123"
            assert result.host_address == "10.0.0.1:8080"
            assert result.request_id == "req-1"
            assert runners._host_cache["r-123"] == "10.0.0.1:8080"
            assert request.await_args.kwargs["json"]["request_id"] == "req-1"

        asyncio.run(run())

    def test_allocate_resolves_display_name(
        self,
        http_client: AsyncHttpClient,
        layered_configs: AsyncLayeredConfigs,
    ) -> None:
        runners = AsyncRunners(http_client, layered_configs=layered_configs)
        resp_data = {"runner_id": "r-123", "host_address": "10.0.0.1:8080"}
        mock_resp = httpx.Response(200, json=resp_data)

        async def run() -> None:
            with patch.object(
                layered_configs,
                "resolve_workload_ref",
                AsyncMock(return_value=ResolvedWorkloadRef(display_name="My Sandbox", workload_key="wk-leaf")),
            ) as resolve:
                with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)):
                    result = await runners.allocate("My Sandbox")
            assert result.runner_id == "r-123"
            resolve.assert_awaited_once_with("My Sandbox")

        asyncio.run(run())

    def test_allocate_preserves_raw_workload_key_when_name_lookup_misses(
        self,
        http_client: AsyncHttpClient,
        layered_configs: AsyncLayeredConfigs,
    ) -> None:
        runners = AsyncRunners(http_client, layered_configs=layered_configs)
        mock_resp = httpx.Response(200, json={"runner_id": "r-123", "host_address": "10.0.0.1:8080"})

        async def run() -> None:
            with patch.object(
                layered_configs,
                "resolve_workload_ref",
                AsyncMock(side_effect=CapsuleNotFound("missing")),
            ):
                with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)) as request:
                    await runners.allocate("wk-raw")
            assert request.await_args.kwargs["json"]["workload_key"] == "wk-raw"

        asyncio.run(run())

    def test_wait_ready_retries_until_ready(self, runners: AsyncRunners) -> None:
        statuses = [
            RunnerStatus(runner_id="r-1", status="pending"),
            RunnerStatus(runner_id="r-1", status="ready"),
        ]

        async def run() -> None:
            with patch.object(runners, "status", AsyncMock(side_effect=statuses)):
                with patch("capsule_sdk.resources.async_runners.asyncio.sleep", AsyncMock()):
                    result = await runners.wait_ready("r-1", timeout=5.0, poll_interval=0.1)
            assert result.status == "ready"

        asyncio.run(run())

    def test_wait_ready_raises_on_terminal_status(self, runners: AsyncRunners) -> None:
        async def run() -> None:
            terminal = RunnerStatus(runner_id="r-1", status="terminated")
            with patch.object(runners, "status", AsyncMock(return_value=terminal)):
                with pytest.raises(CapsuleRunnerUnavailableError):
                    await runners.wait_ready("r-1", timeout=1.0, poll_interval=0.1)

        asyncio.run(run())

    def test_wait_ready_raises_structured_timeout(self, runners: AsyncRunners) -> None:
        async def run() -> None:
            pending = RunnerStatus(runner_id="r-1", status="pending")
            with patch.object(runners, "status", AsyncMock(return_value=pending)):
                with patch("capsule_sdk.resources.async_runners.asyncio.sleep", AsyncMock()):
                    with pytest.raises(CapsuleOperationTimeoutError):
                        await runners.wait_ready("r-1", timeout=0.0, poll_interval=0.1)

        asyncio.run(run())

    def test_shell_resolves_url(self, runners: AsyncRunners) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        session = runners.shell("r-1", cols=120, rows=40)

        async def run() -> None:
            with patch.object(session, "_connect", AsyncMock(return_value=Mock())):
                await session.connect()
            assert session._url == "ws://10.0.0.1:8080/api/v1/runners/r-1/pty?cols=120&rows=40"

        asyncio.run(run())

    def test_allocate_ready_waits_for_runner(self, runners: AsyncRunners) -> None:
        alloc = AllocateRunnerResponse(
            runner_id="r-42",
            host_address="10.0.0.1:8080",
            session_id="s-1",
            request_id="req-1",
        )

        async def run() -> None:
            with patch.object(runners, "allocate", AsyncMock(return_value=alloc)) as allocate:
                with patch.object(AsyncRunnerSession, "wait_ready", AsyncMock(return_value=None)) as wait_ready:
                    session = await runners.allocate_ready("wk-1", startup_timeout=5.0)
            assert session.runner_id == "r-42"
            assert session.request_id == "req-1"
            allocate.assert_awaited_once()
            wait_ready.assert_awaited_once()

        asyncio.run(run())

    def test_allocate_ready_converts_wait_timeout(self, runners: AsyncRunners) -> None:
        alloc = AllocateRunnerResponse(runner_id="r-42", host_address="10.0.0.1:8080", request_id="req-1")

        async def run() -> None:
            with patch.object(runners, "allocate", AsyncMock(return_value=alloc)):
                with patch.object(
                    AsyncRunnerSession,
                    "wait_ready",
                    AsyncMock(side_effect=CapsuleOperationTimeoutError("too slow")),
                ):
                    with pytest.raises(CapsuleAllocationTimeoutError):
                        await runners.allocate_ready("wk-1", startup_timeout=1.0)

        asyncio.run(run())

    def test_from_config_uses_allocate_ready_by_default(self, runners: AsyncRunners) -> None:
        session = AsyncRunnerSession(
            runners,
            "r-42",
            host_address="10.0.0.1:8080",
            session_id="s-1",
            request_id="req-1",
        )

        async def run() -> None:
            with patch.object(runners, "allocate_ready", AsyncMock(return_value=session)) as allocate_ready:
                result = await runners.from_config("my-workload")
            assert result is session
            allocate_ready.assert_awaited_once()

        asyncio.run(run())

    def test_allocate_accepts_create_response(
        self,
        http_client: AsyncHttpClient,
        layered_configs: AsyncLayeredConfigs,
    ) -> None:
        runners = AsyncRunners(http_client, layered_configs=layered_configs)
        response = CreateConfigResponse(config_id="c1", leaf_workload_key="wk-leaf")
        mock_resp = httpx.Response(200, json={"runner_id": "r-123", "host_address": "10.0.0.1:8080"})

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)):
                result = await runners.allocate(response)
            assert result.runner_id == "r-123"

        asyncio.run(run())

    def test_list_with_detail(self, runners: AsyncRunners, http_client: AsyncHttpClient) -> None:
        resp_data = {
            "runners": [{
                "runner_id": "r-1",
                "host_id": "h1",
                "status": "running",
                "age_seconds": 120,
                "sessions": [{"session_id": "s-1", "status": "active", "layer_count": 3}],
            }],
            "count": 1,
            "pagination": {"has_more": False}
        }
        mock_resp = httpx.Response(200, json=resp_data)

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)) as request:
                result = await runners.list(detail=True)
            assert len(result) == 1
            assert result[0].runner_id == "r-1"
            assert result[0].age_seconds == 120
            assert request.await_args.kwargs["params"]["detail"] == "full"

        asyncio.run(run())

    def test_list_with_pagination(self, runners: AsyncRunners, http_client: AsyncHttpClient) -> None:
        resp_data = {
            "runners": [{"runner_id": f"r-{i}", "host_id": "h1", "status": "running"} for i in range(10)],
            "count": 10,
            "pagination": {"has_more": True, "next_cursor": "cursor-abc"}
        }
        mock_resp = httpx.Response(200, json=resp_data)

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)) as request:
                result = await runners.list(limit=10)
            assert len(result) == 10
            assert request.await_args.kwargs["params"]["limit"] == "10"

        asyncio.run(run())

    def test_list_paginated(self, runners: AsyncRunners, http_client: AsyncHttpClient) -> None:
        resp_data = {
            "runners": [{"runner_id": "r-1", "host_id": "h1", "status": "running"}],
            "count": 1,
            "pagination": {"has_more": True, "next_cursor": "cursor-xyz"}
        }
        mock_resp = httpx.Response(200, json=resp_data)

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)):
                result = await runners.list_paginated(limit=1)
            assert len(result.runners) == 1
            assert result.pagination is not None
            assert result.pagination.has_more is True
            assert result.pagination.next_cursor == "cursor-xyz"

        asyncio.run(run())

    def test_list_with_filters(self, runners: AsyncRunners, http_client: AsyncHttpClient) -> None:
        resp_data = {
            "runners": [{"runner_id": "r-1", "host_id": "h1", "status": "busy", "workload_key": "wk-1"}],
            "count": 1,
            "pagination": {"has_more": False}
        }
        mock_resp = httpx.Response(200, json=resp_data)

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)) as request:
                result = await runners.list(status="busy", host_id="h1", workload_key="wk-1")
            assert len(result) == 1
            params = request.await_args.kwargs["params"]
            assert params["status"] == "busy"
            assert params["host_id"] == "h1"
            assert params["workload_key"] == "wk-1"

        asyncio.run(run())
