from __future__ import annotations


class BFError(Exception):
    """Base exception for all bf-sdk errors."""


class BFHTTPError(BFError):
    """HTTP error from the bazel-firecracker API."""

    def __init__(
        self,
        status_code: int,
        message: str,
        *,
        request_id: str | None = None,
        details: dict[str, object] | None = None,
    ) -> None:
        self.status_code = status_code
        self.message = message
        self.request_id = request_id
        self.details = details
        super().__init__(f"HTTP {status_code}: {message}")


class BFAuthError(BFHTTPError):
    """401 Unauthorized."""

    def __init__(self, message: str = "Unauthorized", **kwargs: object) -> None:
        super().__init__(401, message, **kwargs)  # type: ignore[arg-type]


class BFNotFound(BFHTTPError):
    """404 Not Found."""

    def __init__(self, message: str = "Not found", **kwargs: object) -> None:
        super().__init__(404, message, **kwargs)  # type: ignore[arg-type]


class BFConflict(BFHTTPError):
    """409 Conflict."""

    def __init__(self, message: str = "Conflict", **kwargs: object) -> None:
        super().__init__(409, message, **kwargs)  # type: ignore[arg-type]


class BFRateLimited(BFHTTPError):
    """429 Too Many Requests."""

    def __init__(
        self,
        message: str = "Rate limited",
        *,
        retry_after: float | None = None,
        **kwargs: object,
    ) -> None:
        self.retry_after = retry_after
        super().__init__(429, message, **kwargs)  # type: ignore[arg-type]


class BFServiceUnavailable(BFHTTPError):
    """503 Service Unavailable."""

    def __init__(
        self,
        message: str = "Service unavailable",
        *,
        retry_after: float | None = None,
        **kwargs: object,
    ) -> None:
        self.retry_after = retry_after
        super().__init__(503, message, **kwargs)  # type: ignore[arg-type]


class BFConnectionError(BFError):
    """Network-level connection error."""


class BFTimeoutError(BFError):
    """Request timeout."""
