from __future__ import annotations

import os
from dataclasses import dataclass

from bf_sdk._version import __version__


@dataclass(frozen=True)
class ConnectionConfig:
    """Resolved connection configuration."""

    base_url: str
    token: str | None
    timeout: float
    user_agent: str

    @classmethod
    def resolve(
        cls,
        *,
        base_url: str | None = None,
        token: str | None = None,
        timeout: float = 30.0,
    ) -> ConnectionConfig:
        resolved_base_url = (
            base_url
            or os.environ.get("BF_BASE_URL")
            or "http://localhost:8080"
        ).rstrip("/")

        resolved_token = token if token is not None else os.environ.get("BF_TOKEN")

        return cls(
            base_url=resolved_base_url,
            token=resolved_token,
            timeout=timeout,
            user_agent=f"bf-sdk-python/{__version__}",
        )
