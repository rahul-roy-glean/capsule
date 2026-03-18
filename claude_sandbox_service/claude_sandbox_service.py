#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
from collections.abc import Generator
from datetime import datetime
from enum import Enum
import glob
from http import HTTPStatus
import json
import logging
import os
from pathlib import Path
import subprocess
import threading
import time
from typing import Any

from flask import Flask
from flask import jsonify
from flask import request
from flask import Response
from flask import stream_with_context
from pydantic import BaseModel
from pydantic import Field

from python_scio.platform.common.app.claude_sandbox_service.anthropic_message import AnthropicArchivedMessage
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import Message
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import MessageType
from python_scio.platform.common.app.claude_sandbox_service.sdk_session import ClaudeSDKSessionRegistry
from python_scio.platform.common.app.claude_sandbox_service.sdk_session import SessionConfig

try:
    from datetime import UTC
except ImportError:  # pragma: no cover - fallback for older Python
    from datetime import timezone

    UTC = timezone.utc

app = Flask(__name__)
logger = logging.getLogger(__name__)
logging.basicConfig(level=logging.INFO, format='%(asctime)s %(levelname)s %(message)s')

REPO_DIR = Path(os.getenv('REPO_DIR', '/workspace/repo')).resolve()
PORT = 8080
PROMPT_TIMEOUT_SECONDS = 1800
DEFAULT_SESSION_ID = (
    os.getenv('CLAUDE_SANDBOX_DEFAULT_SESSION_ID')
    or os.getenv('CAPSULE_SESSION_ID')
    or os.getenv('SESSION_ID')
    or 'default'
)


class ExecuteStreamMode(str, Enum):
    FOREGROUND = 'foreground'
    BACKGROUND = 'background'


class ClaudeQueryRequest(BaseModel):
    prompt: str = Field(..., description='Prompt to send to Claude')
    timeout: int = Field(default=PROMPT_TIMEOUT_SECONDS, description='Timeout in seconds')
    session_id: str = Field(default=DEFAULT_SESSION_ID, description='Logical service session identifier')
    cwd: str | None = Field(default=None, description='Working directory relative to the repo root')
    model: str | None = Field(default=None, description='Optional Claude model override')
    permission_mode: str | None = Field(default=None, description='Optional SDK permission mode override')
    append_system_prompt: str | None = Field(default=None, description='Additional system prompt text to append')
    resume: str | None = Field(default=None, description='Optional Claude SDK session ID to resume')
    fork_session: bool = Field(default=False, description='Fork the resumed session into a new SDK session')
    max_turns: int | None = Field(default=None, description='Optional SDK max turns override')
    enable_file_checkpointing: bool = Field(
        default=False,
        description='Enable Claude SDK file checkpointing for rewind workflows',
    )


class ExecuteStreamRequest(ClaudeQueryRequest):
    execution_mode: ExecuteStreamMode = Field(
        default=ExecuteStreamMode.FOREGROUND,
        description='Whether to stream in the foreground or launch background execution',
    )


class SessionCreateRequest(BaseModel):
    session_id: str = Field(default=DEFAULT_SESSION_ID, description='Logical service session identifier')
    cwd: str | None = Field(default=None, description='Working directory relative to the repo root')
    model: str | None = Field(default=None, description='Optional Claude model override')
    permission_mode: str | None = Field(default=None, description='Optional SDK permission mode override')
    append_system_prompt: str | None = Field(default=None, description='Additional system prompt text to append')
    resume: str | None = Field(default=None, description='Optional Claude SDK session ID to resume')
    fork_session: bool = Field(default=False, description='Fork the resumed session into a new SDK session')
    max_turns: int | None = Field(default=None, description='Optional SDK max turns override')
    enable_file_checkpointing: bool = Field(
        default=False,
        description='Enable Claude SDK file checkpointing for rewind workflows',
    )


