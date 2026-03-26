from __future__ import annotations

import asyncio
import logging
import time
import uuid
from collections.abc import AsyncIterator, Callable
from typing import TYPE_CHECKING, Any, cast
from urllib.parse import urlencode

from capsule_sdk._errors import (
    CapsuleAllocationTimeoutError,
    CapsuleConnectionError,
    CapsuleNotFound,
    CapsuleOperationTimeoutError,
    CapsuleRateLimited,
    CapsuleRequestTimeoutError,
    CapsuleRunnerUnavailableError,
    CapsuleServiceUnavailable,
)
from capsule_sdk._http_async import AsyncHttpClient, RetryPolicy
from capsule_sdk._shell_async import AsyncShellSession
from capsule_sdk.async_runner_session import AsyncRunnerSession
from capsule_sdk.models.file import (
    FileListResult,
    FileMkdirResult,
    FileReadResult,
    FileRemoveResult,
    FileStatResult,
    FileUploadResult,
    FileWriteResult,
)
from capsule_sdk.models.layered_config import CreateConfigResponse, LayeredConfigDetail, StoredLayeredConfig
from capsule_sdk.models.runner import (
    AllocateRunnerResponse,
    ExecEvent,
    PauseResult,
    Runner,
    RunnerListResponse,
    RunnerStatus,
)
from capsule_sdk.models.workload import ResolvedWorkloadRef, WorkloadSummary

if TYPE_CHECKING:
    from capsule_sdk.resources.async_layered_configs import AsyncLayeredConfigs
    from capsule_sdk.runner_config import RunnerConfig

_ALLOCATE_REQUEST_RETRY_POLICY = RetryPolicy(
    max_retries=2,
    retry_status_codes=frozenset({502, 504}),
    retry_transport_errors=True,
    retry_timeouts=True,
)
_HOST_READ_RETRY_ERRORS = (
    CapsuleConnectionError,
    CapsuleRequestTimeoutError,
    CapsuleServiceUnavailable,
)
_WAIT_RETRY_ERRORS = (
    CapsuleConnectionError,
    CapsuleNotFound,
    CapsuleRateLimited,
    CapsuleRequestTimeoutError,
    CapsuleServiceUnavailable,
)
_TERMINAL_RUNNER_STATUSES = {"terminated", "unavailable", "quarantined", "suspended", "paused"}
logger = logging.getLogger(__name__)


