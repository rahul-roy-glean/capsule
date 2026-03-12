from __future__ import annotations

from unittest.mock import patch

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
from capsule_sdk._http import HttpClient
from capsule_sdk.resources.runners import Runners


@pytest.fixture
def config() -> ConnectionConfig:
    return ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")


@pytest.fixture
def http(config: ConnectionConfig) -> HttpClient:
    client = HttpClient(config)
    yield client
    client.close()


class TestHttpClient:
    def test_get_success(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"runners": [], "count": 0})
        with patch.object(http._client, "request", return_value=mock_resp):
            result = http.get("/api/v1/runners")
        assert result["runners"] == []
        assert result["count"] == 0
        assert result["request_id"]

    def test_post_success(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"runner_id": "r-1", "host_address": "h:8080"})
        with patch.object(http._client, "request", return_value=mock_resp):
            result = http.post("/api/v1/runners/allocate", json_body={"workload_key": "wk1"})
        assert result["runner_id"] == "r-1"

    def test_401_raises_auth_error(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(401, json={"error": "bad key"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with pytest.raises(CapsuleAuthError) as exc_info:
                http.get("/api/v1/runners")
            assert exc_info.value.status_code == 401

    def test_404_raises_not_found(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(404, json={"error": "runner not found"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with pytest.raises(CapsuleNotFound):
                http.get("/api/v1/runners/status")

    def test_429_retries_then_raises(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(429, json={"error": "rate limited"}, headers={"Retry-After": "0"})
        with patch.object(http._client, "request", return_value=mock_resp) as request:
            with patch("capsule_sdk._http.time.sleep"):  # skip actual sleep
                with pytest.raises(CapsuleRateLimited):
                    http.get("/api/v1/runners")
        assert request.call_count == 4

    def test_503_retries_then_raises(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(503, json={"error": "unavailable"}, headers={"Retry-After": "0"})
        with patch.object(http._client, "request", return_value=mock_resp) as request:
            with patch("capsule_sdk._http.time.sleep"):
                with pytest.raises(CapsuleServiceUnavailable) as exc_info:
                    http.get("/api/v1/runners/status")
                assert exc_info.value.status_code == 503
        assert request.call_count == 4

    def test_500_raises_http_error(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(500, json={"error": "internal"})
        with patch.object(http._client, "request", return_value=mock_resp) as request:
            with pytest.raises(CapsuleHTTPError) as exc_info:
                http.post("/api/v1/runners/release", json_body={"runner_id": "r-1"})
            assert exc_info.value.status_code == 500
        assert request.call_count == 1

    def test_connection_error_retries(self, http: HttpClient) -> None:
        with patch.object(http._client, "request", side_effect=httpx.ConnectError("refused")) as request:
            with patch("capsule_sdk._http.time.sleep"):
                with pytest.raises(CapsuleConnectionError):
                    http.get("/api/v1/runners")
        assert request.call_count == 4

    def test_timeout_error_retries(self, http: HttpClient) -> None:
        with patch.object(http._client, "request", side_effect=httpx.ReadTimeout("timeout")) as request:
            with patch("capsule_sdk._http.time.sleep"):
                with pytest.raises(CapsuleRequestTimeoutError):
                    http.get("/api/v1/runners")
        assert request.call_count == 4

    def test_post_timeout_does_not_retry_by_default(self, http: HttpClient) -> None:
        with patch.object(http._client, "request", side_effect=httpx.ReadTimeout("timeout")) as request:
            with patch("capsule_sdk._http.time.sleep"):
                with pytest.raises(CapsuleRequestTimeoutError):
                    http.post("/api/v1/runners/release", json_body={"runner_id": "r-1"})
        assert request.call_count == 1

    def test_retry_succeeds_on_second_attempt(self, http: HttpClient) -> None:
        fail_resp = httpx.Response(503, json={"error": "unavailable"}, headers={"Retry-After": "0"})
        ok_resp = httpx.Response(200, json={"status": "ok"})
        with patch.object(http._client, "request", side_effect=[fail_resp, ok_resp]):
            with patch("capsule_sdk._http.time.sleep"):
                result = http.get("/api/v1/runners/status")
        assert result["status"] == "ok"
        assert result["request_id"]

    def test_delete_success(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(204, text="")
        with patch.object(http._client, "request", return_value=mock_resp):
            result = http.delete("/api/v1/layered-configs/wk1/tags/stable")
        assert result["_raw"] == ""

    def test_post_429_does_not_retry_by_default(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(429, json={"error": "rate limited"}, headers={"Retry-After": "0"})
        with patch.object(http._client, "request", return_value=mock_resp) as request:
            with pytest.raises(CapsuleRateLimited):
                http.post("/api/v1/runners/release", json_body={"runner_id": "r-1"})
        assert request.call_count == 1


class TestAllocateRetries:
    def test_allocate_retries_and_reuses_request_id(self, http: HttpClient) -> None:
        runners = Runners(http)
        fail_resp = httpx.Response(429, json={"error": "rate limited"}, headers={"Retry-After": "0"})
        ok_resp = httpx.Response(200, json={"runner_id": "r-1", "host_address": "10.0.0.1:8080"})

        with patch.object(http._client, "request", side_effect=[fail_resp, ok_resp]) as request:
            with patch("capsule_sdk.resources.runners.time.sleep"):
                result = runners.allocate("wk-1", request_id="req-fixed", startup_timeout=2.0)

        assert result.runner_id == "r-1"
        assert result.request_id == "req-fixed"
        assert request.call_count == 2
        first_headers = request.call_args_list[0].kwargs["headers"]
        second_headers = request.call_args_list[1].kwargs["headers"]
        assert first_headers["X-Request-Id"] == "req-fixed"
        assert second_headers["X-Request-Id"] == "req-fixed"
        first_body = request.call_args_list[0].kwargs["json"]
        assert first_body["request_id"] == "req-fixed"
