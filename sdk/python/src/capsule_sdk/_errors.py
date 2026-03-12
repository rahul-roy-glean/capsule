from __future__ import annotations


class CapsuleError(Exception):
    """Base exception for all capsule-sdk errors."""


class CapsuleHTTPError(CapsuleError):
    """HTTP error from the capsule API."""

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


class CapsuleAuthError(CapsuleHTTPError):
    """401 Unauthorized."""

    def __init__(self, message: str = "Unauthorized", **kwargs: object) -> None:
        super().__init__(401, message, **kwargs)  # type: ignore[arg-type]


class CapsuleNotFound(CapsuleHTTPError):
    """404 Not Found."""

    def __init__(self, message: str = "Not found", **kwargs: object) -> None:
        super().__init__(404, message, **kwargs)  # type: ignore[arg-type]


class CapsuleConflict(CapsuleHTTPError):
    """409 Conflict."""

    def __init__(self, message: str = "Conflict", **kwargs: object) -> None:
        super().__init__(409, message, **kwargs)  # type: ignore[arg-type]


class CapsuleRateLimited(CapsuleHTTPError):
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


class CapsuleServiceUnavailable(CapsuleHTTPError):
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


class CapsuleConnectionError(CapsuleError):
    """Network-level connection error."""


class CapsuleTimeoutError(CapsuleError):
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


class CapsuleRequestTimeoutError(CapsuleTimeoutError):
    """Single request exceeded the configured request timeout."""


class CapsuleOperationTimeoutError(CapsuleTimeoutError):
    """A longer-running runner operation exceeded its timeout."""


class CapsuleAllocationTimeoutError(CapsuleTimeoutError):
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


class CapsuleRunnerUnavailableError(CapsuleError):
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
