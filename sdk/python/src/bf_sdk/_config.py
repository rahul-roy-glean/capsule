from __future__ import annotations

import os
from dataclasses import dataclass

from bf_sdk._version import __version__


@dataclass(frozen=True)
class ConnectionConfig:
    """Resolved connection configuration."""

    base_url: str
    api_key: str | None
    timeout: float
    user_agent: str

    @classmethod
    def resolve(
        cls,
        *,
        base_url: str | None = None,
        api_key: str | None = None,
        timeout: float = 30.0,
    ) -> ConnectionConfig:
        resolved_base_url = (
            base_url
            or os.environ.get("BF_BASE_URL")
            or "http://localhost:8080"
        ).rstrip("/")

        resolved_api_key = api_key or os.environ.get("BF_API_KEY")

        return cls(
            base_url=resolved_base_url,
            api_key=resolved_api_key,
            timeout=timeout,
            user_agent=f"bf-sdk-python/{__version__}",
        )
