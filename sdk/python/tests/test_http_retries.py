from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._errors import (
    BFAuthError,
    BFConnectionError,
    BFHTTPError,
    BFNotFound,
    BFRateLimited,
    BFServiceUnavailable,
    BFTimeoutError,
)
from bf_sdk._http import HttpClient


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
        assert result == {"runners": [], "count": 0}

    def test_post_success(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(200, json={"runner_id": "r-1", "host_address": "h:8080"})
        with patch.object(http._client, "request", return_value=mock_resp):
            result = http.post("/api/v1/runners/allocate", json_body={"workload_key": "wk1"})
        assert result["runner_id"] == "r-1"

    def test_401_raises_auth_error(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(401, json={"error": "bad key"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with pytest.raises(BFAuthError) as exc_info:
                http.get("/api/v1/runners")
            assert exc_info.value.status_code == 401

    def test_404_raises_not_found(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(404, json={"error": "runner not found"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with pytest.raises(BFNotFound):
                http.get("/api/v1/runners/status")

    def test_429_retries_then_raises(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(429, json={"error": "rate limited"}, headers={"Retry-After": "0"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with patch("bf_sdk._http.time.sleep"):  # skip actual sleep
                with pytest.raises(BFRateLimited):
                    http.get("/api/v1/runners")

    def test_503_retries_then_raises(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(503, json={"error": "unavailable"}, headers={"Retry-After": "0"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with patch("bf_sdk._http.time.sleep"):
                with pytest.raises(BFServiceUnavailable) as exc_info:
                    http.get("/api/v1/runners/status")
                assert exc_info.value.status_code == 503

    def test_500_raises_http_error(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(500, json={"error": "internal"})
        with patch.object(http._client, "request", return_value=mock_resp):
            with pytest.raises(BFHTTPError) as exc_info:
                http.post("/api/v1/runners/release", json_body={"runner_id": "r-1"})
            assert exc_info.value.status_code == 500

    def test_connection_error_retries(self, http: HttpClient) -> None:
        with patch.object(http._client, "request", side_effect=httpx.ConnectError("refused")):
            with patch("bf_sdk._http.time.sleep"):
                with pytest.raises(BFConnectionError):
                    http.get("/api/v1/runners")

    def test_timeout_error_retries(self, http: HttpClient) -> None:
        with patch.object(http._client, "request", side_effect=httpx.ReadTimeout("timeout")):
            with patch("bf_sdk._http.time.sleep"):
                with pytest.raises(BFTimeoutError):
                    http.get("/api/v1/runners")

    def test_retry_succeeds_on_second_attempt(self, http: HttpClient) -> None:
        fail_resp = httpx.Response(503, json={"error": "unavailable"}, headers={"Retry-After": "0"})
        ok_resp = httpx.Response(200, json={"status": "ok"})
        with patch.object(http._client, "request", side_effect=[fail_resp, ok_resp]):
            with patch("bf_sdk._http.time.sleep"):
                result = http.get("/api/v1/runners/status")
        assert result == {"status": "ok"}

    def test_delete_success(self, http: HttpClient) -> None:
        mock_resp = httpx.Response(204, text="")
        with patch.object(http._client, "request", return_value=mock_resp):
            result = http.delete("/api/v1/layered-configs/wk1/tags/stable")
        assert result == {"_raw": ""}
