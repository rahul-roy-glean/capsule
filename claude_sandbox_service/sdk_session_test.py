"""Unit tests for Claude SDK session runtime behavior."""

from __future__ import annotations

import asyncio
import logging
import queue
from pathlib import Path

from absl.testing import absltest

from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import Message
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import MessageType
from python_scio.platform.common.app.claude_sandbox_service.sdk_session import ClaudeSDKSession
from python_scio.platform.common.app.claude_sandbox_service.sdk_session import SessionConfig


class TextBlock:
    def __init__(self, text: str) -> None:
        self.text = text


class AssistantMessage:
    def __init__(self, text: str) -> None:
        self.content = [TextBlock(text)]


class _FailingClient:
    def __init__(self) -> None:
        self.disconnected = False

    async def query(self, prompt: str, session_id: str = "default") -> None:
        del prompt, session_id

    async def receive_response(self):
        if False:  # pragma: no cover - force async generator shape
            yield None
        raise BrokenPipeError("stale transport after resume")

    async def get_server_info(self):
        return {"session_id": "sdk-failing"}

    async def disconnect(self) -> None:
        self.disconnected = True


class _SuccessfulClient:
    def __init__(self) -> None:
        self.disconnected = False

    async def query(self, prompt: str, session_id: str = "default") -> None:
        del prompt, session_id

    async def receive_response(self):
        yield AssistantMessage("hello after reconnect")

    async def get_server_info(self):
        return {"session_id": "sdk-success"}

    async def disconnect(self) -> None:
        self.disconnected = True


class ClaudeSDKSessionTest(absltest.TestCase):
    def test_query_reconnects_once_for_stale_existing_client(self) -> None:
        session = ClaudeSDKSession(
            config=SessionConfig(
                session_id="capsule-session-123",
                cwd=Path("/workspace/repo"),
                model="claude-sonnet-test",
            ),
            logger=logging.getLogger("sdk-session-test"),
        )
        failing = _FailingClient()
        successful = _SuccessfulClient()
        session._client = failing

        async def fake_ensure_client():
            if session._client is None:
                session._client = successful
            return session._client

        session._ensure_client = fake_ensure_client  # type: ignore[method-assign]
        stream_queue: queue.Queue[Message] = queue.Queue()

        asyncio.run(session._query_async_inner("test prompt", stream_queue))

        messages = []
        while not stream_queue.empty():
            messages.append(stream_queue.get_nowait())

        self.assertTrue(failing.disconnected)
        self.assertEqual(session.info()["reconnect_count"], 1)
        self.assertEqual(session.info()["sdk_session_id"], "sdk-success")
        self.assertIsNone(session.info()["last_error"])
        self.assertGreaterEqual(len(messages), 2)
        self.assertEqual(messages[0].type, MessageType.SYSTEM_INIT)
        self.assertEqual(messages[1].type, MessageType.ASSISTANT)
        self.assertIn("hello after reconnect", messages[1].human_readable_message)


if __name__ == "__main__":
    absltest.main()
