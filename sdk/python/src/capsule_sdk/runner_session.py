from __future__ import annotations

import time
from collections.abc import Callable, Iterator
from contextlib import suppress
from typing import TYPE_CHECKING, Any

from capsule_sdk._shell import ShellSession
from capsule_sdk.models.file import (
    FileListResult,
    FileMkdirResult,
    FileReadResult,
    FileRemoveResult,
    FileStatResult,
    FileUploadResult,
    FileWriteResult,
)
from capsule_sdk.models.runner import CheckpointResult, ConnectResult, ExecEvent, ExecResult, PauseResult

if TYPE_CHECKING:
    from capsule_sdk.resources.runners import Runners


class RunnerSession:
    """High-level handle for an allocated runner.

    Wraps a ``runner_id`` and provides ergonomic methods for exec, shell,
    pause, resume, and release. Supports context-manager usage for automatic
    release on exit.

    Usage::

        with client.runners.from_config("my-workload", tag="stable") as r:
            r.exec("python", "-c", "print('hello')")
            with r.shell() as sh:
                sh.send("ls\\n")
                print(sh.recv_stdout().decode())
    """

    def __init__(
        self,
        runners: Runners,
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

    # -- Context manager -------------------------------------------------------

    def __enter__(self) -> RunnerSession:
        return self

    def __exit__(self, exc_type: object, exc: object, tb: object) -> None:
        if exc_type is not None:
            with suppress(Exception):
                self.release()
            return
        self.release()

    # -- Lifecycle -------------------------------------------------------------

    def wait_ready(self, *, timeout: float | None = None, poll_interval: float = 2.0) -> None:
        """Block until the runner is ready."""
        self._runners.wait_ready(self._runner_id, timeout=timeout, poll_interval=poll_interval)

    def pause(self) -> PauseResult:
        """Pause the runner and snapshot its session."""
        result = self._runners.pause(self._runner_id)
        if result.session_id:
            self._session_id = result.session_id
        return result

    def resume(self) -> ConnectResult:
        """Reconnect to the runner (extends TTL or resumes from suspend)."""
        previous_runner_id = self._runner_id
        result = self._runners.connect(previous_runner_id)
        if result.runner_id != previous_runner_id:
            self._runner_id = result.runner_id
        return result

    def release(self) -> bool:
        """Release (destroy) the runner."""
        if self._released:
            return True
        ok = self._runners.release(self._runner_id)
        if ok:
            self._released = True
        return ok

    # -- Exec & Shell ----------------------------------------------------------

    def exec(
        self,
        *command: str,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
        on_stdout: Callable[[ExecEvent], Any] | None = None,
        on_stderr: Callable[[ExecEvent], Any] | None = None,
        on_exit: Callable[[int], Any] | None = None,
    ) -> Iterator[ExecEvent]:
        """Execute a command in the runner, streaming ndjson events.

        Accepts the command as positional args for ergonomics::

            r.exec("python", "-c", "print(42)")

        Optional callbacks fire as events stream through; the iterator still
        yields all events regardless.
        """
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

    def exec_collect(
        self,
        *command: str,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
    ) -> ExecResult:
        """Execute a command and collect all output.

        Returns an ``ExecResult`` with structured stdout, stderr, exit_code, and
        duration_ms. Supports tuple unpacking for backwards compatibility::

            output, code = r.exec_collect("echo", "hello")
        """
        stdout_parts: list[str] = []
        stderr_parts: list[str] = []
        exit_code = -1
        t0 = time.monotonic()
        for event in self.exec(*command, env=env, working_dir=working_dir, timeout_seconds=timeout_seconds):
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

    def shell(self, *, command: str | None = None, cols: int = 80, rows: int = 24) -> ShellSession:
        """Open a PTY shell session. Use as a context manager."""
        return self._runners.shell(self._runner_id, command=command, cols=cols, rows=rows)

    # -- File operations -------------------------------------------------------

    def download(self, path: str) -> bytes:
        """Download a file from the runner as raw bytes."""
        return self._runners.file_download(self._runner_id, path)

    def upload(
        self,
        path: str,
        data: bytes | str,
        *,
        mode: str = "overwrite",
        perm: str | None = None,
    ) -> FileUploadResult:
        """Upload data to a file in the runner. Strings are encoded to UTF-8."""
        if isinstance(data, str):
            data = data.encode("utf-8")
        return self._runners.file_upload(self._runner_id, path, data, mode=mode, perm=perm)

    def read_file(self, path: str, *, offset: int = 0, limit: int | None = None) -> FileReadResult:
        """Read a file's content (JSON-based, supports offset/limit)."""
        return self._runners.file_read(self._runner_id, path, offset=offset, limit=limit)

    def read_text(self, path: str, *, offset: int = 0, limit: int | None = None) -> str:
        result = self.read_file(path, offset=offset, limit=limit)
        return result.content or ""

    def write_file(self, path: str, content: str, *, mode: str = "overwrite") -> FileWriteResult:
        """Write string content to a file in the runner."""
        return self._runners.file_write(self._runner_id, path, content, mode=mode)

    def write_text(self, path: str, content: str, *, mode: str = "overwrite") -> FileWriteResult:
        return self.write_file(path, content, mode=mode)

    def list_files(self, path: str, *, recursive: bool = False) -> FileListResult:
        """List files in a directory in the runner."""
        return self._runners.file_list(self._runner_id, path, recursive=recursive)

    def stat_file(self, path: str) -> FileStatResult:
        """Stat a file in the runner."""
        return self._runners.file_stat(self._runner_id, path)

    def remove(self, path: str, *, recursive: bool = False) -> FileRemoveResult:
        """Remove a file or directory in the runner."""
        return self._runners.file_remove(self._runner_id, path, recursive=recursive)

    def mkdir(self, path: str) -> FileMkdirResult:
        """Create a directory in the runner."""
        return self._runners.file_mkdir(self._runner_id, path)

    def checkpoint(self) -> CheckpointResult:
        """Non-destructively checkpoint the runner; VM keeps running."""
        return self._runners.checkpoint(self._runner_id)

    def service_logs(self) -> bytes:
        """Fetch the start_command service logs from the runner."""
        return self._runners.service_logs(self._runner_id)

    # -- Quarantine (debugging) ------------------------------------------------

    def quarantine(self, *, reason: str | None = None) -> dict[str, Any]:
        return self._runners.quarantine(self._runner_id, reason=reason)

    def unquarantine(self) -> dict[str, Any]:
        return self._runners.unquarantine(self._runner_id)

    # -- Private helpers -------------------------------------------------------

    @staticmethod
    def _iter_with_callbacks(
        events: Iterator[ExecEvent],
        on_stdout: Callable[[ExecEvent], Any] | None,
        on_stderr: Callable[[ExecEvent], Any] | None,
        on_exit: Callable[[int], Any] | None,
    ) -> Iterator[ExecEvent]:
        for event in events:
            if event.type == "stdout" and on_stdout:
                on_stdout(event)
            elif event.type == "stderr" and on_stderr:
                on_stderr(event)
            elif event.type == "exit" and on_exit and event.code is not None:
                on_exit(event.code)
            yield event
