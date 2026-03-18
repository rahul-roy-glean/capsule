"""Focused unit tests for message formatting functionality."""

import pytest

from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import FileEdit
from python_scio.platform.common.app.claude_sandbox_service.sandbox_message import Message


class TestMessageFormatting:
    """Test cases for message formatting helper methods."""

    def test_get_tool_icon(self):
        """Test tool icon mapping."""
        assert Message._get_tool_icon('Read') == '📖'
        assert Message._get_tool_icon('Write') == '✏️'
        assert Message._get_tool_icon('Edit') == '✂️'
        assert Message._get_tool_icon('MultiEdit') == '✂️'
        assert Message._get_tool_icon('Bash') == '💻'
        assert Message._get_tool_icon('Grep') == '🔎'
        assert Message._get_tool_icon('Glob') == '📁'
        assert Message._get_tool_icon('LS') == '📂'
        assert Message._get_tool_icon('WebFetch') == '🌐'
        assert Message._get_tool_icon('WebSearch') == '🔍'
        assert Message._get_tool_icon('Task') == '🔄'
        assert Message._get_tool_icon('TodoWrite') == '📝'
        assert Message._get_tool_icon('TodoRead') == '📋'
        assert Message._get_tool_icon('exit_plan_mode') == '✅'
        assert Message._get_tool_icon('NotebookRead') == '📓'
        assert Message._get_tool_icon('NotebookEdit') == '📝'
        assert Message._get_tool_icon('UnknownTool') == '🔧'  # Default

    def test_get_tool_description_file_operations(self):
        """Test tool description for file operations."""
        # Read operations
        assert Message._get_tool_description('Read', {'file_path': '/path/to/file.py'}) == 'Reading file.py'
        assert (
            Message._get_tool_description('Read', {'file_path': '/very/long/path/to/config.json'})
            == 'Reading config.json'
        )
        assert Message._get_tool_description('Read', {'file_path': 'simple.txt'}) == 'Reading simple.txt'

        # Write operations
        assert Message._get_tool_description('Write', {'file_path': '/path/to/output.log'}) == 'Writing to output.log'
        assert Message._get_tool_description('Write', {'file_path': 'new_file.md'}) == 'Writing to new_file.md'

        # Edit operations
        assert Message._get_tool_description('Edit', {'file_path': '/src/main.ts'}) == 'Editing main.ts'
        assert (
            Message._get_tool_description('MultiEdit', {'file_path': '/test/test_module.py'})
            == 'Editing test_module.py'
        )

    def test_get_tool_description_bash_commands(self):
        """Test tool description for bash commands."""
        # Short commands
        assert Message._get_tool_description('Bash', {'command': 'ls -la'}) == 'Running: ls -la'
        assert Message._get_tool_description('Bash', {'command': 'pwd'}) == 'Running: pwd'

        # Long commands - should truncate at 50 chars
        long_command = 'python -m pytest tests/very/long/path/to/test_file.py -v --cov=module --cov-report=html'
        expected = 'Running: python -m pytest tests/very/long/path/to/test_file...'
        assert Message._get_tool_description('Bash', {'command': long_command}) == expected

    def test_get_tool_description_search_operations(self):
        """Test tool description for search operations."""
        assert Message._get_tool_description('Grep', {'pattern': 'TODO'}) == 'Searching for: TODO'
        assert Message._get_tool_description('Grep', {'pattern': 'error.*handling'}) == 'Searching for: error.*handling'
        assert Message._get_tool_description('Glob', {'pattern': '*.py'}) == 'Finding files: *.py'
        assert Message._get_tool_description('Glob', {'pattern': '**/*.test.js'}) == 'Finding files: **/*.test.js'
        assert (
            Message._get_tool_description('WebSearch', {'query': 'Python best practices'})
            == 'Searching web for: Python best practices'
        )

    def test_get_tool_description_directory_operations(self):
        """Test tool description for directory operations."""
        assert Message._get_tool_description('LS', {'path': '/home/user/documents'}) == 'Listing: documents'
        assert Message._get_tool_description('LS', {'path': '/var/log/'}) == 'Listing: /var/log/'  # Trailing slash case
        assert Message._get_tool_description('LS', {'path': '/'}) == 'Listing: /'

    def test_get_tool_description_task(self):
        """Test tool description for Task tool."""
        assert (
            Message._get_tool_description('Task', {'description': 'Search for TODO comments'})
            == 'Task: Search for TODO comments'
        )
        assert (
            Message._get_tool_description('Task', {'description': 'Analyze code structure'})
            == 'Task: Analyze code structure'
        )

    def test_get_tool_description_edge_cases(self):
        """Test edge cases for tool description."""
        # No input
        assert Message._get_tool_description('SomeTool', None) == 'Using SomeTool'
        assert Message._get_tool_description('AnotherTool', {}) == 'Using AnotherTool'

        # Missing expected fields
        assert Message._get_tool_description('Read', {'wrong_field': 'value'}) == 'Using Read'
        assert Message._get_tool_description('Bash', {'cmd': 'ls'}) == 'Using Bash'  # Wrong field name

        # Non-dict input
        assert Message._get_tool_description('Tool', 'string input') == 'Using Tool'
        assert Message._get_tool_description('Tool', ['list', 'input']) == 'Using Tool'

    def test_format_todo_list(self):
        """Test todo list formatting."""
        # Empty list
        assert Message._format_todo_list([]) == '  (No todos)'

        # Single todo
        todos = [{'id': '1', 'content': 'Fix bug', 'status': 'pending', 'priority': 'high'}]
        expected = '  ⏳ 🔴 Fix bug'
        assert Message._format_todo_list(todos) == expected

        # Multiple todos with different statuses
        todos = [
            {'id': '1', 'content': 'Review code', 'status': 'pending', 'priority': 'high'},
            {'id': '2', 'content': 'Write tests', 'status': 'in_progress', 'priority': 'medium'},
            {'id': '3', 'content': 'Deploy', 'status': 'completed', 'priority': 'low'},
        ]
        expected = '  ⏳ 🔴 Review code\n  🔄 🟡 Write tests\n  ✅ 🟢 Deploy'
        assert Message._format_todo_list(todos) == expected

        # Missing fields - priority defaults to 'medium' when missing
        todos = [
            {'content': 'Task without status'},
            {'status': 'pending', 'priority': 'high'},  # Missing content
            {'id': '3', 'content': 'Normal task', 'status': 'pending', 'priority': 'medium'},
        ]
        expected = '  ⏳ 🟡 Task without status\n  ⏳ 🔴 Untitled task\n  ⏳ 🟡 Normal task'
        assert Message._format_todo_list(todos) == expected

        # Unknown status/priority
        todos = [
            {'content': 'Unknown status', 'status': 'unknown', 'priority': 'medium'},
            {'content': 'Unknown priority', 'status': 'pending', 'priority': 'unknown'},
        ]
        expected = '  ❓ 🟡 Unknown status\n  ⏳  Unknown priority'
        assert Message._format_todo_list(todos) == expected

    def test_format_todo_content(self):
        """Test todo content formatting."""
        # Dict with todos
        todo_input = {
            'todos': [
                {'id': '1', 'content': 'Task 1', 'status': 'pending', 'priority': 'high'},
                {'id': '2', 'content': 'Task 2', 'status': 'completed', 'priority': 'low'},
            ]
        }
        expected = '  ⏳ 🔴 Task 1\n  ✅ 🟢 Task 2'
        assert Message._format_todo_content(todo_input) == expected

        # Non-dict input
        assert Message._format_todo_content('string input') == 'string input'
        assert Message._format_todo_content(123) == '123'

        # Dict without todos key
        assert Message._format_todo_content({'other': 'data'}) == "{'other': 'data'}"