class AsyncRunners:
    """Async runner management: control plane + host agent operations."""

    def __init__(self, http: AsyncHttpClient, layered_configs: AsyncLayeredConfigs | None = None) -> None:
        self._http = http
        self._layered_configs = layered_configs
        self._host_cache: dict[str, str] = {}

    def set_host_cache(self, runner_id: str, host_address: str) -> None:
        self._host_cache[runner_id] = host_address

    async def allocate(
        self,
        workload: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
            | ResolvedWorkloadRef
        ),
        *,
        request_id: str | None = None,
        labels: dict[str, str] | None = None,
        session_id: str | None = None,
        network_policy_preset: str | None = None,
        network_policy_json: str | None = None,
        startup_timeout: float | None = None,
        retry_poll_interval: float = 1.0,
    ) -> AllocateRunnerResponse:
        workload_ref = await self._resolve_workload_ref(workload)
        workload_key = workload_ref.workload_key
        if not workload_key:
            raise CapsuleNotFound("Could not resolve workload key for runner allocation.")
        stable_request_id = request_id or str(uuid.uuid4())
        body: dict[str, Any] = {
            "workload_key": workload_key,
            "request_id": stable_request_id,
        }
        if labels:
            body["labels"] = labels
        if session_id:
            body["session_id"] = session_id
        if network_policy_preset:
            body["network_policy_preset"] = network_policy_preset
        if network_policy_json:
            body["network_policy_json"] = network_policy_json

        budget = self._resolve_startup_timeout(startup_timeout)
        deadline = time.monotonic() + budget
        attempt = 0
        last_error: Exception | None = None

        while True:
            try:
                data = await self._http.post(
                    "/api/v1/runners/allocate",
                    json_body=body,
                    request_id=stable_request_id,
                    retry_policy=_ALLOCATE_REQUEST_RETRY_POLICY,
                )
                resp = AllocateRunnerResponse.model_validate(data)
                if resp.host_address:
                    self._host_cache[resp.runner_id] = resp.host_address
                return resp
            except (
                CapsuleRateLimited,
                CapsuleServiceUnavailable,
                CapsuleConnectionError,
                CapsuleRequestTimeoutError,
            ) as exc:
                last_error = exc
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                delay = self._retry_delay(exc, attempt, retry_poll_interval)
                logger.debug(
                    "Retrying runner allocation for %s after %r (attempt=%s, request_id=%s, delay=%.2fs)",
                    workload_key,
                    exc,
                    attempt + 1,
                    stable_request_id,
                    min(delay, remaining),
                )
                await asyncio.sleep(min(delay, remaining))
                attempt += 1

        detail = f" Last error: {last_error}" if last_error else ""
        raise CapsuleAllocationTimeoutError(
            f"Timed out allocating runner for workload {workload_key!r}.{detail}",
            workload_key=workload_key,
            request_id=stable_request_id,
            timeout=budget,
        )

    async def status(self, runner_id: str) -> RunnerStatus:
        data = await self._http.get("/api/v1/runners/status", params={"runner_id": runner_id})
        result = RunnerStatus.model_validate(data)
        if result.host_address:
            self._host_cache[result.runner_id] = result.host_address
        return result

    async def list(self) -> list[Runner]:
        data = await self._http.get("/api/v1/runners")
        return RunnerListResponse.model_validate(data).runners

    async def release(self, runner_id: str) -> bool:
        data = await self._http.post("/api/v1/runners/release", json_body={"runner_id": runner_id})
        self._host_cache.pop(runner_id, None)
        return data.get("success", False)  # type: ignore[no-any-return]

    async def pause(self, runner_id: str, *, sync_fs: bool = False) -> PauseResult:
        body: dict[str, Any] = {"runner_id": runner_id}
        if sync_fs:
            body["sync_fs"] = True
        data = await self._http.post("/api/v1/runners/pause", json_body=body)
        return PauseResult.model_validate(data)

    async def quarantine(
        self,
        runner_id: str,
        *,
        reason: str | None = None,
        block_egress: bool = True,
        pause_vm: bool = True,
    ) -> dict[str, Any]:
        params: dict[str, str] = {"runner_id": runner_id}
        if reason:
            params["reason"] = reason
        params["block_egress"] = str(block_egress).lower()
        params["pause_vm"] = str(pause_vm).lower()
        return await self._http.post("/api/v1/runners/quarantine?" + urlencode(params))

    async def unquarantine(
        self,
        runner_id: str,
        *,
        unblock_egress: bool = True,
        resume_vm: bool = True,
    ) -> dict[str, Any]:
        params: dict[str, str] = {
            "runner_id": runner_id,
            "unblock_egress": str(unblock_egress).lower(),
            "resume_vm": str(resume_vm).lower(),
        }
        return await self._http.post("/api/v1/runners/unquarantine?" + urlencode(params))

    async def wait_ready(
        self,
        runner_id: str,
        *,
        timeout: float | None = None,
        poll_interval: float = 2.0,
    ) -> RunnerStatus:
        budget = self._resolve_startup_timeout(timeout)
        deadline = time.monotonic() + budget
        attempt = 0
        last_error: Exception | None = None

        while time.monotonic() < deadline:
            try:
                result = await self.status(runner_id)
            except _WAIT_RETRY_ERRORS as exc:
                last_error = exc
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                delay = min(self._retry_delay(exc, attempt, poll_interval), remaining)
                logger.debug(
                    "Retrying wait_ready for runner %s after %r (attempt=%s, delay=%.2fs)",
                    runner_id,
                    exc,
                    attempt + 1,
                    delay,
                )
                await asyncio.sleep(delay)
                attempt += 1
                continue

            if result.status == "ready":
                return result
            if result.error:
                raise CapsuleRunnerUnavailableError(
                    result.error,
                    runner_id=runner_id,
                    status=result.status,
                )
            if result.status in _TERMINAL_RUNNER_STATUSES:
                raise CapsuleRunnerUnavailableError(
                    f"Runner {runner_id} entered terminal state {result.status!r}",
                    runner_id=runner_id,
                    status=result.status,
                )
            await asyncio.sleep(poll_interval)
        detail = f" Last error: {last_error}" if last_error else ""
        raise CapsuleOperationTimeoutError(
            f"Runner {runner_id} did not become ready within {budget}s.{detail}",
            runner_id=runner_id,
            timeout=budget,
            operation="wait_ready",
        )

    async def allocate_ready(
        self,
        workload: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
            | ResolvedWorkloadRef
        ),
        *,
        request_id: str | None = None,
        labels: dict[str, str] | None = None,
        session_id: str | None = None,
        network_policy_preset: str | None = None,
        network_policy_json: str | None = None,
        startup_timeout: float | None = None,
        poll_interval: float = 2.0,
    ) -> AsyncRunnerSession:
        workload_ref = await self._resolve_workload_ref(workload)
        workload_key = workload_ref.workload_key
        if not workload_key:
            raise CapsuleNotFound("Could not resolve workload key for runner allocation.")
        budget = self._resolve_startup_timeout(startup_timeout)
        deadline = time.monotonic() + budget
        alloc = await self.allocate(
            workload_key,
            request_id=request_id,
            labels=labels,
            session_id=session_id,
            network_policy_preset=network_policy_preset,
            network_policy_json=network_policy_json,
            startup_timeout=max(deadline - time.monotonic(), 0.0),
            retry_poll_interval=min(1.0, poll_interval),
        )
        session = AsyncRunnerSession(
            self,
            alloc.runner_id,
            host_address=alloc.host_address,
            session_id=alloc.session_id,
            request_id=alloc.request_id,
        )
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise CapsuleAllocationTimeoutError(
                f"Timed out before runner {alloc.runner_id} became ready.",
                workload_key=workload_key,
                request_id=alloc.request_id,
                timeout=budget,
            )
        try:
            await session.wait_ready(timeout=remaining, poll_interval=poll_interval)
        except CapsuleOperationTimeoutError as exc:
            raise CapsuleAllocationTimeoutError(
                f"Timed out waiting for runner {alloc.runner_id} to become ready.",
                workload_key=workload_key,
                request_id=alloc.request_id,
                timeout=budget,
            ) from exc
        return session

    async def from_config(
        self,
        workload: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
            | ResolvedWorkloadRef
        ),
        *,
        request_id: str | None = None,
        labels: dict[str, str] | None = None,
        session_id: str | None = None,
        network_policy_preset: str | None = None,
        network_policy_json: str | None = None,
        startup_timeout: float | None = None,
        wait_ready: bool = True,
        poll_interval: float = 2.0,
    ) -> AsyncRunnerSession:
        if wait_ready:
            return await self.allocate_ready(
                workload,
                request_id=request_id,
                labels=labels,
                session_id=session_id,
                network_policy_preset=network_policy_preset,
                network_policy_json=network_policy_json,
                startup_timeout=startup_timeout,
                poll_interval=poll_interval,
            )

        alloc = await self.allocate(
            workload,
            request_id=request_id,
            labels=labels,
            session_id=session_id,
            network_policy_preset=network_policy_preset,
            network_policy_json=network_policy_json,
            startup_timeout=startup_timeout,
        )
        return AsyncRunnerSession(
            self,
            alloc.runner_id,
            host_address=alloc.host_address,
            session_id=alloc.session_id,
            request_id=alloc.request_id,
        )

    async def file_download(self, runner_id: str, path: str) -> bytes:
        return await self._with_host_read_retry(
            runner_id,
            lambda host: self._http.get_bytes(
                f"/api/v1/runners/{runner_id}/files/download",
                base_url=host,
                params={"path": path},
            ),
        )

    async def file_upload(
        self,
        runner_id: str,
        path: str,
        data: bytes,
        *,
        mode: str = "overwrite",
        perm: str | None = None,
    ) -> FileUploadResult:
        host = await self._resolve_host(runner_id)
        params: dict[str, str] = {"path": path, "mode": mode}
        if perm is not None:
            params["perm"] = perm
        return FileUploadResult.model_validate(
            await self._http.post_bytes(
                f"/api/v1/runners/{runner_id}/files/upload",
                data=data,
                base_url=host,
                params=params,
            )
        )

    async def file_read(
        self,
        runner_id: str,
        path: str,
        *,
        offset: int = 0,
        limit: int | None = None,
    ) -> FileReadResult:
        body: dict[str, Any] = {"path": path, "offset": offset}
        if limit is not None:
            body["limit"] = limit
        return FileReadResult.model_validate(
            await self._with_host_read_retry(
                runner_id,
                lambda host: self._http.post_to_host(
                    f"/api/v1/runners/{runner_id}/files/read",
                    json_body=body,
                    base_url=host,
                ),
            )
        )

    async def file_write(
        self,
        runner_id: str,
        path: str,
        content: str,
        *,
        mode: str = "overwrite",
    ) -> FileWriteResult:
        host = await self._resolve_host(runner_id)
        return FileWriteResult.model_validate(
            await self._http.post_to_host(
                f"/api/v1/runners/{runner_id}/files/write",
                json_body={"path": path, "content": content, "mode": mode},
                base_url=host,
            )
        )

    async def file_list(self, runner_id: str, path: str, *, recursive: bool = False) -> FileListResult:
        return FileListResult.model_validate(
            await self._with_host_read_retry(
                runner_id,
                lambda host: self._http.post_to_host(
                    f"/api/v1/runners/{runner_id}/files/list",
                    json_body={"path": path, "recursive": recursive},
                    base_url=host,
                ),
            )
        )

    async def file_stat(self, runner_id: str, path: str) -> FileStatResult:
        return FileStatResult.model_validate(
            await self._with_host_read_retry(
                runner_id,
                lambda host: self._http.post_to_host(
                    f"/api/v1/runners/{runner_id}/files/stat",
                    json_body={"path": path},
                    base_url=host,
                ),
            )
        )

    async def file_remove(self, runner_id: str, path: str, *, recursive: bool = False) -> FileRemoveResult:
        host = await self._resolve_host(runner_id)
        return FileRemoveResult.model_validate(
            await self._http.post_to_host(
                f"/api/v1/runners/{runner_id}/files/remove",
                json_body={"path": path, "recursive": recursive},
                base_url=host,
            )
        )

    async def file_mkdir(self, runner_id: str, path: str) -> FileMkdirResult:
        host = await self._resolve_host(runner_id)
        return FileMkdirResult.model_validate(
            await self._http.post_to_host(
                f"/api/v1/runners/{runner_id}/files/mkdir",
                json_body={"path": path},
                base_url=host,
            )
        )

    def shell(
        self,
        runner_id: str,
        *,
        command: str | None = None,
        cols: int = 80,
        rows: int = 24,
    ) -> AsyncShellSession:
        query: dict[str, int | str] = {"cols": cols, "rows": rows}
        if command:
            query["command"] = command
        return AsyncShellSession(
            connect_url_factory=lambda: self._build_shell_ws_url(runner_id, query),
            reconnect_url_factory=lambda: self._refresh_shell_ws_url(runner_id, query),
            connect_timeout=self._http.operation_timeout,
        )

    async def exec(
        self,
        runner_id: str,
        command: list[str],
        *,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
    ) -> AsyncIterator[ExecEvent]:
        body: dict[str, Any] = {"command": command}
        if env:
            body["env"] = env
        if working_dir:
            body["working_dir"] = working_dir
        if timeout_seconds:
            body["timeout_seconds"] = timeout_seconds

        async for event in self._exec_with_host_retry(runner_id, body):
            yield event

    @staticmethod
    def _ensure_scheme(addr: str) -> str:
        if not addr.startswith(("http://", "https://")):
            return f"http://{addr}"
        return addr

    async def _resolve_host(self, runner_id: str) -> str:
        if runner_id in self._host_cache:
            return self._ensure_scheme(self._host_cache[runner_id])
        result = await self.status(runner_id)
        if result.host_address:
            self._host_cache[result.runner_id] = result.host_address
            return self._ensure_scheme(result.host_address)
        raise CapsuleServiceUnavailable(f"No host address available for runner {runner_id}")

    async def _exec_with_host_retry(self, runner_id: str, body: dict[str, Any]) -> AsyncIterator[ExecEvent]:
        host = await self._resolve_host(runner_id)
        url = f"/api/v1/runners/{runner_id}/exec"

        received_any = False
        try:
            async for event_dict in self._http.post_stream_ndjson(url, json_body=body, base_url=host):
                received_any = True
                yield ExecEvent.model_validate(event_dict)
            return
        except CapsuleServiceUnavailable:
            if received_any:
                raise
            self._host_cache.pop(runner_id, None)
            new_host = await self._resolve_host(runner_id)
            async for event_dict in self._http.post_stream_ndjson(url, json_body=body, base_url=new_host):
                yield ExecEvent.model_validate(event_dict)

    def _resolve_startup_timeout(self, timeout: float | None) -> float:
        return self._http.startup_timeout if timeout is None else timeout

    async def _resolve_workload_ref(
        self,
        workload: (
            str
            | CreateConfigResponse
            | StoredLayeredConfig
            | LayeredConfigDetail
            | RunnerConfig
            | WorkloadSummary
            | ResolvedWorkloadRef
        ),
    ) -> ResolvedWorkloadRef:
        if isinstance(workload, ResolvedWorkloadRef):
            return workload

        if hasattr(workload, "leaf_workload_key"):
            value = cast(Any, workload).leaf_workload_key
            if isinstance(value, str) and value:
                return ResolvedWorkloadRef(
                    display_name=cast(Any, workload).display_name if hasattr(workload, "display_name") else None,
                    config_id=cast(Any, workload).config_id if hasattr(workload, "config_id") else None,
                    workload_key=value,
                )

        if self._layered_configs is None:
            if isinstance(workload, str):
                return ResolvedWorkloadRef(display_name=workload, workload_key=workload)
            raise CapsuleNotFound(
                "This runner client cannot resolve workload references "
                "without layered config support."
            )

        try:
            return await self._layered_configs.resolve_workload_ref(workload)
        except CapsuleNotFound:
            if isinstance(workload, str):
                return ResolvedWorkloadRef(display_name=workload, workload_key=workload)
            raise

    async def _with_host_read_retry(self, runner_id: str, op: Callable[[str], Any]) -> Any:
        host = await self._resolve_host(runner_id)
        try:
            result = op(host)
            if asyncio.isfuture(result) or hasattr(result, "__await__"):
                return await result
            return result
        except _HOST_READ_RETRY_ERRORS:
            self._host_cache.pop(runner_id, None)
            logger.debug("Retrying host read operation after refreshing host for runner %s", runner_id)
            retry_result = op(await self._resolve_host(runner_id))
            if asyncio.isfuture(retry_result) or hasattr(retry_result, "__await__"):
                return await retry_result
            return retry_result

    def _retry_delay(self, exc: Exception, attempt: int, poll_interval: float) -> float:
        retry_after = getattr(exc, "retry_after", None)
        if isinstance(retry_after, int | float) and retry_after > 0:
            return float(retry_after)
        return max(poll_interval, min(5.0, poll_interval * (2**attempt)))

    async def _build_shell_ws_url(self, runner_id: str, query: dict[str, int | str]) -> str:
        host = await self._resolve_host(runner_id)
        qs = urlencode(query)
        scheme = "wss" if host.startswith("https://") else "ws"
        host_addr = host.replace("https://", "").replace("http://", "")
        return f"{scheme}://{host_addr}/api/v1/runners/{runner_id}/pty?{qs}"

    async def _refresh_shell_ws_url(self, runner_id: str, query: dict[str, int | str]) -> str:
        self._host_cache.pop(runner_id, None)
        return await self._build_shell_ws_url(runner_id, query)
