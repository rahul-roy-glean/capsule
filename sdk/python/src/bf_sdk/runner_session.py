from __future__ import annotations

from collections.abc import Iterator
from typing import TYPE_CHECKING, Any

from bf_sdk._shell import ShellSession
from bf_sdk.models.runner import ConnectResult, ExecEvent, PauseResult

if TYPE_CHECKING:
    from bf_sdk.resources.runners import Runners


class RunnerSession:
    """High-level handle for an allocated runner.

    Wraps a ``runner_id`` and provides ergonomic methods for exec, shell,
    pause, resume, and release. Supports context-manager usage for automatic
    release on exit.

    Usage::

        with client.runners.from_template("my-workload", tag="stable") as r:
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
    ) -> None:
        self._runners = runners
        self._runner_id = runner_id
        self._session_id = session_id
        if host_address:
            self._runners.set_host_cache(runner_id, host_address)

    @property
    def runner_id(self) -> str:
        return self._runner_id

    @property
    def session_id(self) -> str | None:
        return self._session_id

    # -- Context manager -------------------------------------------------------

    def __enter__(self) -> RunnerSession:
        return self

    def __exit__(self, *_: object) -> None:
        self.release()

    # -- Lifecycle -------------------------------------------------------------

    def wait_ready(self, *, timeout: float = 120.0, poll_interval: float = 2.0) -> None:
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
        return self._runners.connect(self._runner_id)

    def release(self) -> bool:
        """Release (destroy) the runner."""
        return self._runners.release(self._runner_id)

    # -- Exec & Shell ----------------------------------------------------------

    def exec(
        self,
        *command: str,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
    ) -> Iterator[ExecEvent]:
        """Execute a command in the runner, streaming ndjson events.

        Accepts the command as positional args for ergonomics::

            r.exec("python", "-c", "print(42)")
        """
        return self._runners.exec(
            self._runner_id,
            list(command),
            env=env,
            working_dir=working_dir,
            timeout_seconds=timeout_seconds,
        )

    def exec_collect(
        self,
        *command: str,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
    ) -> tuple[str, int]:
        """Execute a command and collect all output. Returns (output, exit_code)."""
        output_parts: list[str] = []
        exit_code = -1
        for event in self.exec(*command, env=env, working_dir=working_dir, timeout_seconds=timeout_seconds):
            if event.type in ("stdout", "stderr") and event.data:
                output_parts.append(event.data)
            elif event.type == "exit" and event.code is not None:
                exit_code = event.code
        return "".join(output_parts), exit_code

    def shell(self, *, command: str | None = None, cols: int = 80, rows: int = 24) -> ShellSession:
        """Open a PTY shell session. Use as a context manager."""
        return self._runners.shell(self._runner_id, command=command, cols=cols, rows=rows)

    # -- Quarantine (debugging) ------------------------------------------------

    def quarantine(self, *, reason: str | None = None) -> dict[str, Any]:
        return self._runners.quarantine(self._runner_id, reason=reason)

    def unquarantine(self) -> dict[str, Any]:
        return self._runners.unquarantine(self._runner_id)