class TestExtractFileEdits:
    """Test cases for _extract_file_edits."""

    def test_edit_tool(self):
        edits = Message._extract_file_edits(
            'Edit',
            {
                'file_path': '/workspace/repo/main.py',
                'old_string': 'def foo():',
                'new_string': 'def bar():',
            },
        )
        assert edits == [
            FileEdit(
                tool_name='Edit',
                file_path='/workspace/repo/main.py',
                old_string='def foo():',
                new_string='def bar():',
            )
        ]

    def test_write_tool(self):
        edits = Message._extract_file_edits(
            'Write',
            {
                'file_path': '/workspace/repo/new_file.py',
                'content': 'print("hello")',
            },
        )
        assert edits == [
            FileEdit(
                tool_name='Write',
                file_path='/workspace/repo/new_file.py',
                new_string='print("hello")',
            )
        ]

    def test_multi_edit_tool(self):
        edits = Message._extract_file_edits(
            'MultiEdit',
            {
                'file_path': '/workspace/repo/app.py',
                'edits': [
                    {'old_string': 'import os', 'new_string': 'import sys'},
                    {'old_string': 'x = 1', 'new_string': 'x = 2'},
                ],
            },
        )
        assert len(edits) == 2
        assert edits[0].old_string == 'import os'
        assert edits[0].new_string == 'import sys'
        assert edits[1].old_string == 'x = 1'
        assert edits[1].file_path == '/workspace/repo/app.py'

    def test_non_edit_tool_returns_empty(self):
        assert Message._extract_file_edits('Read', {'file_path': '/foo'}) == []
        assert Message._extract_file_edits('Bash', {'command': 'ls'}) == []
        assert Message._extract_file_edits('Grep', {'pattern': 'foo'}) == []

    def test_missing_file_path_returns_empty(self):
        assert Message._extract_file_edits('Edit', {'old_string': 'a', 'new_string': 'b'}) == []
        assert Message._extract_file_edits('Write', {'content': 'hello'}) == []

    def test_none_input_returns_empty(self):
        assert Message._extract_file_edits('Edit', None) == []

    def test_non_dict_input_returns_empty(self):
        assert Message._extract_file_edits('Edit', 'string') == []

    def test_multi_edit_empty_edits_list(self):
        assert (
            Message._extract_file_edits(
                'MultiEdit',
                {
                    'file_path': '/workspace/repo/app.py',
                    'edits': [],
                },
            )
            == []
        )

    def test_multi_edit_non_list_edits(self):
        assert (
            Message._extract_file_edits(
                'MultiEdit',
                {
                    'file_path': '/workspace/repo/app.py',
                    'edits': 'not a list',
                },
            )
            == []
        )


if __name__ == '__main__':
    pytest.main([__file__, '-v'])
