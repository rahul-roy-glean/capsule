"""Config ID validation utilities."""

from __future__ import annotations

import re

_VALID_CONFIG_ID = re.compile(r"^[a-z0-9][a-z0-9-]*[a-z0-9]$")


def validate_config_id(config_id: str) -> None:
    """Validate that *config_id* is a valid slug.

    A valid config ID is 3-64 characters, lowercase alphanumeric with hyphens,
    and must not start or end with a hyphen.

    Raises:
        ValueError: If the config ID is invalid.
    """
    if len(config_id) < 3 or len(config_id) > 64:
        raise ValueError(
            f"config_id must be 3-64 characters, got {len(config_id)}: {config_id!r}"
        )
    if not _VALID_CONFIG_ID.match(config_id):
        raise ValueError(
            f"config_id must be lowercase alphanumeric with hyphens "
            f"(no leading/trailing hyphens): {config_id!r}"
        )
