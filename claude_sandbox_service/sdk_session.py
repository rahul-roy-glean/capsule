from __future__ import annotations

import asyncio
from collections.abc import Generator
from dataclasses import dataclass
from dataclasses import field
import logging
import os
from pathlib import Path
import queue
import threading
from typing import Any

from claude_agent_sdk import ClaudeAgentOptions
from claude_agent_sdk import ClaudeSDKClient

from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import Message
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import MessageType


class QueryAttemptError(RuntimeError):
    """Tracks whether a Claude query produced any response before failing."""

    def __init__(self, cause: Exception, *, response_started: bool) -> None:
        super().__init__(str(cause))
        self.cause = cause
        self.response_started = response_started


def _parse_setting_sources(raw_value: str | None) -> tuple[str, ...] | None:
    if raw_value is None:
        return ('user', 'project', 'local')
    parts = tuple(part.strip() for part in raw_value.split(',') if part.strip())
    return parts or None


def _load_optional_text_file(path_str: str | None) -> str | None:
    if not path_str:
        return None
    path = Path(path_str)
    if not path.exists():
        return None
    content = path.read_text(encoding='utf-8').strip()
    return content or None


@dataclass(frozen=True)
class SessionConfig:
    session_id: str
    cwd: Path
    model: str | None = None
    permission_mode: str = 'bypassPermissions'
    append_system_prompt: str | None = None
    resume: str | None = None
    fork_session: bool = False
    max_turns: int | None = None
    enable_file_checkpointing: bool = False
    include_partial_messages: bool = False
    mcp_config_path: str | None = None
    allowed_tools: tuple[str, ...] = field(default_factory=tuple)
    disallowed_tools: tuple[str, ...] = field(default_factory=tuple)
    setting_sources: tuple[str, ...] | None = None

    @classmethod
    def from_environment(
        cls,
        *,
        session_id: str,
        cwd: Path,
        model: str | None = None,
        permission_mode: str | None = None,
        append_system_prompt: str | None = None,
        resume: str | None = None,
        fork_session: bool = False,
        max_turns: int | None = None,
        enable_file_checkpointing: bool = False,
    ) -> SessionConfig:
        effective_model = model or os.getenv('CLAUDE_CODE_MODEL') or os.getenv('ANTHROPIC_MODEL')
        effective_permission_mode = permission_mode or os.getenv(
            'CLAUDE_PERMISSION_MODE',
            'bypassPermissions',
        )
        system_prompt_path = os.getenv('CLAUDE_SYSTEM_PROMPT_PATH', '/opt/claude-defaults/system-prompt.md')
        appended_prompt = append_system_prompt or _load_optional_text_file(system_prompt_path)
        mcp_config_path = os.getenv('CLAUDE_MCP_CONFIG_PATH', '/home/claude/.claude.json')
        if not Path(mcp_config_path).exists():
            mcp_config_path = None
        setting_sources = _parse_setting_sources(os.getenv('CLAUDE_SETTING_SOURCES'))

        return cls(
            session_id=session_id,
            cwd=cwd,
            model=effective_model,
            permission_mode=effective_permission_mode,
            append_system_prompt=appended_prompt,
            resume=resume,
            fork_session=fork_session,
            max_turns=max_turns,
            enable_file_checkpointing=enable_file_checkpointing,
            mcp_config_path=mcp_config_path,
            setting_sources=setting_sources,
        )

    def build_sdk_options(self) -> ClaudeAgentOptions:
        system_prompt: dict[str, Any] = {
            'type': 'preset',
            'preset': 'claude_code',
        }
        if self.append_system_prompt:
            system_prompt['append'] = self.append_system_prompt

        option_kwargs: dict[str, Any] = {
            'tools': {'type': 'preset', 'preset': 'claude_code'},
            'system_prompt': system_prompt,
            'permission_mode': self.permission_mode,
            'cwd': str(self.cwd),
            'enable_file_checkpointing': self.enable_file_checkpointing,
            'include_partial_messages': self.include_partial_messages,
        }
        if self.model:
            option_kwargs['model'] = self.model
        if self.resume:
            option_kwargs['resume'] = self.resume
            option_kwargs['fork_session'] = self.fork_session
        if self.max_turns is not None:
            option_kwargs['max_turns'] = self.max_turns
        if self.mcp_config_path:
            option_kwargs['mcp_servers'] = self.mcp_config_path
        if self.allowed_tools:
            option_kwargs['allowed_tools'] = list(self.allowed_tools)
        if self.disallowed_tools:
            option_kwargs['disallowed_tools'] = list(self.disallowed_tools)
        if self.setting_sources is not None:
            option_kwargs['setting_sources'] = list(self.setting_sources)
        return ClaudeAgentOptions(**option_kwargs)