class ExecuteStreamResponseStatus(str, Enum):
    SUCCESS = 'success'
    ERROR = 'error'


class ExecuteStreamResponse(BaseModel):
    status: ExecuteStreamResponseStatus
    stderr: str = Field('', description='Standard error output')


class CostUsageRequest(BaseModel):
    start_time: datetime | None = Field(
        default=None,
        description='Start time for cost calculation (defaults to UTC timestamp 0 if not specified)',
    )
    end_time: datetime | None = Field(
        default=None,
        description='End time for cost calculation (no end time limit if not specified)',
    )


class CostUsageResponse(BaseModel):
    model: str = Field(..., description='Model name string')
    prompt_tokens: int = Field(..., description='Number of prompt tokens')
    response_tokens: int = Field(..., description='Number of response tokens')
    cache_creation_input_tokens: int = Field(..., description='Number of cache creation input tokens')
    cache_read_input_tokens: int = Field(..., description='Number of cache read input tokens')


class CostUsageListResponse(BaseModel):
    usage: list[CostUsageResponse] = Field(..., description='Cost usage per model')


def _resolve_sandbox_path(path_str: str) -> Path:
    sanitized = (path_str or '').strip()
    if sanitized.startswith('/'):
        sanitized = sanitized.lstrip('/')
    relative_path = Path(sanitized) if sanitized else Path('.')
    target = (REPO_DIR / relative_path).resolve()

    try:
        target.relative_to(REPO_DIR)
    except ValueError as exc:
        raise ValueError('Requested path is outside of sandbox root') from exc
    return target


def _resolve_request_cwd(cwd: str | None) -> Path:
    return _resolve_sandbox_path(cwd) if cwd else REPO_DIR


def _session_config_from_request(data: SessionCreateRequest | ClaudeQueryRequest) -> SessionConfig:
    return SessionConfig.from_environment(
        session_id=data.session_id,
        cwd=_resolve_request_cwd(data.cwd),
        model=data.model,
        permission_mode=data.permission_mode,
        append_system_prompt=data.append_system_prompt,
        resume=data.resume,
        fork_session=data.fork_session,
        max_turns=data.max_turns,
        enable_file_checkpointing=data.enable_file_checkpointing,
    )


def _iter_noop_stream() -> Generator[str, None, None]:
    yield Message(type=MessageType.GLEAN, human_readable_message='noop').model_dump_json() + '\n'


def _parse_execute_stream_request() -> ExecuteStreamRequest:
    json_body = request.get_json(silent=True)
    if json_body is not None:
        return ExecuteStreamRequest(**json_body)

    base64_request_body = request.data.decode('utf-8')
    request_body = base64.b64decode(base64_request_body).decode('utf-8')
    return ExecuteStreamRequest(**json.loads(request_body))


_sdk_registry = ClaudeSDKSessionRegistry(logger=logger)


@app.post('/update_proxy')
def update_proxy():
    data = request.get_json(silent=True)
    token = data.get('proxy_token') if data else None
    if not token:
        return jsonify({'error': 'proxy_token required'}), 400

    namespace = os.getenv('NAMESPACE', 'code-interpreter-namespace')
    proxy_url = f'http://{token}:x@claude-egress-proxy.{namespace}.svc.cluster.local:3128'

    current_proxy = os.environ.get('HTTP_PROXY', '')
    if current_proxy == proxy_url:
        logger.info('Proxy token unchanged, skipping update')
        return jsonify({'status': 'ok', 'changed': False}), 200

    for key in ('HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy'):
        os.environ[key] = proxy_url

    for git_key in ('http.proxy', 'https.proxy'):
        subprocess.run(
            ['git', 'config', '--global', git_key, proxy_url],
            check=False,
            capture_output=True,
        )

    logger.info('Proxy token updated')
    return jsonify({'status': 'ok', 'changed': True}), 200


@app.get('/health')
def health():
    return jsonify({'status': 'healthy'}), 200


