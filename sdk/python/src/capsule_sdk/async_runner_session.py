from __future__ import annotations

import inspect
import time
from collections.abc import AsyncIterator, Callable
from contextlib import suppress
from typing import TYPE_CHECKING, Any

from capsule_sdk._shell_async import AsyncShellSession
from capsule_sdk.models.file import (
    FileListResult,
    FileMkdirResult,
    FileReadResult,
    FileRemoveResult,
    FileStatResult,
    FileUploadResult,
    FileWriteResult,
)
from capsule_sdk.models.runner import ConnectResult, ExecEvent, ExecResult, PauseResult

if TYPE_CHECKING:
    from capsule_sdk.resources.async_runners import AsyncRunners


class AsyncRunnerSession:
    """High-level async handle for an allocated runner."""

    def __init__(
        self,
        runners: AsyncRunners,
        runner_id: str,
        *,
        host_address: str | None = None,
        session_id: str | None = None,
        request_id: str | None = None,
    ) -> None:
        self._runners = runners
        self._runner_id = runner_id
        self._session_id = session_id
        self._request_id = request_id
        self._released = False
        if host_address:
            self._runners.set_host_cache(runner_id, host_address)

    @property
    def runner_id(self) -> str:
        return self._runner_id

    @property
    def session_id(self) -> str | None:
        return self._session_id

    @property
    def request_id(self) -> str | None:
        return self._request_id

    async def __aenter__(self) -> AsyncRunnerSession:
        return self

    async def __aexit__(self, exc_type: object, exc: object, tb: object) -> None:
        if exc_type is not None:
            with suppress(Exception):
                await self.release()
            return
        await self.release()

    async def wait_ready(self, *, timeout: float | None = None, poll_interval: float = 2.0) -> None:
        await self._runners.wait_ready(self._runner_id, timeout=timeout, poll_interval=poll_interval)

    async def pause(self) -> PauseResult:
        result = await self._runners.pause(self._runner_id)
        if result.session_id:
            self._session_id = result.session_id
        return result

    async def resume(self) -> ConnectResult:
        previous_runner_id = self._runner_id
        result = await self._runners.connect(previous_runner_id)
        if result.runner_id != previous_runner_id:
            self._runner_id = result.runner_id
        return result

    async def fork(self) -> AsyncRunnerSession:
        result = await self._runners.fork(self._runner_id)
        return AsyncRunnerSession(
            self._runners,
            result.runner_id,
            host_address=result.host_address,
            session_id=result.session_id,
        )

    async def release(self) -> bool:
        if self._released:
            return True
        ok = await self._runners.release(self._runner_id)
        if ok:
            self._released = True
        return ok

    def exec(
        self,
        *command: str,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
        on_stdout: Callable[[ExecEvent], Any] | None = None,
        on_stderr: Callable[[ExecEvent], Any] | None = None,
        on_exit: Callable[[int], Any] | None = None,
    ) -> AsyncIterator[ExecEvent]:
        events = self._runners.exec(
            self._runner_id,
            list(command),
            env=env,
            working_dir=working_dir,
            timeout_seconds=timeout_seconds,
        )
        if on_stdout or on_stderr or on_exit:
            return self._iter_with_callbacks(events, on_stdout, on_stderr, on_exit)
        return events

    async def exec_collect(
        self,
        *command: str,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
    ) -> ExecResult:
        stdout_parts: list[str] = []
        stderr_parts: list[str] = []
        exit_code = -1
        t0 = time.monotonic()
        async for event in self.exec(*command, env=env, working_dir=working_dir, timeout_seconds=timeout_seconds):
            if event.type == "stdout" and event.data:
                stdout_parts.append(event.data)
            elif event.type == "stderr" and event.data:
                stderr_parts.append(event.data)
            elif event.type == "exit" and event.code is not None:
                exit_code = event.code
        duration_ms = (time.monotonic() - t0) * 1000
        return ExecResult(
            stdout="".join(stdout_parts),
            stderr="".join(stderr_parts),
            exit_code=exit_code,
            duration_ms=round(duration_ms, 1),
        )

    def shell(self, *, command: str | None = None, cols: int = 80, rows: int = 24) -> AsyncShellSession:
        return self._runners.shell(self._runner_id, command=command, cols=cols, rows=rows)

    async def download(self, path: str) -> bytes:
        return await self._runners.file_download(self._runner_id, path)

    async def upload(
        self,
        path: str,
        data: bytes | str,
        *,
        mode: str = "overwrite",
        perm: str | None = None,
    ) -> FileUploadResult:
        if isinstance(data, str):
            data = data.encode("utf-8")
        return await self._runners.file_upload(self._runner_id, path, data, mode=mode, perm=perm)

    async def read_file(self, path: str, *, offset: int = 0, limit: int | None = None) -> FileReadResult:
        return await self._runners.file_read(self._runner_id, path, offset=offset, limit=limit)

    async def read_text(self, path: str, *, offset: int = 0, limit: int | None = None) -> str:
        result = await self.read_file(path, offset=offset, limit=limit)
        return result.content or ""

    async def write_file(self, path: str, content: str, *, mode: str = "overwrite") -> FileWriteResult:
        return await self._runners.file_write(self._runner_id, path, content, mode=mode)

    async def write_text(self, path: str, content: str, *, mode: str = "overwrite") -> FileWriteResult:
        return await self.write_file(path, content, mode=mode)

    async def list_files(self, path: str, *, recursive: bool = False) -> FileListResult:
        return await self._runners.file_list(self._runner_id, path, recursive=recursive)

    async def stat_file(self, path: str) -> FileStatResult:
        return await self._runners.file_stat(self._runner_id, path)

    async def remove(self, path: str, *, recursive: bool = False) -> FileRemoveResult:
        return await self._runners.file_remove(self._runner_id, path, recursive=recursive)

    async def mkdir(self, path: str) -> FileMkdirResult:
        return await self._runners.file_mkdir(self._runner_id, path)

    async def quarantine(self, *, reason: str | None = None) -> dict[str, Any]:
        return await self._runners.quarantine(self._runner_id, reason=reason)

    async def unquarantine(self) -> dict[str, Any]:
        return await self._runners.unquarantine(self._runner_id)

    @staticmethod
    async def _iter_with_callbacks(
        events: AsyncIterator[ExecEvent],
        on_stdout: Callable[[ExecEvent], Any] | None,
        on_stderr: Callable[[ExecEvent], Any] | None,
        on_exit: Callable[[int], Any] | None,
    ) -> AsyncIterator[ExecEvent]:
        async for event in events:
            if event.type == "stdout" and on_stdout:
                await _maybe_await(on_stdout(event))
            elif event.type == "stderr" and on_stderr:
                await _maybe_await(on_stderr(event))
            elif event.type == "exit" and on_exit and event.code is not None:
                await _maybe_await(on_exit(event.code))
            yield event


async def _maybe_await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value
