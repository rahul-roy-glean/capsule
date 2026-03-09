from __future__ import annotations

from collections.abc import Sequence
from typing import Any


def normalize_snapshot_command(command: str | dict[str, Any]) -> dict[str, Any]:
    """Normalize legacy shorthand to the control-plane SnapshotCommand shape."""
    if isinstance(command, str):
        return {"type": "shell", "args": ["bash", "-lc", command]}

    normalized = dict(command)
    if "type" in normalized and "args" in normalized:
        return normalized

    legacy_command = normalized.pop("command", None)
    if isinstance(legacy_command, str):
        normalized["type"] = normalized.get("type", "shell")
        normalized["args"] = ["bash", "-lc", legacy_command]
        return normalized

    if "args" in normalized and "type" not in normalized:
        normalized["type"] = "shell"
    return normalized


def normalize_snapshot_commands(
    commands: Sequence[str | dict[str, Any]] | None,
) -> list[dict[str, Any]] | None:
    if commands is None:
        return None
    return [normalize_snapshot_command(command) for command in commands]
