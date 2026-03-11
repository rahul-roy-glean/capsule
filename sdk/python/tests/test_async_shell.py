from __future__ import annotations

import asyncio
import struct
from unittest.mock import AsyncMock, Mock, patch

import pytest

from bf_sdk._shell import MSG_EXIT, MSG_RESIZE, MSG_SIGNAL, MSG_STDIN, MSG_STDOUT
from bf_sdk._shell_async import AsyncShellSession


class TestAsyncShellProtocol:
    def test_not_connected_raises(self) -> None:
        session = AsyncShellSession("ws://localhost/pty")

        async def run() -> None:
            with pytest.raises(RuntimeError, match="not connected"):
                await session.send("hello")

        asyncio.run(run())

    def test_exit_code_initially_none(self) -> None:
        session = AsyncShellSession("ws://localhost/pty")
        assert session.exit_code is None

    def test_connect_retries_with_refreshed_url(self) -> None:
        conn = Mock()
        session = AsyncShellSession(
            "ws://stale/pty",
            reconnect_url_factory=AsyncMock(return_value="ws://fresh/pty"),
            connect_timeout=5.0,
        )

        async def run() -> None:
            with patch.object(session, "_connect", AsyncMock(side_effect=[OSError("stale"), conn])) as connect:
                await session.connect()
            assert session._conn is conn
            assert session._url == "ws://fresh/pty"
            assert connect.call_args_list[0].args[0] == "ws://stale/pty"
            assert connect.call_args_list[1].args[0] == "ws://fresh/pty"

        asyncio.run(run())

    def test_send_builds_stdin_frame(self) -> None:
        frames_sent: list[bytes] = []

        class MockConn:
            async def send(self, data: bytes) -> None:
                frames_sent.append(data)

            async def close(self) -> None:
                return None

        session = AsyncShellSession("ws://localhost/pty")
        session._conn = MockConn()

        async def run() -> None:
            await session.send("hello")

        asyncio.run(run())
        assert frames_sent == [bytes([MSG_STDIN]) + b"hello"]

    def test_resize_and_signal_build_frames(self) -> None:
        frames_sent: list[bytes] = []

        class MockConn:
            async def send(self, data: bytes) -> None:
                frames_sent.append(data)

            async def close(self) -> None:
                return None

        session = AsyncShellSession("ws://localhost/pty")
        session._conn = MockConn()

        async def run() -> None:
            await session.resize(120, 40)
            await session.send_signal(15)

        asyncio.run(run())
        resize_frame, signal_frame = frames_sent
        assert resize_frame[0] == MSG_RESIZE
        cols, rows = struct.unpack(">HH", resize_frame[1:5])
        assert cols == 120
        assert rows == 40
        assert signal_frame == bytes([MSG_SIGNAL, 15])

    def test_recv_and_wait_exit(self) -> None:
        exit_frame = bytes([MSG_EXIT]) + struct.pack(">i", 7)
        recv_data = [
            bytes([MSG_STDOUT]) + b"output",
            exit_frame,
        ]

        class MockConn:
            async def recv(self) -> bytes:
                return recv_data.pop(0)

            async def close(self) -> None:
                return None

        session = AsyncShellSession("ws://localhost/pty")
        session._conn = MockConn()

        async def run() -> None:
            msg_type, payload = await session.recv()
            assert msg_type == MSG_STDOUT
            assert payload == b"output"
            code = await session.wait_exit()
            assert code == 7

        asyncio.run(run())

    def test_recv_stdout_returns_empty_on_exit(self) -> None:
        exit_frame = bytes([MSG_EXIT]) + struct.pack(">i", 0)

        class MockConn:
            async def recv(self) -> bytes:
                return exit_frame

            async def close(self) -> None:
                return None

        session = AsyncShellSession("ws://localhost/pty")
        session._conn = MockConn()

        async def run() -> None:
            data = await session.recv_stdout()
            assert data == b""
            assert session.exit_code == 0

        asyncio.run(run())
