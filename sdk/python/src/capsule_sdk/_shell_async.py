from __future__ import annotations

import asyncio
import struct
from collections.abc import Awaitable, Callable
from typing import Any

import websockets
from websockets.exceptions import WebSocketException

from capsule_sdk._shell import MSG_EXIT, MSG_RESIZE, MSG_SIGNAL, MSG_STDIN, MSG_STDOUT


class AsyncShellSession:
    """Async WebSocket PTY session using the binary frame protocol."""

    def __init__(
        self,
        ws_url: str | None = None,
        *,
        connect_url_factory: Callable[[], Awaitable[str]] | None = None,
        reconnect_url_factory: Callable[[], Awaitable[str]] | None = None,
        connect_timeout: float | None = None,
    ) -> None:
        self._url = ws_url
        self._connect_url_factory = connect_url_factory
        self._reconnect_url_factory = reconnect_url_factory
        self._connect_timeout = connect_timeout
        self._conn: Any | None = None
        self._exit_code: int | None = None

    async def connect(self) -> None:
        try:
            self._url = await self._resolve_initial_url()
            self._conn = await self._connect(self._url)
        except (OSError, WebSocketException):
            if self._reconnect_url_factory is None:
                raise
            self._url = await self._reconnect_url_factory()
            self._conn = await self._connect(self._url)

    async def close(self) -> None:
        if self._conn:
            await self._conn.close()
            self._conn = None

    async def __aenter__(self) -> AsyncShellSession:
        await self.connect()
        return self

    async def __aexit__(self, *_: object) -> None:
        await self.close()

    async def send(self, data: bytes | str) -> None:
        """Send stdin data to the PTY."""
        if isinstance(data, str):
            data = data.encode()
        frame = bytes([MSG_STDIN]) + data
        await self._ensure_connected().send(frame)

    async def recv(self, timeout: float | None = None) -> tuple[int, bytes]:
        """Receive a frame. Returns (msg_type, payload)."""
        conn = self._ensure_connected()
        raw = await self._recv_with_timeout(conn, timeout)
        if isinstance(raw, str):
            raw = raw.encode()
        if len(raw) == 0:
            return (MSG_STDOUT, b"")
        msg_type = raw[0]
        payload = raw[1:]

        if msg_type == MSG_EXIT and len(payload) >= 4:
            self._exit_code = struct.unpack(">i", payload[:4])[0]

        return (msg_type, payload)

    async def recv_stdout(self, timeout: float | None = None) -> bytes:
        """Receive until a stdout frame and return its data. Handles exit frames."""
        while True:
            msg_type, payload = await self.recv(timeout=timeout)
            if msg_type == MSG_STDOUT:
                return payload
            if msg_type == MSG_EXIT:
                return b""

    async def resize(self, cols: int, rows: int) -> None:
        """Send a resize frame."""
        frame = bytes([MSG_RESIZE]) + struct.pack(">HH", cols, rows)
        await self._ensure_connected().send(frame)

    async def send_signal(self, signal: int) -> None:
        """Send a signal to the PTY process."""
        frame = bytes([MSG_SIGNAL, signal])
        await self._ensure_connected().send(frame)

    async def wait_exit(self, timeout: float | None = None) -> int:
        """Block until the exit frame is received. Returns exit code."""
        while self._exit_code is None:
            msg_type, payload = await self.recv(timeout=timeout)
            if msg_type == MSG_EXIT and len(payload) >= 4:
                self._exit_code = struct.unpack(">i", payload[:4])[0]
        return self._exit_code

    @property
    def exit_code(self) -> int | None:
        return self._exit_code

    def _ensure_connected(self) -> Any:
        if self._conn is None:
            raise RuntimeError("AsyncShellSession is not connected. Call connect() or use as context manager.")
        return self._conn

    async def _resolve_initial_url(self) -> str:
        if self._url is not None:
            return self._url
        if self._connect_url_factory is None:
            raise RuntimeError("No websocket URL available for AsyncShellSession.")
        return await self._connect_url_factory()

    async def _connect(self, url: str) -> Any:
        return await websockets.connect(url, open_timeout=self._connect_timeout)

    @staticmethod
    async def _recv_with_timeout(conn: Any, timeout: float | None) -> Any:
        if timeout is None:
            return await conn.recv()
        return await asyncio.wait_for(conn.recv(), timeout=timeout)