class ClaudeSDKSession:
    """Owns a single Claude Agent SDK client on a dedicated event loop thread."""

    def __init__(self, *, config: SessionConfig, logger: logging.Logger) -> None:
        self._config = config
        self._logger = logger
        self._loop: asyncio.AbstractEventLoop | None = None
        self._loop_thread: threading.Thread | None = None
        self._loop_ready = threading.Event()
        self._state_lock = threading.Lock()
        self._query_lock = threading.Lock()
        self._client: ClaudeSDKClient | None = None
        self._sdk_session_id: str | None = None
        self._session_notice_sent = False
        self._last_error: str | None = None
        self._reconnect_count = 0

    @property
    def session_id(self) -> str:
        return self._config.session_id

    def stream_query(self, prompt: str, *, timeout_seconds: float | None = None) -> Generator[str, None, None]:
        if not self._query_lock.acquire(blocking=False):
            yield self._serialize_message(
                Message(
                    type=MessageType.ERROR_RESULT,
                    human_readable_message='**Session Busy**\n  • Another Claude request is already running.',
                )
            )
            return

        stream_queue: queue.Queue[Message] = queue.Queue()
        try:
            future = self._submit_coroutine(self._query_async(prompt, timeout_seconds, stream_queue))
            while True:
                try:
                    message = stream_queue.get(timeout=0.1)
                    yield self._serialize_message(message)
                except queue.Empty:
                    if future.done():
                        break

            while True:
                try:
                    message = stream_queue.get_nowait()
                    yield self._serialize_message(message)
                except queue.Empty:
                    break

            future.result()
        finally:
            self._query_lock.release()

    def interrupt(self) -> None:
        if not self._has_live_loop():
            return
        self._submit_coroutine(self._interrupt_async()).result()

    def reset(self) -> None:
        if not self._has_live_loop():
            self._session_notice_sent = False
            self._sdk_session_id = None
            return
        self._submit_coroutine(self._reset_async()).result()
        if self._loop is not None:
            self._loop.call_soon_threadsafe(self._loop.stop)
        if self._loop_thread is not None:
            self._loop_thread.join(timeout=1)
        self._loop = None
        self._loop_thread = None
        self._session_notice_sent = False
        self._sdk_session_id = None

    def info(self) -> dict[str, Any]:
        return {
            'session_id': self._config.session_id,
            'sdk_session_id': self._sdk_session_id,
            'model': self._config.model,
            'cwd': str(self._config.cwd),
            'permission_mode': self._config.permission_mode,
            'connected': self._client is not None,
            'busy': self._query_lock.locked(),
            'can_pause': not self._query_lock.locked(),
            'resume': self._config.resume,
            'last_error': self._last_error,
            'reconnect_count': self._reconnect_count,
        }

    async def _query_async(
        self,
        prompt: str,
        timeout_seconds: float | None,
        stream_queue: queue.Queue[Message],
    ) -> None:
        try:
            if timeout_seconds and timeout_seconds > 0:
                await asyncio.wait_for(
                    self._query_async_inner(prompt, stream_queue),
                    timeout=timeout_seconds,
                )
            else:
                await self._query_async_inner(prompt, stream_queue)
        except asyncio.TimeoutError:
            await self._interrupt_async()
            stream_queue.put(
                Message(
                    type=MessageType.ERROR_RESULT,
                    human_readable_message=f'**Request Timed Out**\n  • Claude was interrupted after {timeout_seconds}s.',
                )
            )
        except Exception as exc:  # pragma: no cover - defensive
            self._logger.exception('Claude SDK query failed for session %s', self._config.session_id)
            stream_queue.put(
                Message(
                    type=MessageType.ERROR_RESULT,
                    human_readable_message=f'**Claude SDK Error**\n  • {exc}',
                )
            )

    async def _query_async_inner(self, prompt: str, stream_queue: queue.Queue[Message]) -> None:
        had_existing_client = self._client is not None
        try:
            await self._query_once(prompt, stream_queue)
            self._last_error = None
        except QueryAttemptError as exc:
            self._last_error = str(exc.cause)
            if had_existing_client and not exc.response_started:
                self._logger.warning(
                    'Claude SDK client for session %s failed before response; reconnecting once',
                    self._config.session_id,
                )
                await self._drop_client()
                self._reconnect_count += 1
                try:
                    await self._query_once(prompt, stream_queue)
                    self._last_error = None
                    return
                except QueryAttemptError as retry_exc:
                    self._last_error = str(retry_exc.cause)
                    raise retry_exc.cause from retry_exc
            raise exc.cause from exc

    async def _query_once(self, prompt: str, stream_queue: queue.Queue[Message]) -> None:
        client = await self._ensure_client()
        if not self._session_notice_sent:
            stream_queue.put(
                Message.session_ready(
                    session_id=self._config.session_id,
                    model=self._config.model or 'default',
                    cwd=str(self._config.cwd),
                    permission_mode=self._config.permission_mode,
                )
            )
            self._session_notice_sent = True

        response_started = False
        try:
            await client.query(prompt, session_id=self._config.session_id)
            async for sdk_message in client.receive_response():
                response_started = True
                stream_queue.put(Message.from_sdk_message(sdk_message))

            server_info = await client.get_server_info()
            if isinstance(server_info, dict):
                session_id = server_info.get('session_id')
                if isinstance(session_id, str) and session_id:
                    self._sdk_session_id = session_id
        except Exception as exc:
            raise QueryAttemptError(exc, response_started=response_started) from exc

    async def _interrupt_async(self) -> None:
        client = self._client
        if client is None:
            return
        await client.interrupt()

    async def _reset_async(self) -> None:
        await self._drop_client()

    async def _drop_client(self) -> None:
        client = self._client
        if client is not None:
            await client.disconnect()
        self._client = None

    async def _ensure_client(self) -> ClaudeSDKClient:
        if self._client is not None:
            return self._client

        options = self._config.build_sdk_options()
        client = ClaudeSDKClient(options)
        await client.connect()
        self._client = client
        return client

    def _ensure_loop(self) -> None:
        with self._state_lock:
            if self._has_live_loop():
                return

            self._loop_ready.clear()

            def loop_target() -> None:
                loop = asyncio.new_event_loop()
                asyncio.set_event_loop(loop)
                self._loop = loop
                self._loop_ready.set()
                loop.run_forever()

            self._loop_thread = threading.Thread(
                target=loop_target,
                name=f'claude-sdk-session-{self._config.session_id}',
                daemon=True,
            )
            self._loop_thread.start()
            self._loop_ready.wait(timeout=5)

    def _submit_coroutine(self, coro: Any) -> Any:
        self._ensure_loop()
        if self._loop is None:  # pragma: no cover - defensive
            raise RuntimeError('Claude SDK loop failed to start')
        return asyncio.run_coroutine_threadsafe(coro, self._loop)

    def _has_live_loop(self) -> bool:
        return self._loop is not None and self._loop_thread is not None and self._loop_thread.is_alive()

    @staticmethod
    def _serialize_message(message: Message) -> str:
        return f'{message.model_dump_json()}\n'


class ClaudeSDKSessionRegistry:
    def __init__(self, *, logger: logging.Logger) -> None:
        self._logger = logger
        self._sessions: dict[str, ClaudeSDKSession] = {}
        self._lock = threading.Lock()

    def get_or_create(self, config: SessionConfig) -> ClaudeSDKSession:
        with self._lock:
            existing = self._sessions.get(config.session_id)
            if existing is not None:
                return existing
            session = ClaudeSDKSession(config=config, logger=self._logger)
            self._sessions[config.session_id] = session
            return session

    def get(self, session_id: str) -> ClaudeSDKSession | None:
        with self._lock:
            return self._sessions.get(session_id)

    def reset(self, session_id: str) -> bool:
        with self._lock:
            session = self._sessions.pop(session_id, None)
        if session is None:
            return False
        session.reset()
        return True

    def list_sessions(self) -> list[dict[str, Any]]:
        with self._lock:
            sessions = list(self._sessions.values())
        return [session.info() for session in sessions]
