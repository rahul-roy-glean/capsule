from __future__ import annotations

import os
from dataclasses import dataclass

from capsule_sdk._version import __version__


@dataclass(frozen=True)
class ConnectionConfig:
    """Resolved connection configuration."""

    base_url: str
    token: str | None
    request_timeout: float
    startup_timeout: float
    operation_timeout: float
    user_agent: str

    @property
    def timeout(self) -> float:
        """Backward-compatible alias for the request timeout."""
        return self.request_timeout

    @classmethod
    def resolve(
        cls,
        *,
        base_url: str | None = None,
        token: str | None = None,
        timeout: float = 30.0,
        request_timeout: float | None = None,
        startup_timeout: float | None = None,
        operation_timeout: float | None = None,
    ) -> ConnectionConfig:
        resolved_base_url = (
            base_url
            or os.environ.get("CAPSULE_BASE_URL")
            or "http://localhost:8080"
        ).rstrip("/")

        resolved_token = token if token is not None else os.environ.get("CAPSULE_TOKEN")
        resolved_request_timeout = (
            request_timeout
            if request_timeout is not None
            else float(os.environ.get("CAPSULE_REQUEST_TIMEOUT", timeout))
        )
        resolved_startup_timeout = (
            startup_timeout
            if startup_timeout is not None
            else float(os.environ.get("CAPSULE_STARTUP_TIMEOUT", 45.0))
        )
        resolved_operation_timeout = (
            operation_timeout
            if operation_timeout is not None
            else float(os.environ.get("CAPSULE_OPERATION_TIMEOUT", 120.0))
        )

        return cls(
            base_url=resolved_base_url,
            token=resolved_token,
            request_timeout=resolved_request_timeout,
            startup_timeout=resolved_startup_timeout,
            operation_timeout=resolved_operation_timeout,
            user_agent=f"capsule-sdk-python/{__version__}",
        )
