from __future__ import annotations

from enum import Enum
import json
import logging
from typing import Any

from pydantic import BaseModel
from pydantic import Field

logger = logging.getLogger(__file__)

TODO_WRITE_TOOL = 'TodoWrite'
TODO_READ_TOOL = 'TodoRead'
ASSISTANT_WORKING_MESSAGE = '**Assistant**: Working...'
FILE_EDIT_TOOLS = frozenset({'Edit', 'MultiEdit', 'Write'})


class MessageType(str, Enum):
    UNSPECIFIED = 'unspecified'
    ASSISTANT = 'assistant'
    USER = 'user'
    SUCCESS_RESULT = 'success_result'
    ERROR_RESULT = 'error_result'
    SYSTEM_INIT = 'system_init'
    FILE_EDIT = 'file_edit'
    GLEAN = 'Glean'


class ContentType(str, Enum):
    UNSPECIFIED = 'unspecified'
    TEXT = 'text'


class FileEdit(BaseModel):
    tool_name: str = Field(..., description='Tool that performed the edit')
    file_path: str = Field(..., description='Path of the file being edited')
    old_string: str = Field(default='', description='Original text for edit operations')
    new_string: str = Field(default='', description='Replacement text or full file content')


class Message(BaseModel):
    type: MessageType = Field(..., description='Type of message')
    human_readable_message: str = Field(..., description='Human readable message content')
    content_type: ContentType = Field(default=ContentType.UNSPECIFIED, description='Content type of message')
    file_edits: list[FileEdit] = Field(default_factory=list, description='Structured file edits')

    @classmethod
    def session_ready(
        cls,
        *,
        session_id: str,
        model: str,
        cwd: str,
        permission_mode: str,
    ) -> Message:
        return cls(
            type=MessageType.SYSTEM_INIT,
            human_readable_message=(
                '**Claude SDK Session Ready**\n'
                f'  • Session: {session_id}\n'
                f'  • Model: {model}\n'
                f'  • Permission mode: {permission_mode}\n'
                f'  • Working directory: {cwd}'
            ),
        )

    @classmethod
    def from_sdk_message(cls, sdk_message: Any) -> Message:
        message_kind = type(sdk_message).__name__

        if message_kind == 'AssistantMessage':
            content_text, content_type, file_edits = cls._format_blocks(getattr(sdk_message, 'content', []))
            message_type = MessageType.FILE_EDIT if file_edits else MessageType.ASSISTANT
            return cls(
                type=message_type,
                human_readable_message=f'**Assistant**: {content_text}',
                content_type=content_type,
                file_edits=file_edits,
            )

        if message_kind == 'UserMessage':
            content_text, _, _ = cls._format_blocks(cls._coerce_user_content(getattr(sdk_message, 'content', '')))
            return cls(type=MessageType.USER, human_readable_message=f'**User**: {content_text}')

        if message_kind == 'ResultMessage':
            return cls._format_result_message(
                subtype=str(getattr(sdk_message, 'subtype', '')),
                is_error=bool(getattr(sdk_message, 'is_error', False)),
                duration_ms=float(getattr(sdk_message, 'duration_ms', 0)),
                num_turns=int(getattr(sdk_message, 'num_turns', 0)),
                result=str(getattr(sdk_message, 'result', '') or ''),
            )

        if message_kind == 'TaskStartedMessage':
            return cls(
                type=MessageType.SYSTEM_INIT,
                human_readable_message=(
                    '**Background Task Started**\n'
                    f'  • Task: {getattr(sdk_message, "description", "Unknown")}\n'
                    f'  • Type: {getattr(sdk_message, "task_type", "unknown")}'
                ),
            )

        if message_kind == 'TaskProgressMessage':
            usage = getattr(sdk_message, 'usage', {}) or {}
            return cls(
                type=MessageType.SYSTEM_INIT,
                human_readable_message=(
                    '**Background Task Progress**\n'
                    f'  • Task: {getattr(sdk_message, "description", "Unknown")}\n'
                    f'  • Tool uses: {usage.get("tool_uses", 0)}\n'
                    f'  • Duration: {usage.get("duration_ms", 0)}ms'
                ),
            )

        if message_kind == 'TaskNotificationMessage':
            status = str(getattr(sdk_message, 'status', 'completed'))
            summary = str(getattr(sdk_message, 'summary', '') or '')
            title = 'Background Task Completed' if status == 'completed' else 'Background Task Update'
            return cls(
                type=MessageType.SYSTEM_INIT,
                human_readable_message=f'**{title}**\n  • Status: {status}\n  • Summary: {summary}',
            )

        if message_kind == 'SystemMessage':
            return cls(
                type=MessageType.SYSTEM_INIT,
                human_readable_message=(
                    f'**System Message**\n'
                    f'  • Subtype: {getattr(sdk_message, "subtype", "unknown")}\n'
                    f'  • Data: {getattr(sdk_message, "data", {})}'
                ),
            )

        return cls(
            type=MessageType.UNSPECIFIED,
            human_readable_message=str(sdk_message),
        )

    @classmethod
    def from_anthropic(cls, raw_message_chunk: str) -> Message:
        try:
            payload = json.loads(raw_message_chunk)
        except json.JSONDecodeError:
            logger.exception('Unsupported anthropic message chunk: %s', raw_message_chunk)
            return cls(type=MessageType.UNSPECIFIED, human_readable_message=raw_message_chunk)

        message_type = payload.get('type', '')
        if message_type == 'assistant':
            content_text, content_type, file_edits = cls._format_blocks(payload.get('message', {}).get('content', []))
            normalized_type = MessageType.FILE_EDIT if file_edits else MessageType.ASSISTANT
            return cls(
                type=normalized_type,
                human_readable_message=f'**Assistant**: {content_text}',
                content_type=content_type,
                file_edits=file_edits,
            )

        if message_type == 'user':
            content_text, _, _ = cls._format_blocks(
                cls._coerce_user_content(payload.get('message', {}).get('content', ''))
            )
            return cls(type=MessageType.USER, human_readable_message=f'**User**: {content_text}')

        if message_type == 'result':
            return cls._format_result_message(
                subtype=str(payload.get('subtype', '')),
                is_error=bool(payload.get('is_error', False)),
                duration_ms=float(payload.get('duration_ms', 0)),
                num_turns=int(payload.get('num_turns', 0)),
                result=str(payload.get('result', '') or ''),
            )

        if message_type == 'system' and payload.get('subtype') == 'init':
            tools = payload.get('tools', []) or []
            tool_sample = ', '.join(tools[:3])
            if len(tools) > 3:
                tool_sample += f' (and {len(tools) - 3} more)'
            tools_text = f'{len(tools)} tools available: {tool_sample}' if tools else 'no tools available'
            return cls(
                type=MessageType.SYSTEM_INIT,
                human_readable_message=(
                    '**System Initialized**\n'
                    f'  • Model: {payload.get("model", "")}\n'
                    f'  • Tools: {tools_text}\n'
                    f'  • Working directory: {payload.get("cwd", "")}'
                ),
            )

        return cls(type=MessageType.UNSPECIFIED, human_readable_message=raw_message_chunk)

    def should_filter(self) -> bool:
        return self.human_readable_message == ASSISTANT_WORKING_MESSAGE

    @staticmethod
    def _format_result_message(
        *,
        subtype: str,
        is_error: bool,
        duration_ms: float,
        num_turns: int,
        result: str,
    ) -> Message:
        duration_str = f'{duration_ms / 1000:.1f}s' if duration_ms > 1000 else f'{duration_ms:.0f}ms'
        if is_error or subtype.startswith('error'):
            error_desc = 'Maximum conversation turns reached' if subtype == 'error_max_turns' else 'Task encountered an error'
            return Message(
                type=MessageType.ERROR_RESULT,
                human_readable_message=(
                    f'**{error_desc}**\n'
                    f'  • Turns: {num_turns}\n'
                    f'  • Duration: {duration_str}'
                ),
            )

        content_type = ContentType.TEXT if result else ContentType.UNSPECIFIED
        return Message(
            type=MessageType.SUCCESS_RESULT,
            human_readable_message=(
                '**Task Completed Successfully**\n'
                f'  • Result: {result}\n'
                f'  • Duration: {duration_str}'
            ),
            content_type=content_type,
        )

    @staticmethod
    def _coerce_user_content(content: Any) -> list[Any]:
        if isinstance(content, str):
            return [{'type': 'text', 'text': content}]
        if isinstance(content, list):
            return content
        return [content]

    @staticmethod
    def _format_blocks(blocks: list[Any]) -> tuple[str, ContentType, list[FileEdit]]:
        content_parts: list[str] = []
        content_type = ContentType.UNSPECIFIED
        file_edits: list[FileEdit] = []

        for block in blocks:
            normalized = Message._normalize_block(block)
            block_type = normalized.get('type', '')

            if block_type == 'text':
                text = str(normalized.get('text', ''))
                if text:
                    content_type = ContentType.TEXT
                    content_parts.append(text)
                continue

            if block_type == 'tool_use':
                tool_name = str(normalized.get('name', 'unknown'))
                tool_input = normalized.get('input', {})
                tool_icon = Message._get_tool_icon(tool_name)
                if tool_name == TODO_WRITE_TOOL:
                    todo_content = Message._format_todo_content(tool_input)
                    content_parts.append(f'\n{tool_icon} **{tool_name}**:\n{todo_content}\n')
                elif tool_name == TODO_READ_TOOL:
                    content_parts.append(f'\n{tool_icon} **Reading todo list...**\n')
                else:
                    tool_desc = Message._get_tool_description(tool_name, tool_input)
                    content_parts.append(f'\n{tool_icon} **{tool_desc}**\n')
                file_edits.extend(Message._extract_file_edits(tool_name, tool_input))
                continue

            if block_type == 'tool_result':
                todo_formatted = Message._try_format_todo_result(normalized.get('content'))
                if todo_formatted:
                    content_parts.append(todo_formatted)
                    continue
                result_content = normalized.get('content', '')
                result_preview = Message._preview_text(result_content, 200)
                if normalized.get('is_error'):
                    content_parts.append(f'\n**Tool Error**: {result_preview}\n')
                else:
                    content_parts.append(f'\n**Result**: {result_preview}\n')
                continue

            if block_type == 'server_tool_use':
                tool_name = str(normalized.get('name', 'unknown'))
                tool_icon = Message._get_tool_icon(tool_name)
                content_parts.append(f'\n{tool_icon} **Server Tool: {tool_name}**\n')
                continue

            if block_type == 'thinking':
                thinking_content = str(normalized.get('thinking', ''))
                if thinking_content:
                    content_parts.append(f'\n💭 **Thinking**: {thinking_content}\n')
                continue

            if block_type == 'web_search_tool_result':
                preview = Message._preview_text(normalized.get('content', ''), 200)
                content_parts.append(f'\n**Web Search Results**:\n{preview}\n')
                continue

            if block_type == 'document':
                title = str(normalized.get('title', 'Untitled'))
                context = str(normalized.get('context', ''))
                if context:
                    content_parts.append(f'\n**Document**: {title}\n  Context: {Message._preview_text(context, 100)}\n')
                else:
                    content_parts.append(f'\n**Document**: {title}\n')
                continue

        content_text = ' '.join(content_parts).strip() if content_parts else 'Working...'
        return content_text, content_type, file_edits

    @staticmethod
    def _normalize_block(block: Any) -> dict[str, Any]:
        if isinstance(block, dict):
            return block

        if hasattr(block, 'text'):
            return {'type': 'text', 'text': getattr(block, 'text', '')}
        if hasattr(block, 'thinking'):
            return {'type': 'thinking', 'thinking': getattr(block, 'thinking', '')}
        if hasattr(block, 'name') and hasattr(block, 'input'):
            return {
                'type': 'tool_use',
                'name': getattr(block, 'name', ''),
                'input': getattr(block, 'input', {}),
            }
        if hasattr(block, 'tool_use_id'):
            return {
                'type': 'tool_result',
                'tool_use_id': getattr(block, 'tool_use_id', ''),
                'content': getattr(block, 'content', ''),
                'is_error': getattr(block, 'is_error', False),
            }
        if hasattr(block, 'type'):
            return {'type': getattr(block, 'type')}
        return {'type': '', 'raw': str(block)}

    @staticmethod
    def _preview_text(content: Any, limit: int) -> str:
        value = content if isinstance(content, str) else json.dumps(content, default=str)
        return value[:limit] + '...' if len(value) > limit else value

    @staticmethod
    def _get_tool_icon(tool_name: str) -> str:
        tool_icons = {
            TODO_WRITE_TOOL: '📝',
            TODO_READ_TOOL: '📋',
            'Bash': '💻',
            'Read': '📖',
            'Write': '✏️',
            'Edit': '✂️',
            'MultiEdit': '✂️',
            'Search': '🔍',
            'Grep': '🔎',
            'Glob': '📁',
            'LS': '📂',
            'WebFetch': '🌐',
            'WebSearch': '🔍',
            'Task': '🔄',
            'Agent': '🔄',
            'exit_plan_mode': '✅',
            'NotebookRead': '📓',
            'NotebookEdit': '📝',
        }
        return tool_icons.get(tool_name, '🔧')

    @staticmethod
    def _format_todo_content(todo_input: Any) -> str:
        if isinstance(todo_input, dict) and 'todos' in todo_input:
            todos = todo_input['todos']
            if isinstance(todos, list):
                return Message._format_todo_list(todos)
        return str(todo_input)

    @staticmethod
    def _try_format_todo_result(result_content: Any) -> str | None:
        if 'todos' not in str(result_content).lower():
            return None
        try:
            payload = json.loads(result_content) if isinstance(result_content, str) else result_content
        except (json.JSONDecodeError, TypeError):
            return None
        if isinstance(payload, dict) and isinstance(payload.get('todos'), list):
            todo_list = Message._format_todo_list(payload['todos'])
            return f'\n**Current Todo List**:\n{todo_list}\n'
        return None

    @staticmethod
    def _get_tool_description(tool_name: str, tool_input: Any) -> str:
        if not isinstance(tool_input, dict):
            return f'Using {tool_name}'

        if tool_name == 'Read':
            file_path = tool_input.get('file_path', '')
            if file_path:
                return f'Reading {file_path.split("/")[-1]}'
        if tool_name == 'Write':
            file_path = tool_input.get('file_path', '')
            if file_path:
                return f'Writing to {file_path.split("/")[-1]}'
        if tool_name in {'Edit', 'MultiEdit'}:
            file_path = tool_input.get('file_path', '')
            if file_path:
                return f'Editing {file_path.split("/")[-1]}'
        if tool_name == 'Bash':
            command = tool_input.get('command', '')
            if command:
                return f'Running: {command[:50] + "..." if len(command) > 50 else command}'
        if tool_name == 'Grep':
            pattern = tool_input.get('pattern', '')
            if pattern:
                return f'Searching for: {pattern}'
        if tool_name == 'Glob':
            pattern = tool_input.get('pattern', '')
            if pattern:
                return f'Finding files: {pattern}'
        if tool_name == 'LS':
            path = tool_input.get('path', '')
            if path:
                return f'Listing: {path.split("/")[-1] or path}'
        if tool_name == 'WebSearch':
            query = tool_input.get('query', '')
            if query:
                return f'Searching web for: {query}'
        if tool_name in {'Task', 'Agent'}:
            description = tool_input.get('description', '')
            if description:
                return f'Task: {description}'
        return f'Using {tool_name}'

    @staticmethod
    def _extract_file_edits(tool_name: str, tool_input: Any) -> list[FileEdit]:
        if tool_name not in FILE_EDIT_TOOLS or not isinstance(tool_input, dict):
            return []

        if tool_name == 'Write':
            file_path = tool_input.get('file_path', '')
            if not file_path:
                return []
            return [
                FileEdit(
                    tool_name=tool_name,
                    file_path=file_path,
                    new_string=tool_input.get('content', ''),
                )
            ]

        if tool_name == 'Edit':
            file_path = tool_input.get('file_path', '')
            if not file_path:
                return []
            return [
                FileEdit(
                    tool_name=tool_name,
                    file_path=file_path,
                    old_string=tool_input.get('old_string', ''),
                    new_string=tool_input.get('new_string', ''),
                )
            ]

        if tool_name == 'MultiEdit':
            file_path = tool_input.get('file_path', '')
            edits_input = tool_input.get('edits', [])
            if not file_path or not isinstance(edits_input, list):
                return []
            return [
                FileEdit(
                    tool_name=tool_name,
                    file_path=file_path,
                    old_string=edit.get('old_string', ''),
                    new_string=edit.get('new_string', ''),
                )
                for edit in edits_input
                if isinstance(edit, dict)
            ]

        return []

    @staticmethod
    def _format_todo_list(todos: list[dict]) -> str:
        if not todos:
            return '  (No todos)'

        formatted_todos: list[str] = []
        for todo in todos:
            status_icon = {
                'pending': '⏳',
                'in_progress': '🔄',
                'completed': '✅',
            }.get(todo.get('status', 'pending'), '❓')
            priority_icon = {
                'high': '🔴',
                'medium': '🟡',
                'low': '🟢',
            }.get(todo.get('priority', 'medium'), '')
            priority_prefix = f'{priority_icon} ' if priority_icon else ' '
            content = todo.get('content', 'Untitled task')
            formatted_todos.append(f'  {status_icon} {priority_prefix}{content}')

        return '\n'.join(formatted_todos)
