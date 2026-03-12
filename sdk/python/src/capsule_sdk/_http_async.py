from __future__ import annotations

import asyncio
import json
import logging
import random
import uuid
from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from typing import Any, cast

import httpx

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._errors import (
    CapsuleAuthError,
    CapsuleConflict,
    CapsuleConnectionError,
    CapsuleHTTPError,
    CapsuleNotFound,
    CapsuleRateLimited,
    CapsuleRequestTimeoutError,
    CapsuleServiceUnavailable,
)

_RETRYABLE_STATUS_CODES = {429, 502, 503, 504}
_MAX_RETRIES = 3
_BASE_BACKOFF = 0.5
logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class RetryPolicy:
    max_retries: int = 0
    retry_status_codes: frozenset[int] = field(default_factory=lambda: frozenset())
    retry_transport_errors: bool = False
    retry_timeouts: bool = False


_GET_RETRY_POLICY = RetryPolicy(
    max_retries=_MAX_RETRIES,
    retry_status_codes=frozenset(_RETRYABLE_STATUS_CODES),
    retry_transport_errors=True,
    retry_timeouts=True,
)


class AsyncHttpClient:
    """Asynchronous HTTP client with auth, retries, and ndjson streaming."""

    def __init__(self, config: ConnectionConfig) -> None:
        self._config = config
        headers: dict[str, str] = {
            "User-Agent": config.user_agent,
            "Accept": "application/json",
        }
        if config.token:
            headers["Authorization"] = f"Bearer {config.token}"

        self._client: httpx.AsyncClient = httpx.AsyncClient(
            base_url=config.base_url,
            headers=headers,
            timeout=httpx.Timeout(config.request_timeout),
        )

    async def close(self) -> None:
        await self._client.aclose()

    @property
    def startup_timeout(self) -> float:
        return self._config.startup_timeout

    @property
    def operation_timeout(self) -> float:
        return self._config.operation_timeout

    async def get(
        self,
        url: str,
        *,
        params: dict[str, str] | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        return await self._request(
            "GET",
            url,
            params=params,
            request_id=request_id,
            timeout=timeout,
            retry_policy=_GET_RETRY_POLICY,
        )

    async def post(
        self,
        url: str,
        *,
        json_body: dict[str, Any] | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
        retry_policy: RetryPolicy | None = None,
    ) -> dict[str, Any]:
        return await self._request(
            "POST",
            url,
            json_body=json_body,
            request_id=request_id,
            timeout=timeout,
            retry_policy=retry_policy or RetryPolicy(),
        )

    async def delete(
        self,
        url: str,
        *,
        request_id: str | None = None,
        timeout: float | None = None,
        retry_policy: RetryPolicy | None = None,
    ) -> dict[str, Any]:
        return await self._request(
            "DELETE",
            url,
            request_id=request_id,
            timeout=timeout,
            retry_policy=retry_policy or RetryPolicy(),
        )

    async def get_bytes(
        self,
        url: str,
        *,
        base_url: str | None = None,
        params: dict[str, str] | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
    ) -> bytes:
        request_id = request_id or str(uuid.uuid4())
        headers = {"X-Request-Id": request_id}
        timeout_value = self._resolve_timeout(timeout, self._config.operation_timeout)

        try:
            async with self._client_for(base_url, timeout_value) as client:
                async with client.stream("GET", url, params=params, headers=headers) as resp:
                    if resp.status_code >= 400:
                        await resp.aread()
                        self._raise_for_status(resp, request_id)
                    return await resp.aread()
        except httpx.ConnectError as exc:
            raise CapsuleConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise CapsuleRequestTimeoutError(
                f"Timed out downloading bytes from {url}",
                request_id=request_id,
                timeout=timeout_value,
                operation="download",
            ) from exc

    async def post_bytes(
        self,
        url: str,
        *,
        data: bytes,
        base_url: str | None = None,
        params: dict[str, str] | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        request_id = request_id or str(uuid.uuid4())
        headers = {"X-Request-Id": request_id, "Content-Type": "application/octet-stream"}
        timeout_value = self._resolve_timeout(timeout, self._config.operation_timeout)

        try:
            async with self._client_for(base_url, timeout_value) as client:
                resp = await client.post(url, content=data, params=params, headers=headers)
                if resp.status_code >= 400:
                    self._raise_for_status(resp, request_id)
                return self._decode_response_body(resp, request_id)
        except httpx.ConnectError as exc:
            raise CapsuleConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise CapsuleRequestTimeoutError(
                f"Timed out uploading bytes to {url}",
                request_id=request_id,
                timeout=timeout_value,
                operation="upload",
            ) from exc

    async def post_to_host(
        self,
        url: str,
        *,
        json_body: dict[str, Any] | None = None,
        base_url: str | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        request_id = request_id or str(uuid.uuid4())
        headers = {"X-Request-Id": request_id}
        timeout_value = self._resolve_timeout(timeout, self._config.operation_timeout)

        try:
            async with self._client_for(base_url, timeout_value) as client:
                resp = await client.post(url, json=json_body, headers=headers)
                if resp.status_code >= 400:
                    self._raise_for_status(resp, request_id)
                return self._decode_response_body(resp, request_id)
        except httpx.ConnectError as exc:
            raise CapsuleConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise CapsuleRequestTimeoutError(
                f"Timed out calling host operation {url}",
                request_id=request_id,
                timeout=timeout_value,
                operation="host_request",
            ) from exc

    async def post_stream_ndjson(
        self,
        url: str,
        *,
        json_body: dict[str, Any] | None = None,
        base_url: str | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
    ) -> AsyncIterator[dict[str, Any]]:
        request_id = request_id or str(uuid.uuid4())
        headers = {"X-Request-Id": request_id, "Accept": "application/x-ndjson"}
        timeout_value = self._resolve_timeout(timeout, self._config.operation_timeout)

        try:
            async with self._client_for(base_url, timeout_value) as client:
                async with client.stream("POST", url, json=json_body, headers=headers) as resp:
                    if resp.status_code >= 400:
                        await resp.aread()
                        self._raise_for_status(resp, request_id)
                    async for line in resp.aiter_lines():
                        line = line.strip()
                        if not line:
                            continue
                        try:
                            yield json.loads(line)
                        except json.JSONDecodeError:
                            continue
        except httpx.ConnectError as exc:
            raise CapsuleConnectionError(str(exc)) from exc
        except httpx.TimeoutException as exc:
            raise CapsuleRequestTimeoutError(
                f"Timed out streaming {url}",
                request_id=request_id,
                timeout=timeout_value,
                operation="stream",
            ) from exc

    async def _request(
        self,
        method: str,
        url: str,
        *,
        params: dict[str, str] | None = None,
        json_body: dict[str, Any] | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
        retry_policy: RetryPolicy | None = None,
    ) -> dict[str, Any]:
        request_id = request_id or str(uuid.uuid4())
        last_exc: Exception | None = None
        policy = retry_policy or RetryPolicy()
        timeout_value = self._resolve_timeout(timeout, self._config.request_timeout)

        for attempt in range(policy.max_retries + 1):
            try:
                resp = await self._client.request(
                    method,
                    url,
                    params=params,
                    json=json_body,
                    headers={"X-Request-Id": request_id},
                    timeout=self._timeout_config(timeout_value),
                )

                if resp.status_code < 400:
                    return self._decode_response_body(resp, request_id)

                if resp.status_code in policy.retry_status_codes and attempt < policy.max_retries:
                    retry_after = _parse_retry_after(resp)
                    logger.debug(
                        "Retrying %s %s after HTTP %s (attempt %s/%s, request_id=%s, retry_after=%s)",
                        method,
                        url,
                        resp.status_code,
                        attempt + 1,
                        policy.max_retries,
                        request_id,
                        retry_after,
                    )
                    await self._backoff(attempt, retry_after)
                    continue

                self._raise_for_status(resp, request_id)

            except (httpx.ConnectError, httpx.ReadError, httpx.WriteError) as exc:
                last_exc = exc
                if policy.retry_transport_errors and attempt < policy.max_retries:
                    logger.debug(
                        "Retrying %s %s after transport error %r (attempt %s/%s, request_id=%s)",
                        method,
                        url,
                        exc,
                        attempt + 1,
                        policy.max_retries,
                        request_id,
                    )
                    await self._backoff(attempt)
                    continue
                raise CapsuleConnectionError(str(exc)) from exc

            except httpx.TimeoutException as exc:
                last_exc = exc
                if policy.retry_timeouts and attempt < policy.max_retries:
                    logger.debug(
                        "Retrying %s %s after timeout %r (attempt %s/%s, request_id=%s)",
                        method,
                        url,
                        exc,
                        attempt + 1,
                        policy.max_retries,
                        request_id,
                    )
                    await self._backoff(attempt)
                    continue
                raise CapsuleRequestTimeoutError(
                    f"{method} {url} timed out",
                    request_id=request_id,
                    timeout=timeout_value,
                    operation="request",
                ) from exc

        if last_exc:
            raise CapsuleConnectionError(str(last_exc)) from last_exc
        raise CapsuleConnectionError("Max retries exceeded")  # pragma: no cover

    def _client_for(self, base_url: str | None, timeout: float) -> _AsyncClientContext:
        return _AsyncClientContext(self._client, base_url=base_url, timeout=self._timeout_config(timeout))

    def _raise_for_status(self, resp: httpx.Response, request_id: str) -> None:
        body: dict[str, Any] = {}
        try:
            raw = resp.json()
            if isinstance(raw, dict):
                body = cast(dict[str, Any], raw)
        except (json.JSONDecodeError, ValueError):
            pass

        message = str(body.get("error", resp.text))
        kwargs: dict[str, Any] = {"request_id": request_id}

        status = resp.status_code
        if status == 401:
            raise CapsuleAuthError(message, **kwargs)
        if status == 404:
            raise CapsuleNotFound(message, **kwargs)
        if status == 409:
            raise CapsuleConflict(message, **kwargs)
        if status == 429:
            raise CapsuleRateLimited(message, retry_after=_parse_retry_after(resp), **kwargs)
        if status == 503:
            raise CapsuleServiceUnavailable(message, retry_after=_parse_retry_after(resp), **kwargs)
        raise CapsuleHTTPError(status, message, **kwargs)

    @staticmethod
    async def _backoff(attempt: int, retry_after: float | None = None) -> None:
        if retry_after and retry_after > 0:
            await asyncio.sleep(retry_after)
        else:
            delay = _BASE_BACKOFF * (2**attempt) + random.uniform(0, 0.5)
            await asyncio.sleep(delay)

    @staticmethod
    def _timeout_config(timeout: float | None) -> httpx.Timeout:
        return httpx.Timeout(timeout)

    @staticmethod
    def _resolve_timeout(timeout: float | None, default: float) -> float:
        return default if timeout is None else timeout

    @staticmethod
    def _decode_response_body(resp: httpx.Response, request_id: str) -> dict[str, Any]:
        try:
            raw = resp.json()
            if isinstance(raw, dict):
                payload = cast(dict[str, Any], raw)
                if "request_id" not in payload:
                    payload["request_id"] = request_id
                return payload
        except (json.JSONDecodeError, ValueError):
            pass
        return {"_raw": resp.text, "request_id": request_id}


class _AsyncClientContext:
    def __init__(self, shared_client: httpx.AsyncClient, *, base_url: str | None, timeout: httpx.Timeout) -> None:
        self._shared_client = shared_client
        self._base_url = base_url
        self._timeout = timeout
        self._client: httpx.AsyncClient | None = None

    async def __aenter__(self) -> httpx.AsyncClient:
        if self._base_url:
            self._client = httpx.AsyncClient(
                base_url=self._base_url,
                headers=dict(self._shared_client.headers),
                timeout=self._timeout,
            )
            return self._client
        return self._shared_client

    async def __aexit__(self, *_: object) -> None:
        if self._client is not None:
            await self._client.aclose()


def _parse_retry_after(resp: httpx.Response) -> float | None:
    raw = resp.headers.get("Retry-After")
    if raw is None:
        return None
    try:
        return float(raw)
    except ValueError:
        return None