@app.get('/info')
def info():
    return jsonify(
        {
            'repo_url': os.getenv('REPO_URL', ''),
            'agent_backend': 'claude_agent_sdk',
            'default_session_id': DEFAULT_SESSION_ID,
            'default_permission_mode': os.getenv('CLAUDE_PERMISSION_MODE', 'bypassPermissions'),
            'system_prompt_path': os.getenv('CLAUDE_SYSTEM_PROMPT_PATH', '/opt/claude-defaults/system-prompt.md'),
            'mcp_config_path': os.getenv('CLAUDE_MCP_CONFIG_PATH', '/home/claude/.claude.json'),
            'setting_sources': os.getenv('CLAUDE_SETTING_SOURCES', 'user,project,local'),
            'compatibility_routes': ['/execute', '/execute_stream', '/interrupt', '/reset'],
            'session_routes': [
                '/sessions',
                '/sessions/<session_id>',
                '/sessions/<session_id>/query',
                '/sessions/<session_id>/interrupt',
                '/sessions/<session_id>/reset',
            ],
        }
    ), 200


@app.get('/sessions')
def list_sessions():
    return jsonify({'sessions': _sdk_registry.list_sessions()}), 200


@app.post('/sessions')
def create_session():
    data: dict[str, Any] | None = request.get_json(silent=True)
    try:
        session_request = SessionCreateRequest(**(data or {}))
        session = _sdk_registry.get_or_create(_session_config_from_request(session_request))
    except Exception as exc:
        return jsonify({'error': f'Failed to create session: {exc}'}), 400
    return jsonify(session.info()), 200


@app.get('/sessions/<session_id>')
def get_session(session_id: str):
    session = _sdk_registry.get(session_id)
    if session is None:
        return jsonify({'error': 'Session not found'}), 404
    return jsonify(session.info()), 200


@app.post('/sessions/<session_id>/query')
def session_query(session_id: str):
    data: dict[str, Any] | None = request.get_json(silent=True)
    try:
        query_request = ClaudeQueryRequest(**{**(data or {}), 'session_id': session_id})
        session = _sdk_registry.get_or_create(_session_config_from_request(query_request))
    except Exception as exc:
        return jsonify({'error': f'Failed to parse request: {exc}'}), 400

    return Response(
        stream_with_context(session.stream_query(query_request.prompt, timeout_seconds=query_request.timeout)),
        status=HTTPStatus.OK,
        mimetype='text/event-stream',
        headers={
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
            'X-Accel-Buffering': 'no',
        },
    )


@app.post('/sessions/<session_id>/interrupt')
def session_interrupt(session_id: str):
    session = _sdk_registry.get(session_id)
    if session is None:
        return jsonify({'status': 'ok', 'detail': 'no active session'}), 200
    session.interrupt()
    return jsonify({'status': 'ok', 'session_id': session_id}), 200


@app.post('/sessions/<session_id>/reset')
def session_reset(session_id: str):
    removed = _sdk_registry.reset(session_id)
    detail = 'session reset' if removed else 'no active session'
    return jsonify({'status': 'ok', 'detail': detail, 'session_id': session_id}), 200


@app.post('/execute')
def execute():
    data: dict[str, Any] | None = request.get_json(silent=True)
    if not data or not data.get('prompt'):
        logger.warning('Invalid execute payload. raw=%s parsed=%s', request.data[:500], data)
        return jsonify({'error': 'Invalid request payload'}), 400

    query_request = ClaudeQueryRequest(**data)
    session = _sdk_registry.get_or_create(_session_config_from_request(query_request))
    start_time = time.time()
    messages: list[dict[str, Any]] = []
    for chunk in session.stream_query(query_request.prompt, timeout_seconds=query_request.timeout):
        message = Message(**json.loads(chunk))
        if not message.should_filter():
            messages.append(message.model_dump())

    status = 'success'
    if any(message['type'] == MessageType.ERROR_RESULT.value for message in messages):
        status = 'error'

    duration = int(time.time() - start_time)
    output = messages[-1]['human_readable_message'] if messages else ''
    return jsonify(
        {
            'status': status,
            'session_id': query_request.session_id,
            'output': output,
            'messages': messages,
            'stderr': '',
            'execution_time_seconds': duration,
        }
    ), 200


