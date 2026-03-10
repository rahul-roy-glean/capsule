from __future__ import annotations

import json
import random
import time
import uuid
from collections.abc import Iterator
from typing import Any, cast

import httpx

from bf_sdk._config import ConnectionConfig
from bf_sdk._errors import (
    BFAuthError,
    BFConflict,
    BFConnectionError,
    BFHTTPError,
    BFNotFound,
    BFRateLimited,
    BFServiceUnavailable,
    BFTimeoutError,
)

_RETRYABLE_STATUS_CODES = {429, 502, 503, 504}
_MAX_RETRIES = 3
_BASE_BACKOFF = 0.5


class HttpClient:
    """Synchronous HTTP client with auth, retries, and ndjson streaming."""

    def __init__(self, config: ConnectionConfig) -> None:
        self._config = config
        headers: dict[str, str] = {
            "User-Agent": config.user_agent,
            "Accept": "application/json",
        }
        if config.token:
            headers["Authorization"] = f"Bearer {config.token}"

        self._client = httpx.Client(
            base_url=config.base_url,
            headers=headers,
            timeout=httpx.Timeout(config.timeout),
        )

    def close(self) -> None:
        self._client.close()

    # -- Public request methods ------------------------------------------------

    def get(self, url: str, *, params: dict[str, str] | None = None) -> dict[str, Any]:
        return self._request("GET", url, params=params)

    def post(self, url: str, *, json_body: dict[str, Any] | None = None) -> dict[str, Any]:
        return self._request("POST", url, json_body=json_body)

    def delete(self, url: str) -> dict[str, Any]:
        return self._request("DELETE", url)

    def get_bytes(
        self,
        url: str,
        *,
        base_url: str | None = None,
        params: dict[str, str] | None = None,
    ) -> bytes:
        """Streaming GET that returns raw bytes (for file download)."""
        request_id = str(uuid.uuid4())
        headers = {"X-Request-Id": request_id}

        client = self._client
        if base_url:
            client = httpx.Client(
                base_url=base_url,
                headers=dict(self._client.headers),
                timeout=httpx.Timeout(None),
            )

        try:
            with client.stream("GET", url, params=params, headers=headers) as resp:
                if resp.status_code >= 400:
                    resp.read()
                    self._raise_for_status(resp, request_id)
                return resp.read()
        except httpx.ConnectError as exc:
            raise BFConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise BFTimeoutError(str(exc)) from exc
        finally:
            if base_url:
                client.close()

    def post_bytes(
        self,
        url: str,
        *,
        data: bytes,
        base_url: str | None = None,
        params: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        """POST with raw binary body, returns JSON response (for file upload)."""
        request_id = str(uuid.uuid4())
        headers = {"X-Request-Id": request_id, "Content-Type": "application/octet-stream"}

        client = self._client
        if base_url:
            client = httpx.Client(
                base_url=base_url,
                headers=dict(self._client.headers),
                timeout=httpx.Timeout(None),
            )

        try:
            resp = client.post(url, content=data, params=params, headers=headers)
            if resp.status_code >= 400:
                self._raise_for_status(resp, request_id)
            try:
                return resp.json()  # type: ignore[no-any-return]
            except (json.JSONDecodeError, ValueError):
                return {"_raw": resp.text}
        except httpx.ConnectError as exc:
            raise BFConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise BFTimeoutError(str(exc)) from exc
        finally:
            if base_url:
                client.close()

    def post_to_host(
        self,
        url: str,
        *,
        json_body: dict[str, Any] | None = None,
        base_url: str | None = None,
    ) -> dict[str, Any]:
        """POST to host agent with no timeout. Returns JSON response."""
        request_id = str(uuid.uuid4())
        headers = {"X-Request-Id": request_id}

        client = self._client
        if base_url:
            client = httpx.Client(
                base_url=base_url,
                headers=dict(self._client.headers),
                timeout=httpx.Timeout(None),
            )

        try:
            resp = client.post(url, json=json_body, headers=headers)
            if resp.status_code >= 400:
                self._raise_for_status(resp, request_id)
            try:
                return resp.json()  # type: ignore[no-any-return]
            except (json.JSONDecodeError, ValueError):
                return {"_raw": resp.text}
        except httpx.ConnectError as exc:
            raise BFConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise BFTimeoutError(str(exc)) from exc
        finally:
            if base_url:
                client.close()

    def post_stream_ndjson(
        self,
        url: str,
        *,
        json_body: dict[str, Any] | None = None,
        base_url: str | None = None,
    ) -> Iterator[dict[str, Any]]:
        """POST and stream back ndjson lines. Yields dicts."""
        request_id = str(uuid.uuid4())
        headers = {"X-Request-Id": request_id, "Accept": "application/x-ndjson"}

        client = self._client
        if base_url:
            client = httpx.Client(
                base_url=base_url,
                headers=dict(self._client.headers),
                timeout=httpx.Timeout(None),  # no timeout for streaming
            )

        try:
            with client.stream(
                "POST",
                url,
                json=json_body,
                headers=headers,
            ) as resp:
                if resp.status_code >= 400:
                    resp.read()
                    self._raise_for_status(resp, request_id)
                for line in resp.iter_lines():
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        yield json.loads(line)
                    except json.JSONDecodeError:
                        continue
        except httpx.ConnectError as exc:
            raise BFConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise BFTimeoutError(str(exc)) from exc
        finally:
            if base_url:
                client.close()

    # -- Internal --------------------------------------------------------------

    def _request(
        self,
        method: str,
        url: str,
        *,
        params: dict[str, str] | None = None,
        json_body: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        request_id = str(uuid.uuid4())
        last_exc: Exception | None = None

        for attempt in range(_MAX_RETRIES):
            try:
                resp = self._client.request(
                    method,
                    url,
                    params=params,
                    json=json_body,
                    headers={"X-Request-Id": request_id},
                )

                if resp.status_code < 400:
                    try:
                        return resp.json()  # type: ignore[no-any-return]
                    except (json.JSONDecodeError, ValueError):
                        return {"_raw": resp.text}

                if resp.status_code in _RETRYABLE_STATUS_CODES and attempt < _MAX_RETRIES - 1:
                    retry_after = _parse_retry_after(resp)
                    self._backoff(attempt, retry_after)
                    continue

                self._raise_for_status(resp, request_id)

            except (httpx.ConnectError, httpx.ReadError, httpx.WriteError) as exc:
                last_exc = exc
                if attempt < _MAX_RETRIES - 1:
                    self._backoff(attempt)
                    continue
                raise BFConnectionError(str(exc)) from exc

            except httpx.TimeoutException as exc:
                last_exc = exc
                # Only retry timeouts for idempotent methods. Retrying a
                # POST/DELETE timeout is dangerous: the server may have
                # received the request and started processing it, so a
                # retry would create a duplicate operation.
                if method == "GET" and attempt < _MAX_RETRIES - 1:
                    self._backoff(attempt)
                    continue
                raise BFTimeoutError(str(exc)) from exc

        # Should not reach here, but satisfy type checker
        if last_exc:
            raise BFConnectionError(str(last_exc)) from last_exc
        raise BFConnectionError("Max retries exceeded")  # pragma: no cover

    def _raise_for_status(self, resp: httpx.Response, request_id: str) -> None:
        body: dict[str, Any] = {}
        try:
            raw = resp.json()
            if isinstance(raw, dict):
                body = cast(dict[str, Any], raw)
        except (json.JSONDecodeError, ValueError):
            pass

        message: str = str(body.get("error", resp.text))
        kwargs: dict[str, Any] = {"request_id": request_id}

        status = resp.status_code
        if status == 401:
            raise BFAuthError(message, **kwargs)
        if status == 404:
            raise BFNotFound(message, **kwargs)
        if status == 409:
            raise BFConflict(message, **kwargs)
        if status == 429:
            raise BFRateLimited(message, retry_after=_parse_retry_after(resp), **kwargs)
        if status == 503:
            raise BFServiceUnavailable(message, retry_after=_parse_retry_after(resp), **kwargs)
        raise BFHTTPError(status, message, **kwargs)

    @staticmethod
    def _backoff(attempt: int, retry_after: float | None = None) -> None:
        if retry_after and retry_after > 0:
            time.sleep(retry_after)
        else:
            delay = _BASE_BACKOFF * (2**attempt) + random.uniform(0, 0.5)
            time.sleep(delay)


def _parse_retry_after(resp: httpx.Response) -> float | None:
    raw = resp.headers.get("Retry-After")
    if raw is None:
        return None
    try:
        return float(raw)
    except ValueError:
        return None
