from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, patch

import httpx
import pytest

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._errors import (
    CapsuleAuthError,
    CapsuleConnectionError,
    CapsuleHTTPError,
    CapsuleNotFound,
    CapsuleRateLimited,
    CapsuleRequestTimeoutError,
    CapsuleServiceUnavailable,
)
from capsule_sdk._http_async import AsyncHttpClient


@pytest.fixture
def config() -> ConnectionConfig:
    return ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")


@pytest.fixture
def http(config: ConnectionConfig) -> AsyncHttpClient:
    client = AsyncHttpClient(config)
    yield client
    asyncio.run(client.close())


class TestAsyncHttpClient:
    def test_get_success(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(200, json={"runners": [], "count": 0})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)):
                result = await http.get("/api/v1/runners")
            assert result["runners"] == []
            assert result["count"] == 0
            assert result["request_id"]

        asyncio.run(run())

    def test_post_success(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(200, json={"runner_id": "r-1", "host_address": "h:8080"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)):
                result = await http.post("/api/v1/runners/allocate", json_body={"workload_key": "wk1"})
            assert result["runner_id"] == "r-1"

        asyncio.run(run())

    def test_401_raises_auth_error(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(401, json={"error": "bad key"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)):
                with pytest.raises(CapsuleAuthError) as exc_info:
                    await http.get("/api/v1/runners")
                assert exc_info.value.status_code == 401

        asyncio.run(run())

    def test_404_raises_not_found(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(404, json={"error": "runner not found"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)):
                with pytest.raises(CapsuleNotFound):
                    await http.get("/api/v1/runners/status")

        asyncio.run(run())

    def test_429_retries_then_raises(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(429, json={"error": "rate limited"}, headers={"Retry-After": "0"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)) as request:
                with patch("capsule_sdk._http_async.asyncio.sleep", AsyncMock()):
                    with pytest.raises(CapsuleRateLimited):
                        await http.get("/api/v1/runners")
            assert request.await_count == 4

        asyncio.run(run())

    def test_503_retries_then_raises(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(503, json={"error": "unavailable"}, headers={"Retry-After": "0"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)) as request:
                with patch("capsule_sdk._http_async.asyncio.sleep", AsyncMock()):
                    with pytest.raises(CapsuleServiceUnavailable) as exc_info:
                        await http.get("/api/v1/runners/status")
                    assert exc_info.value.status_code == 503
            assert request.await_count == 4

        asyncio.run(run())

    def test_500_raises_http_error(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(500, json={"error": "internal"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)) as request:
                with pytest.raises(CapsuleHTTPError) as exc_info:
                    await http.post("/api/v1/runners/release", json_body={"runner_id": "r-1"})
                assert exc_info.value.status_code == 500
            assert request.await_count == 1

        asyncio.run(run())

    def test_connection_error_retries(self, http: AsyncHttpClient) -> None:
        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(side_effect=httpx.ConnectError("refused"))) as request:
                with patch("capsule_sdk._http_async.asyncio.sleep", AsyncMock()):
                    with pytest.raises(CapsuleConnectionError):
                        await http.get("/api/v1/runners")
            assert request.await_count == 4

        asyncio.run(run())

    def test_timeout_error_retries(self, http: AsyncHttpClient) -> None:
        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(side_effect=httpx.ReadTimeout("timeout"))) as request:
                with patch("capsule_sdk._http_async.asyncio.sleep", AsyncMock()):
                    with pytest.raises(CapsuleRequestTimeoutError):
                        await http.get("/api/v1/runners")
            assert request.await_count == 4

        asyncio.run(run())

    def test_post_timeout_does_not_retry_by_default(self, http: AsyncHttpClient) -> None:
        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(side_effect=httpx.ReadTimeout("timeout"))) as request:
                with patch("capsule_sdk._http_async.asyncio.sleep", AsyncMock()):
                    with pytest.raises(CapsuleRequestTimeoutError):
                        await http.post("/api/v1/runners/release", json_body={"runner_id": "r-1"})
            assert request.await_count == 1

        asyncio.run(run())

    def test_retry_succeeds_on_second_attempt(self, http: AsyncHttpClient) -> None:
        fail_resp = httpx.Response(503, json={"error": "unavailable"}, headers={"Retry-After": "0"})
        ok_resp = httpx.Response(200, json={"status": "ok"})

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(side_effect=[fail_resp, ok_resp])):
                with patch("capsule_sdk._http_async.asyncio.sleep", AsyncMock()):
                    result = await http.get("/api/v1/runners/status")
            assert result["status"] == "ok"
            assert result["request_id"]

        asyncio.run(run())

    def test_delete_success(self, http: AsyncHttpClient) -> None:
        mock_resp = httpx.Response(204, text="")

        async def run() -> None:
            with patch.object(http._client, "request", AsyncMock(return_value=mock_resp)):
                result = await http.delete("/api/v1/layered-configs/wk1/tags/stable")
            assert result["_raw"] == ""

        asyncio.run(run())

    def test_post_stream_ndjson(self, http: AsyncHttpClient) -> None:
        class DummyResponse:
            def __init__(self) -> None:
                self.status_code = 200

            async def __aenter__(self) -> DummyResponse:
                return self

            async def __aexit__(self, *_: object) -> None:
                return None

            async def aread(self) -> bytes:
                return b""

            async def aiter_lines(self):  # type: ignore[no-untyped-def]
                for line in ['{"type":"stdout","data":"hello"}', '', '{"type":"exit","code":0}']:
                    yield line

        class DummyClient:
            def stream(self, *_: object, **__: object) -> DummyResponse:
                return DummyResponse()

        class DummyContext:
            async def __aenter__(self) -> DummyClient:
                return DummyClient()

            async def __aexit__(self, *_: object) -> None:
                return None

        async def run() -> None:
            with patch.object(http, "_client_for", return_value=DummyContext()):
                result = [item async for item in http.post_stream_ndjson("/api/v1/runners/r-1/exec")]
            assert result == [{"type": "stdout", "data": "hello"}, {"type": "exit", "code": 0}]

        asyncio.run(run())

    def test_get_bytes(self, http: AsyncHttpClient) -> None:
        class DummyResponse:
            status_code = 200

            async def __aenter__(self) -> DummyResponse:
                return self

            async def __aexit__(self, *_: object) -> None:
                return None

            async def aread(self) -> bytes:
                return b"payload"

        class DummyClient:
            def stream(self, *_: object, **__: object) -> DummyResponse:
                return DummyResponse()

        class DummyContext:
            async def __aenter__(self) -> DummyClient:
                return DummyClient()

            async def __aexit__(self, *_: object) -> None:
                return None

        async def run() -> None:
            with patch.object(http, "_client_for", return_value=DummyContext()):
                payload = await http.get_bytes("/api/v1/runners/r-1/files/download")
            assert payload == b"payload"

        asyncio.run(run())
