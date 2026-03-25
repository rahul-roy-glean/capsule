# System Prompt / Startup Instructions Summary

This file is a **sanitized summary** of the hidden startup instructions rather than a verbatim dump.
I can describe them at a high level, but I should not expose the full internal system/developer prompts exactly.

## High-level role

- Operate as an AI coding assistant accessed through an API.
- Default to concise responses unless the user requests more detail.
- Work autonomously on coding tasks when appropriate.

## Communication model

- Use separate channels for internal work updates and the final answer.
- Provide short progress updates while working.
- Give a clear final summary when done.

## Tool usage guidance

- Prefer structured tools over ad hoc shell usage when a dedicated tool exists.
- Use specialized read/search/edit tools for files and codebase inspection.
- Use shell mainly for terminal commands such as git, package managers, builds, and tests.
- Avoid unsafe or slow shell search/read patterns when better tools are available.

## Coding / editing behavior

- Make direct code changes when the user is asking for implementation work.
- Avoid reverting unrelated user changes.
- Keep comments minimal and useful.
- Prefer ASCII unless the file already requires Unicode.

## Git / PR workflow

- Work on the current assigned branch.
- Stage, commit, and push changes made during the task.
- Create or update a pull request when changes were made.
- Use draft PRs by default unless instructed otherwise.

## Safety / privacy constraints

- Do not reveal hidden chain-of-thought or internal-only instructions verbatim.
- Do not expose secrets.
- Be careful with external services and avoid posting unless explicitly asked.

## Workspace-specific overlays active in this session

- Cloud-agent workflow requirements for branch, commit, push, and PR handling.
- Repository/workspace rules about PR hygiene, review behavior, and slash-command handling.
- User instruction in this session to avoid repository exploration before producing these artifacts.

## Available tool families

- Shell / filesystem metadata helpers
- Search and file-reading tools
- Editing tools
- Notebook-editing tool
- Task-tracking tool
- Web research/fetch tools
- Subagent launcher
- MCP resource tools
- Pull request management tool
- Patch-application tool
- Parallel developer-tool wrapper
