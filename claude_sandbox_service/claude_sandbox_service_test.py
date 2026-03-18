"""Tests for Claude sandbox service."""

import base64
import json
import os
from pathlib import Path
import shutil
import tempfile
from unittest.mock import patch

from absl.testing import absltest
from parameterized import parameterized

from python_scio.platform.common.app.claude_sandbox_service import claude_sandbox_service as claude_service
from python_scio.platform.common.app.claude_sandbox_service import sdk_session
from python_scio.platform.common.app.claude_sandbox_service.claude_sandbox_service import app
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import ContentType
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import Message
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import MessageType


class _FakeSDKSession:
    """Simple fake Claude SDK session used by HTTP route tests."""

    def __init__(self, chunks: list[Message] | None = None):
        self._chunks = chunks or []
        self.interrupt_called = False

    def stream_query(self, prompt: str, *, timeout_seconds: float | None = None):
        del prompt, timeout_seconds
        for message in self._chunks:
            yield message.model_dump_json() + '\n'

    def interrupt(self):
        self.interrupt_called = True

    def reset(self):
        return None

    def info(self):
        return {
            'session_id': 'default',
            'sdk_session_id': 'sdk-default',
            'model': 'claude-sonnet-test',
            'cwd': '/repo',
            'permission_mode': 'bypassPermissions',
            'connected': True,
            'busy': False,
            'can_pause': True,
            'resume': None,
            'last_error': None,
            'reconnect_count': 0,
        }


