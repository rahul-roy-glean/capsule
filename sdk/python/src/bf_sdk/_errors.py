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

    def __init__(
        self,
        message: str = "Timed out",
        *,
        request_id: str | None = None,
        runner_id: str | None = None,
        timeout: float | None = None,
        operation: str | None = None,
    ) -> None:
        self.message = message
        self.request_id = request_id
        self.runner_id = runner_id
        self.timeout = timeout
        self.operation = operation
        super().__init__(message)


class BFRequestTimeoutError(BFTimeoutError):
    """Single request exceeded the configured request timeout."""


class BFOperationTimeoutError(BFTimeoutError):
    """A longer-running runner operation exceeded its timeout."""


class BFAllocationTimeoutError(BFTimeoutError):
    """Allocation did not produce a usable runner before the startup timeout."""

    def __init__(
        self,
        message: str,
        *,
        workload_key: str,
        request_id: str | None = None,
        timeout: float | None = None,
    ) -> None:
        self.workload_key = workload_key
        super().__init__(
            message,
            request_id=request_id,
            timeout=timeout,
            operation="allocate",
        )


class BFRunnerUnavailableError(BFError):
    """Runner cannot become ready because it is in a terminal or unavailable state."""

    def __init__(
        self,
        message: str,
        *,
        runner_id: str,
        status: str | None = None,
        retry_after: float | None = None,
    ) -> None:
        self.message = message
        self.runner_id = runner_id
        self.status = status
        self.retry_after = retry_after
        super().__init__(message)
