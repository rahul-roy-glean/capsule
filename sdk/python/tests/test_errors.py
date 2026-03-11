from __future__ import annotations

from bf_sdk._errors import (
    BFAllocationTimeoutError,
    BFAuthError,
    BFConflict,
    BFConnectionError,
    BFError,
    BFHTTPError,
    BFNotFound,
    BFOperationTimeoutError,
    BFRateLimited,
    BFRequestTimeoutError,
    BFRunnerUnavailableError,
    BFServiceUnavailable,
    BFTimeoutError,
)


class TestErrorHierarchy:
    def test_base_error(self) -> None:
        err = BFError("something broke")
        assert str(err) == "something broke"
        assert isinstance(err, Exception)

    def test_http_error(self) -> None:
        err = BFHTTPError(500, "Internal error", request_id="req-1")
        assert err.status_code == 500
        assert err.message == "Internal error"
        assert err.request_id == "req-1"
        assert isinstance(err, BFError)

    def test_auth_error(self) -> None:
        err = BFAuthError()
        assert err.status_code == 401
        assert isinstance(err, BFHTTPError)

    def test_not_found(self) -> None:
        err = BFNotFound("runner not found")
        assert err.status_code == 404

    def test_conflict(self) -> None:
        err = BFConflict()
        assert err.status_code == 409

    def test_rate_limited(self) -> None:
        err = BFRateLimited(retry_after=5.0)
        assert err.status_code == 429
        assert err.retry_after == 5.0

    def test_service_unavailable(self) -> None:
        err = BFServiceUnavailable(retry_after=10.0)
        assert err.status_code == 503
        assert err.retry_after == 10.0

    def test_connection_error(self) -> None:
        err = BFConnectionError("connection refused")
        assert isinstance(err, BFError)

    def test_timeout_error(self) -> None:
        err = BFTimeoutError("timed out")
        assert isinstance(err, BFError)

    def test_request_timeout_error(self) -> None:
        err = BFRequestTimeoutError("request timed out", request_id="req-1", timeout=30.0)
        assert err.request_id == "req-1"
        assert err.timeout == 30.0

    def test_operation_timeout_error(self) -> None:
        err = BFOperationTimeoutError("wait_ready timed out", runner_id="r-1", operation="wait_ready")
        assert err.runner_id == "r-1"
        assert err.operation == "wait_ready"

    def test_allocation_timeout_error(self) -> None:
        err = BFAllocationTimeoutError("allocate timed out", workload_key="wk-1", request_id="req-2", timeout=45.0)
        assert err.workload_key == "wk-1"
        assert err.request_id == "req-2"
        assert err.operation == "allocate"

    def test_runner_unavailable_error(self) -> None:
        err = BFRunnerUnavailableError("runner unavailable", runner_id="r-1", status="terminated")
        assert err.runner_id == "r-1"
        assert err.status == "terminated"
