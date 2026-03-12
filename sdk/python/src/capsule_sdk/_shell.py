from __future__ import annotations

import struct
from collections.abc import Callable
from typing import TYPE_CHECKING

import websockets.sync.client
from websockets.exceptions import WebSocketException

if TYPE_CHECKING:
    from websockets.sync.client import ClientConnection

# Binary frame protocol constants (must match capsule-thaw-agent/pty_handler.go)
MSG_STDIN = 0x00
MSG_STDOUT = 0x01
MSG_RESIZE = 0x02
MSG_EXIT = 0x03
MSG_SIGNAL = 0x04


class ShellSession:
    """WebSocket PTY session using the binary frame protocol."""

    def __init__(
        self,
        ws_url: str,
        *,
        reconnect_url_factory: Callable[[], str] | None = None,
        connect_timeout: float | None = None,
    ) -> None:
        self._url = ws_url
        self._reconnect_url_factory = reconnect_url_factory
        self._connect_timeout = connect_timeout
        self._conn: ClientConnection | None = None
        self._exit_code: int | None = None

    def connect(self) -> None:
        try:
            self._conn = self._connect(self._url)
        except (OSError, WebSocketException):
            if self._reconnect_url_factory is None:
                raise
            self._url = self._reconnect_url_factory()
            self._conn = self._connect(self._url)

    def close(self) -> None:
        if self._conn:
            self._conn.close()
            self._conn = None

    def __enter__(self) -> ShellSession:
        self.connect()
        return self

    def __exit__(self, *_: object) -> None:
        self.close()

    def send(self, data: bytes | str) -> None:
        """Send stdin data to the PTY."""
        if isinstance(data, str):
            data = data.encode()
        frame = bytes([MSG_STDIN]) + data
        self._ensure_connected().send(frame)

    def recv(self, timeout: float | None = None) -> tuple[int, bytes]:
        """Receive a frame. Returns (msg_type, payload)."""
        conn = self._ensure_connected()
        raw = conn.recv(timeout=timeout)
        if isinstance(raw, str):
            raw = raw.encode()
        if len(raw) == 0:
            return (MSG_STDOUT, b"")
        msg_type = raw[0]
        payload = raw[1:]

        if msg_type == MSG_EXIT and len(payload) >= 4:
            self._exit_code = struct.unpack(">i", payload[:4])[0]

        return (msg_type, payload)

    def recv_stdout(self, timeout: float | None = None) -> bytes:
        """Receive until a stdout frame and return its data. Handles exit frames."""
        while True:
            msg_type, payload = self.recv(timeout=timeout)
            if msg_type == MSG_STDOUT:
                return payload
            if msg_type == MSG_EXIT:
                return b""

    def resize(self, cols: int, rows: int) -> None:
        """Send a resize frame."""
        frame = bytes([MSG_RESIZE]) + struct.pack(">HH", cols, rows)
        self._ensure_connected().send(frame)

    def send_signal(self, signal: int) -> None:
        """Send a signal to the PTY process."""
        frame = bytes([MSG_SIGNAL, signal])
        self._ensure_connected().send(frame)

    def wait_exit(self, timeout: float | None = None) -> int:
        """Block until the exit frame is received. Returns exit code."""
        while self._exit_code is None:
            msg_type, payload = self.recv(timeout=timeout)
            if msg_type == MSG_EXIT and len(payload) >= 4:
                self._exit_code = struct.unpack(">i", payload[:4])[0]
        return self._exit_code

    @property
    def exit_code(self) -> int | None:
        return self._exit_code

    def _ensure_connected(self) -> ClientConnection:
        if self._conn is None:
            raise RuntimeError("ShellSession is not connected. Call connect() or use as context manager.")
        return self._conn

    def _connect(self, url: str) -> ClientConnection:
        return websockets.sync.client.connect(url, open_timeout=self._connect_timeout)
