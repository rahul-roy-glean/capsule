from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock

from capsule_sdk.async_runner_session import AsyncRunnerSession
from capsule_sdk.models.runner import ConnectResult, ExecEvent, ExecResult, ForkSessionResponse, PauseResult
from capsule_sdk.resources.async_runners import AsyncRunners


def _iter_events(events: list[ExecEvent]):
    async def _gen():
        for event in events:
            yield event

    return _gen()


class TestAsyncRunnerSession:
    def test_properties(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        session = AsyncRunnerSession(
            runners,  # type: ignore[arg-type]
            "r-1",
            host_address="10.0.0.1:8080",
            session_id="s-1",
            request_id="req-1",
        )
        assert session.runner_id == "r-1"
        assert session.session_id == "s-1"
        assert session.request_id == "req-1"

    def test_context_manager_releases(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        session = AsyncRunnerSession(runners, "r-1")

        async def run() -> None:
            async with session:
                pass
            runners.release.assert_awaited_once_with("r-1")

        asyncio.run(run())

    def test_pause_and_resume(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        runners.pause.return_value = PauseResult(success=True, session_id="s-new")
        runners.connect.return_value = ConnectResult(status="resumed", runner_id="r-2", host_address="10.0.0.2:8080")
        session = AsyncRunnerSession(runners, "r-1")

        async def run() -> None:
            result = await session.pause()
            assert result.success is True
            assert session.session_id == "s-new"
            resumed = await session.resume()
            assert resumed.status == "resumed"
            assert session.runner_id == "r-2"

        asyncio.run(run())

    def test_fork(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        runners.fork.return_value = ForkSessionResponse(
            runner_id="r-fork",
            host_address="10.0.0.9:8080",
            session_id="s-fork",
        )
        session = AsyncRunnerSession(runners, "r-1")

        async def run() -> None:
            forked = await session.fork()
            runners.fork.assert_awaited_once_with("r-1")
            assert forked.runner_id == "r-fork"
            assert forked.session_id == "s-fork"

        asyncio.run(run())

    def test_exec_collect(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        events = [
            ExecEvent(type="stdout", data="hello\n"),
            ExecEvent(type="stderr", data="warn\n"),
            ExecEvent(type="exit", code=0),
        ]
        runners.exec.return_value = _iter_events(events)
        session = AsyncRunnerSession(runners, "r-1")

        async def run() -> None:
            result = await session.exec_collect("echo", "hello")
            assert isinstance(result, ExecResult)
            assert result.stdout == "hello\n"
            assert result.stderr == "warn\n"
            assert result.exit_code == 0
            assert result.duration_ms is not None

        asyncio.run(run())

    def test_exec_callbacks_support_async_and_sync(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        events = [
            ExecEvent(type="stdout", data="out1"),
            ExecEvent(type="stderr", data="err1"),
            ExecEvent(type="stdout", data="out2"),
            ExecEvent(type="exit", code=0),
        ]
        runners.exec.return_value = _iter_events(events)
        session = AsyncRunnerSession(runners, "r-1")
        stdout_calls: list[str] = []
        stderr_calls: list[str] = []
        exit_calls: list[int] = []

        async def on_stdout(event: ExecEvent) -> None:
            stdout_calls.append(event.data or "")

        async def run() -> None:
            collected = []
            async for event in session.exec(
                "test",
                on_stdout=on_stdout,
                on_stderr=lambda e: stderr_calls.append(e.data or ""),
                on_exit=lambda code: exit_calls.append(code),
            ):
                collected.append(event)

            assert stdout_calls == ["out1", "out2"]
            assert stderr_calls == ["err1"]
            assert exit_calls == [0]
            assert len(collected) == 4

        asyncio.run(run())

    def test_release_is_idempotent(self) -> None:
        runners = AsyncMock(spec=AsyncRunners)
        runners.set_host_cache = lambda *_: None  # type: ignore[method-assign]
        runners.release.return_value = True
        session = AsyncRunnerSession(runners, "r-1")

        async def run() -> None:
            assert await session.release() is True
            assert await session.release() is True
            runners.release.assert_awaited_once_with("r-1")

        asyncio.run(run())