@app.route('/execute_stream', methods=['POST'])
def execute_stream():
    try:
        if os.getenv('NOOP'):
            return Response(
                stream_with_context(_iter_noop_stream()),
                status=HTTPStatus.OK,
                mimetype='text/event-stream',
                headers={
                    'Cache-Control': 'no-cache',
                    'Connection': 'keep-alive',
                    'X-Accel-Buffering': 'no',
                },
            )

        try:
            exec_request = _parse_execute_stream_request()
            session = _sdk_registry.get_or_create(_session_config_from_request(exec_request))
        except Exception as exc:
            logger.exception('failed to parse execute_stream request')
            return Response(
                response=ExecuteStreamResponse(
                    status=ExecuteStreamResponseStatus.ERROR,
                    stderr=f'Failed to parse request: {exc}',
                ).model_dump_json(),
                status=HTTPStatus.BAD_REQUEST,
                mimetype='application/json',
            )

        if exec_request.execution_mode == ExecuteStreamMode.BACKGROUND:

            def background_stream_execution() -> None:
                try:
                    for _ in session.stream_query(exec_request.prompt, timeout_seconds=exec_request.timeout):
                        pass
                    logger.info('Background SDK execution completed for session %s', exec_request.session_id)
                except Exception:
                    logger.exception('Error in background SDK execution')

            threading.Thread(target=background_stream_execution, daemon=True).start()
            return Response(
                response=ExecuteStreamResponse(
                    status=ExecuteStreamResponseStatus.SUCCESS,
                    stderr='Execution started in background',
                ).model_dump_json(),
                status=HTTPStatus.OK,
                mimetype='text/event-stream',
            )

        return Response(
            stream_with_context(session.stream_query(exec_request.prompt, timeout_seconds=exec_request.timeout)),
            status=HTTPStatus.OK,
            mimetype='text/event-stream',
            headers={
                'Cache-Control': 'no-cache',
                'Connection': 'keep-alive',
                'X-Accel-Buffering': 'no',
            },
        )
    except Exception as exc:  # pragma: no cover - defensive
        logger.exception('Error in stream endpoint')
        return Response(
            response=ExecuteStreamResponse(
                status=ExecuteStreamResponseStatus.ERROR,
                stderr=f'An internal error happened: {exc}\n\n',
            ).model_dump_json(),
            status=HTTPStatus.INTERNAL_SERVER_ERROR,
            mimetype='text/event-stream',
        )


@app.post('/reset')
def reset():
    data = request.get_json(silent=True) or {}
    session_id = data.get('session_id', DEFAULT_SESSION_ID)
    removed = _sdk_registry.reset(session_id)
    detail = 'session reset' if removed else 'no active session'
    return jsonify({'status': 'ok', 'detail': detail, 'session_id': session_id}), 200


@app.post('/interrupt')
def interrupt():
    data = request.get_json(silent=True) or {}
    session_id = data.get('session_id', DEFAULT_SESSION_ID)
    session = _sdk_registry.get(session_id)
    if session is None:
        return jsonify({'status': 'ok', 'detail': 'no active session', 'session_id': session_id}), 200
    session.interrupt()
    return jsonify({'status': 'ok', 'session_id': session_id}), 200


