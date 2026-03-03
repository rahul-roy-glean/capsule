from __future__ import annotations

import struct

import pytest

from bf_sdk._shell import MSG_EXIT, MSG_RESIZE, MSG_SIGNAL, MSG_STDIN, MSG_STDOUT, ShellSession


class TestShellProtocol:
    def test_constants(self) -> None:
        assert MSG_STDIN == 0x00
        assert MSG_STDOUT == 0x01
        assert MSG_RESIZE == 0x02
        assert MSG_EXIT == 0x03
        assert MSG_SIGNAL == 0x04

    def test_not_connected_raises(self) -> None:
        session = ShellSession("ws://localhost/pty")
        with pytest.raises(RuntimeError, match="not connected"):
            session.send("hello")

    def test_exit_code_initially_none(self) -> None:
        session = ShellSession("ws://localhost/pty")
        assert session.exit_code is None

    def test_context_manager_calls_close(self) -> None:
        session = ShellSession("ws://localhost/pty")
        # Just test the __exit__ path doesn't error when not connected
        session.close()

    def test_send_builds_stdin_frame(self) -> None:
        """Verify that send() constructs the correct binary frame."""
        frames_sent: list[bytes] = []

        class MockConn:
            def send(self, data: bytes) -> None:
                frames_sent.append(data)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        session.send("hello")
        assert len(frames_sent) == 1
        assert frames_sent[0] == bytes([MSG_STDIN]) + b"hello"

    def test_send_bytes(self) -> None:
        frames_sent: list[bytes] = []

        class MockConn:
            def send(self, data: bytes) -> None:
                frames_sent.append(data)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        session.send(b"\x1b[A")
        assert frames_sent[0] == bytes([MSG_STDIN]) + b"\x1b[A"

    def test_resize_builds_frame(self) -> None:
        frames_sent: list[bytes] = []

        class MockConn:
            def send(self, data: bytes) -> None:
                frames_sent.append(data)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        session.resize(120, 40)
        assert len(frames_sent) == 1
        frame = frames_sent[0]
        assert frame[0] == MSG_RESIZE
        cols, rows = struct.unpack(">HH", frame[1:5])
        assert cols == 120
        assert rows == 40

    def test_signal_builds_frame(self) -> None:
        frames_sent: list[bytes] = []

        class MockConn:
            def send(self, data: bytes) -> None:
                frames_sent.append(data)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        session.send_signal(15)  # SIGTERM
        assert frames_sent[0] == bytes([MSG_SIGNAL, 15])

    def test_recv_stdout(self) -> None:
        recv_data = [bytes([MSG_STDOUT]) + b"output data"]

        class MockConn:
            def recv(self, timeout: float | None = None) -> bytes:
                return recv_data.pop(0)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        msg_type, payload = session.recv()
        assert msg_type == MSG_STDOUT
        assert payload == b"output data"

    def test_recv_exit(self) -> None:
        exit_frame = bytes([MSG_EXIT]) + struct.pack(">i", 42)
        recv_data = [exit_frame]

        class MockConn:
            def recv(self, timeout: float | None = None) -> bytes:
                return recv_data.pop(0)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        msg_type, payload = session.recv()
        assert msg_type == MSG_EXIT
        assert session.exit_code == 42

    def test_recv_stdout_helper(self) -> None:
        recv_data = [
            bytes([MSG_STDOUT]) + b"line 1",
        ]

        class MockConn:
            def recv(self, timeout: float | None = None) -> bytes:
                return recv_data.pop(0)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        data = session.recv_stdout()
        assert data == b"line 1"

    def test_recv_stdout_skips_to_exit(self) -> None:
        exit_frame = bytes([MSG_EXIT]) + struct.pack(">i", 0)
        recv_data = [exit_frame]

        class MockConn:
            def recv(self, timeout: float | None = None) -> bytes:
                return recv_data.pop(0)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        data = session.recv_stdout()
        assert data == b""
        assert session.exit_code == 0

    def test_wait_exit(self) -> None:
        exit_frame = bytes([MSG_EXIT]) + struct.pack(">i", 7)
        recv_data = [
            bytes([MSG_STDOUT]) + b"output",
            exit_frame,
        ]

        class MockConn:
            def recv(self, timeout: float | None = None) -> bytes:
                return recv_data.pop(0)

            def close(self) -> None:
                pass

        session = ShellSession("ws://localhost/pty")
        session._conn = MockConn()  # type: ignore[assignment]
        code = session.wait_exit()
        assert code == 7