class ClaudeSandboxServiceTest(absltest.TestCase):
    def setUp(self):
        """Set up test client."""
        app.config['TESTING'] = True
        self.client = app.test_client()
        self.repo_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.repo_dir.cleanup)
        self.repo_root = Path(self.repo_dir.name)
        claude_service.REPO_DIR = self.repo_root.resolve()
        claude_service._sdk_registry = sdk_session.ClaudeSDKSessionRegistry(logger=claude_service.logger)

    def test_health_endpoint(self):
        """Test health check endpoint."""
        response = self.client.get('/health')
        self.assertEqual(response.status_code, 200)
        data = json.loads(response.data)
        self.assertEqual(data['status'], 'healthy')

    @patch.dict('os.environ', {'REPO_URL': 'test-repo'})
    def test_info_endpoint(self):
        """Test info endpoint."""
        response = self.client.get('/info')
        self.assertEqual(response.status_code, 200)
        data = json.loads(response.data)
        self.assertEqual(data['repo_url'], 'test-repo')

    def test_execute_missing_prompt(self):
        """Test execute endpoint with missing prompt."""
        response = self.client.post('/execute', json={})
        self.assertEqual(response.status_code, 400)
        data = json.loads(response.data)
        self.assertIn('error', data)
        self.assertEqual(data['error'], 'Invalid request payload')

    @patch.object(claude_service, '_sdk_registry')
    def test_execute_success(self, mock_registry):
        """Test successful Claude SDK execution."""
        mock_registry.get_or_create.return_value = _FakeSDKSession(
            [
                Message(type=MessageType.ASSISTANT, human_readable_message='**Assistant**: abcd123xyz'),
                Message(
                    type=MessageType.SUCCESS_RESULT,
                    human_readable_message='**Task Completed Successfully**\n  • Result: abcd123xyz\n  • Duration: 2.5s',
                    content_type=ContentType.TEXT,
                ),
            ]
        )

        response = self.client.post('/execute', json={'prompt': 'test prompt', 'session_id': 'default'})
        self.assertEqual(response.status_code, 200)
        data = json.loads(response.data)
        self.assertEqual(data['status'], 'success')
        self.assertIn('output', data)
        self.assertIn('abcd123xyz', data['output'])

    @patch.object(claude_service, '_sdk_registry')
    def test_execute_error(self, mock_registry):
        """Test Claude SDK errors propagate through execute response."""
        mock_registry.get_or_create.return_value = _FakeSDKSession(
            [
                Message(
                    type=MessageType.ERROR_RESULT,
                    human_readable_message='**Task encountered an error**\n  • Turns: 2\n  • Duration: 500ms',
                )
            ]
        )

        response = self.client.post('/execute', json={'prompt': 'test prompt'})
        self.assertEqual(response.status_code, 200)
        data = json.loads(response.data)
        self.assertEqual(data['status'], 'error')

    def test_reset_session(self):
        """Test session reset endpoint."""
        response = self.client.post('/reset')
        self.assertEqual(response.status_code, 200)
        data = json.loads(response.data)
        self.assertEqual(data['status'], 'ok')
        self.assertEqual(data['session_id'], 'default')

    @parameterized.expand(
        [
            ('commands_run', 'post', '/commands/run'),
            ('commands_send_stdin', 'post', '/commands/send_stdin'),
            ('filesystem_list', 'post', '/filesystem/list'),
            ('filesystem_exists', 'post', '/filesystem/exists'),
            ('filesystem_write_files', 'post', '/filesystem/write_files'),
            ('filesystem_read', 'post', '/filesystem/read'),
        ]
    )
    def test_legacy_runner_routes_removed(self, _name, method, route):
        """Legacy runner file/command routes should no longer be exposed here."""
        response = getattr(self.client, method)(route, json={})
        self.assertEqual(response.status_code, 404)

    @parameterized.expand(
        [
            ('missing_prompt', {}, 400),
            ('empty_prompt', {'prompt': ''}, [200, 400]),  # Accept either behavior
        ]
    )
    def test_execute_stream_endpoint_validation(self, _name, json_data, expected_status):
        """Test execute_stream endpoint validation."""
        response = self.client.post('/execute_stream', json=json_data)
        if isinstance(expected_status, list):
            self.assertIn(response.status_code, expected_status)
        else:
            self.assertEqual(response.status_code, expected_status)

    @parameterized.expand(
        [
            (
                'success_case',
                [
                    Message(
                        type=MessageType.SYSTEM_INIT,
                        human_readable_message='**Claude SDK Session Ready**\n  • Session: default\n  • Model: claude-sonnet-test\n  • Permission mode: bypassPermissions\n  • Working directory: /repo',
                    ),
                    Message(type=MessageType.ASSISTANT, human_readable_message='**Assistant**: abcd123xyz'),
                    Message(
                        type=MessageType.SUCCESS_RESULT,
                        human_readable_message='**Task Completed Successfully**\n  • Result: abcd123xyz\n  • Duration: 2.5s',
                        content_type=ContentType.TEXT,
                    ),
                ],
                [
                    '**Claude SDK Session Ready**\n  • Session: default\n  • Model: claude-sonnet-test\n  • Permission mode: bypassPermissions\n  • Working directory: /repo',
                    '**Assistant**: abcd123xyz',
                    '**Task Completed Successfully**\n  • Result: abcd123xyz\n  • Duration: 2.5s',
                ],
            ),
            (
                'error_case',
                [
                    Message(
                        type=MessageType.ERROR_RESULT,
                        human_readable_message='**Claude SDK Error**\n  • sdk not installed',
                    )
                ],
                ['**Claude SDK Error**\n  • sdk not installed'],
            ),
        ]
    )
    @patch.object(claude_service, '_sdk_registry')
    def test_execute_stream(self, _name, stream_messages, expected_content, mock_registry):
        """Test streaming execution via SDK-backed session."""
        mock_registry.get_or_create.return_value = _FakeSDKSession(stream_messages)
        base64_data = base64.b64encode(json.dumps({'prompt': 'test prompt'}).encode('utf-8')).decode('utf-8')

        response = self.client.post('/execute_stream', data=base64_data, content_type='application/octet-stream')
        self.assertEqual(response.status_code, 200)  # Streaming always returns 200
        self.assertEqual(response.mimetype, 'text/event-stream')

        # Collect streaming data
        streamed_content = []
        for chunk in response.response:
            if chunk:
                streamed_content.append(
                    Message(
                        **json.loads(chunk.decode('utf-8') if isinstance(chunk, bytes) else chunk)
                    ).human_readable_message
                )

        # Verify the stream contained expected content
        self.assertEqual(expected_content, streamed_content)

    @patch.object(claude_service, '_sdk_registry')
    def test_session_routes(self, mock_registry):
        """Session CRUD-like routes surface in-memory SDK session info."""
        fake_session = _FakeSDKSession()
        mock_registry.get_or_create.return_value = fake_session
        mock_registry.get.return_value = fake_session
        mock_registry.list_sessions.return_value = [fake_session.info()]
        mock_registry.reset.return_value = True

        create_response = self.client.post('/sessions', json={'session_id': 'default'})
        self.assertEqual(create_response.status_code, 200)
        self.assertEqual(json.loads(create_response.data)['session_id'], 'default')

        list_response = self.client.get('/sessions')
        self.assertEqual(list_response.status_code, 200)
        self.assertLen(json.loads(list_response.data)['sessions'], 1)

        get_response = self.client.get('/sessions/default')
        self.assertEqual(get_response.status_code, 200)
        self.assertEqual(json.loads(get_response.data)['sdk_session_id'], 'sdk-default')
        self.assertTrue(json.loads(get_response.data)['can_pause'])

        interrupt_response = self.client.post('/sessions/default/interrupt')
        self.assertEqual(interrupt_response.status_code, 200)
        self.assertTrue(fake_session.interrupt_called)

        reset_response = self.client.post('/sessions/default/reset')
        self.assertEqual(reset_response.status_code, 200)
        self.assertEqual(json.loads(reset_response.data)['detail'], 'session reset')

    @parameterized.expand(
        [
            (
                'success_case',
                {
                    'file_contents': {
                        'project1/session1.jsonl': [
                            '{"timestamp": "2024-01-01T10:00:00Z", "message": {"id": "msg_001", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 100, "output_tokens": 50, "cache_creation_input_tokens": 10, "cache_read_input_tokens": 5}}}',
                            '{"timestamp": "2024-01-01T10:05:00Z", "message": {"id": "msg_002", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 200, "output_tokens": 150, "cache_creation_input_tokens": 20, "cache_read_input_tokens": 15}}}',
                        ],
                        'project2/session2.jsonl': [
                            '{"timestamp": "2024-01-01T11:00:00Z", "message": {"id": "msg_003", "model": "claude-3.5-sonnet-20241022", "usage": {"input_tokens": 75, "output_tokens": 25, "cache_creation_input_tokens": 5, "cache_read_input_tokens": 0}}}'
                        ],
                    },
                    'request_body': {},
                },
                200,
                {
                    'usage': [
                        {
                            'model': 'claude-sonnet-4-20250514',
                            'prompt_tokens': 300,  # 100 + 200
                            'response_tokens': 200,  # 50 + 150
                            'cache_creation_input_tokens': 30,  # 10 + 20
                            'cache_read_input_tokens': 20,  # 5 + 15
                        },
                        {
                            'model': 'claude-3.5-sonnet-20241022',
                            'prompt_tokens': 75,
                            'response_tokens': 25,
                            'cache_creation_input_tokens': 5,
                            'cache_read_input_tokens': 0,
                        },
                    ]
                },
            ),
            (
                'deduplication_streaming_chunks',
                {
                    'file_contents': {
                        'project1/session1.jsonl': [
                            # Same message ID logged 3 times (streaming chunks) - should only count final entry
                            '{"timestamp": "2024-01-01T10:00:00Z", "message": {"id": "msg_streaming", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 100, "output_tokens": 1, "cache_creation_input_tokens": 50, "cache_read_input_tokens": 1000}}}',
                            '{"timestamp": "2024-01-01T10:00:01Z", "message": {"id": "msg_streaming", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 100, "output_tokens": 1, "cache_creation_input_tokens": 50, "cache_read_input_tokens": 1000}}}',
                            '{"timestamp": "2024-01-01T10:00:02Z", "message": {"id": "msg_streaming", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 100, "output_tokens": 200, "cache_creation_input_tokens": 50, "cache_read_input_tokens": 1000}}}',
                            # Different message ID - should be counted separately
                            '{"timestamp": "2024-01-01T10:01:00Z", "message": {"id": "msg_other", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 50, "output_tokens": 25, "cache_creation_input_tokens": 10, "cache_read_input_tokens": 500}}}',
                        ],
                    },
                    'request_body': {},
                },
                200,
                {
                    'usage': [
                        {
                            'model': 'claude-sonnet-4-20250514',
                            # Only final entry of msg_streaming (100, 200, 50, 1000) + msg_other (50, 25, 10, 500)
                            'prompt_tokens': 150,  # 100 + 50 (NOT 100*3 + 50 = 350)
                            'response_tokens': 225,  # 200 + 25 (NOT 1+1+200 + 25 = 227)
                            'cache_creation_input_tokens': 60,  # 50 + 10 (NOT 50*3 + 10 = 160)
                            'cache_read_input_tokens': 1500,  # 1000 + 500 (NOT 1000*3 + 500 = 3500)
                        },
                    ]
                },
            ),
            (
                'no_claude_home',
                {
                    'skip_claude_home_setup': True,
                },
                500,
                {'error': 'CLAUDE_HOME environment variable not set'},
            ),
            (
                'no_jsonl_files',
                {
                    'file_contents': {},
                },
                400,
                {'error': 'No JSONL files found in CLAUDE_HOME/projects/'},
            ),
            (
                'time_filter_case',
                {
                    'file_contents': {
                        'project1/session1.jsonl': [
                            '{"timestamp": "2024-01-01T09:00:00Z", "message": {"id": "msg_before", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 100, "output_tokens": 50, "cache_creation_input_tokens": 10, "cache_read_input_tokens": 5}}}',
                            '{"timestamp": "2024-01-01T11:00:00Z", "message": {"id": "msg_within", "model": "claude-sonnet-4-20250514", "usage": {"input_tokens": 200, "output_tokens": 150, "cache_creation_input_tokens": 20, "cache_read_input_tokens": 15}}}',
                        ]
                    },
                    'request_body': {
                        'start_time': '2024-01-01T10:00:00Z',
                        'end_time': '2024-01-01T12:00:00Z',
                    },
                },
                200,
                {
                    'usage': [
                        {
                            'model': 'claude-sonnet-4-20250514',
                            'prompt_tokens': 200,
                            'response_tokens': 150,
                            'cache_creation_input_tokens': 20,
                            'cache_read_input_tokens': 15,
                        }
                    ]
                },
            ),
            (
                'invalid_request_data',
                {
                    'file_contents': {'project1/session1.jsonl': ['{}']},
                    'request_body': {'start_time': 'invalid_datetime_string'},
                },
                400,
                {
                    'error': "Failed to parse request: 1 validation error for CostUsageRequest\nstart_time\n  Input should be a valid datetime or date, invalid character in year [type=datetime_from_date_parsing, input_value='invalid_datetime_string', input_type=str]\n    For further information visit https://errors.pydantic.dev/2.12/v/datetime_from_date_parsing"
                },
            ),
        ]
    )
    @patch.dict('os.environ', {}, clear=True)
    def test_cost_usage_endpoint(self, _name, mock_setup, expected_status, expected_response):
        """Test cost usage endpoint with actual temporary files."""
        # Create temporary directory structure
        temp_dir = None
        if not mock_setup.get('skip_claude_home_setup'):
            temp_dir = tempfile.mkdtemp()
            projects_dir = os.path.join(temp_dir, 'projects')
            os.makedirs(projects_dir, exist_ok=True)

            # Create actual JSONL files
            for relative_path, lines in mock_setup.get('file_contents', {}).items():
                file_path = os.path.join(projects_dir, relative_path)
                os.makedirs(os.path.dirname(file_path), exist_ok=True)
                with open(file_path, 'w') as f:
                    for line in lines:
                        f.write(line + '\n')

            # Set CLAUDE_HOME to temp directory
            os.environ['CLAUDE_HOME'] = temp_dir

        try:
            # Send request with optional body data
            response = (
                self.client.get(
                    '/cost_usage',
                    data=json.dumps(mock_setup.get('request_body', {})),
                    content_type='application/json',
                )
                if 'request_body' in mock_setup
                else self.client.get('/cost_usage')
            )
            self.assertEqual(response.status_code, expected_status, response.data)
            data = json.loads(response.data)
            if expected_status == 200:
                self.assertIn('usage', data)
                self.assertEqual(len(data['usage']), len(expected_response['usage']))
                # Verify model usage entries (sorted by model for consistency)
                actual_usage = sorted(data['usage'], key=lambda x: x['model'])
                expected_usage = sorted(expected_response['usage'], key=lambda x: x['model'])
                for actual, expected in zip(actual_usage, expected_usage, strict=False):
                    self.assertEqual(actual['model'], expected['model'])
                    self.assertEqual(actual['prompt_tokens'], expected['prompt_tokens'])
                    self.assertEqual(actual['response_tokens'], expected['response_tokens'])
                    self.assertEqual(
                        actual['cache_creation_input_tokens'],
                        expected['cache_creation_input_tokens'],
                    )
                    self.assertEqual(
                        actual['cache_read_input_tokens'],
                        expected['cache_read_input_tokens'],
                    )
            else:
                self.assertIn('error', data)
                self.assertEqual(data['error'], expected_response['error'])
        finally:
            # Clean up temporary directory
            if temp_dir:
                shutil.rmtree(temp_dir)

    def test_message_content_type_assistant_with_text(self):
        """Test that ASSISTANT message with text block has content_type=TEXT."""
        raw_message = json.dumps(
            {
                'type': 'assistant',
                'message': {
                    'id': 'msg_123',
                    'type': 'message',
                    'role': 'assistant',
                    'content': [{'type': 'text', 'text': 'Hello world'}],
                    'model': 'claude-3',
                    'stop_reason': 'end_turn',
                    'usage': {'input_tokens': 10, 'output_tokens': 5},
                },
                'session_id': 'session_123',
            }
        )
        message = Message.from_anthropic(raw_message)
        self.assertEqual(message.content_type, ContentType.TEXT)

    def test_message_content_type_success_with_result(self):
        """Test that SUCCESS_RESULT message with non-empty result has content_type=TEXT."""
        raw_message = json.dumps(
            {
                'type': 'result',
                'subtype': 'success',
                'duration_ms': 1500.0,
                'duration_api_ms': 1200.0,
                'is_error': False,
                'num_turns': 5,
                'result': 'Task completed with output',
                'session_id': 'session_123',
                'total_cost_usd': 0.05,
            }
        )
        message = Message.from_anthropic(raw_message)
        self.assertEqual(message.content_type, ContentType.TEXT)

    def test_message_content_type_success_without_result(self):
        """Test that SUCCESS_RESULT message with empty result has content_type=UNSPECIFIED."""
        raw_message = json.dumps(
            {
                'type': 'result',
                'subtype': 'success',
                'duration_ms': 1500.0,
                'duration_api_ms': 1200.0,
                'is_error': False,
                'num_turns': 5,
                'result': '',
                'session_id': 'session_123',
                'total_cost_usd': 0.05,
            }
        )
        message = Message.from_anthropic(raw_message)
        self.assertEqual(message.content_type, ContentType.UNSPECIFIED)

    def test_should_filter_assistant_working_message(self):
        """Test that 'Assistant: Working...' message is filtered."""
        from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import ASSISTANT_WORKING_MESSAGE
        from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import MessageType

        message = Message(
            type=MessageType.ASSISTANT,
            human_readable_message=ASSISTANT_WORKING_MESSAGE,
            content_type=ContentType.UNSPECIFIED,
        )
        self.assertTrue(message.should_filter())

    def test_should_not_filter_other_messages(self):
        """Test that other messages are not filtered."""
        from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import MessageType

        test_cases = [
            'Processing your request',
            'Assistant: Working',
            '',
            'Assistant: Done!',
            'assistant: working...',  # case sensitive
            'Assistant: Working...',  # plain format (not what from_anthropic generates)
        ]

        for message_text in test_cases:
            with self.subTest(message=message_text):
                message = Message(
                    type=MessageType.ASSISTANT,
                    human_readable_message=message_text,
                    content_type=ContentType.TEXT,
                )
                self.assertFalse(message.should_filter())


if __name__ == '__main__':
    absltest.main()