@app.get('/cost_usage')
def cost_usage():
    try:
        data: dict[str, Any] | None = request.get_json(silent=True)

        try:
            usage_request = CostUsageRequest(**data) if data is not None else CostUsageRequest()
        except Exception as exc:
            logger.exception('Failed to parse cost usage request')
            return jsonify({'error': f'Failed to parse request: {exc}'}), 400

        claude_home = os.getenv('CLAUDE_HOME')
        if not claude_home:
            return jsonify({'error': 'CLAUDE_HOME environment variable not set'}), 500

        projects_pattern = os.path.join(claude_home, 'projects', '*', '*.jsonl')
        jsonl_files = glob.glob(projects_pattern)
        if not jsonl_files:
            return jsonify({'error': 'No JSONL files found in CLAUDE_HOME/projects/'}), 400

        message_usage_by_id: dict[str, tuple[datetime, str, Any]] = {}
        start_filter = usage_request.start_time or datetime.fromtimestamp(0, tz=UTC)
        end_filter = usage_request.end_time or datetime.now(tz=UTC)

        for file_path in jsonl_files:
            try:
                with open(file_path, encoding='utf-8') as handle:
                    for line_num, line in enumerate(handle, 1):
                        line = line.strip()
                        if not line:
                            continue
                        try:
                            raw_data = json.loads(line)
                            entry = AnthropicArchivedMessage(**raw_data)
                            if not entry.message or not entry.message.usage or not entry.message.model:
                                continue
                            if not entry.timestamp:
                                continue
                            if entry.timestamp < start_filter or entry.timestamp > end_filter:
                                continue
                            if entry.message.model == '<synthetic>' or not entry.message.id:
                                continue

                            existing = message_usage_by_id.get(entry.message.id)
                            if existing is None or entry.timestamp >= existing[0]:
                                message_usage_by_id[entry.message.id] = (
                                    entry.timestamp,
                                    entry.message.model,
                                    entry.message.usage,
                                )
                        except json.JSONDecodeError as exc:
                            logger.warning('Invalid JSON in file %s line %d: %s', file_path, line_num, exc)
                        except Exception as exc:
                            logger.warning('Error processing line %d in file %s: %s', line_num, file_path, exc)
            except Exception as exc:
                logger.warning('Error reading file %s: %s', file_path, exc)

        model_usage: dict[str, dict[str, int]] = {}
        for _, model_name, usage in message_usage_by_id.values():
            if model_name not in model_usage:
                model_usage[model_name] = {
                    'prompt_tokens': 0,
                    'response_tokens': 0,
                    'cache_creation_input_tokens': 0,
                    'cache_read_input_tokens': 0,
                }
            model_usage[model_name]['prompt_tokens'] += usage.input_tokens
            model_usage[model_name]['response_tokens'] += usage.output_tokens
            model_usage[model_name]['cache_creation_input_tokens'] += usage.cache_creation_input_tokens
            model_usage[model_name]['cache_read_input_tokens'] += usage.cache_read_input_tokens

        usage_list = [
            CostUsageResponse(
                model=model_name,
                prompt_tokens=usage_data['prompt_tokens'],
                response_tokens=usage_data['response_tokens'],
                cache_creation_input_tokens=usage_data['cache_creation_input_tokens'],
                cache_read_input_tokens=usage_data['cache_read_input_tokens'],
            )
            for model_name, usage_data in model_usage.items()
        ]
        return jsonify(CostUsageListResponse(usage=usage_list).model_dump()), 200
    except Exception as exc:  # pragma: no cover - defensive
        logger.exception('Error in cost_usage endpoint')
        return jsonify({'error': f'Internal server error: {exc}'}), 500


def main() -> None:
    parser = argparse.ArgumentParser(description='Claude Code sandbox service')
    parser.add_argument('--execute', '-e', type=str, help='Execute Claude with the given prompt')
    parser.add_argument('--port', type=int, default=PORT, help='Port to run the service on')
    parser.add_argument('--host', type=str, default='0.0.0.0', help='Host to bind to')
    args = parser.parse_args()

    app.run(host=args.host, port=args.port)


if __name__ == '__main__':
    main()
