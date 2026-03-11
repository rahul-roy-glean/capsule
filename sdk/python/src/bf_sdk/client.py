from __future__ import annotations

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.resources.layered_configs import LayeredConfigs
from bf_sdk.resources.runners import Runners
from bf_sdk.resources.snapshots import Snapshots
from bf_sdk.resources.workloads import Workloads
from bf_sdk.runner_config import RunnerConfigs


class BFClient:
    """Main entry point for the bazel-firecracker Python SDK."""

    def __init__(
        self,
        *,
        base_url: str | None = None,
        token: str | None = None,
        timeout: float = 30.0,
        request_timeout: float | None = None,
        startup_timeout: float | None = None,
        operation_timeout: float | None = None,
    ) -> None:
        self._config = ConnectionConfig.resolve(
            base_url=base_url,
            token=token,
            timeout=timeout,
            request_timeout=request_timeout,
            startup_timeout=startup_timeout,
            operation_timeout=operation_timeout,
        )
        self._http = HttpClient(self._config)
        self._layered_configs = LayeredConfigs(self._http)
        self.runners = Runners(self._http, layered_configs=self._layered_configs)
        self.snapshots = Snapshots(self._http)
        self.runner_configs = RunnerConfigs(self._layered_configs)
        self.workloads = Workloads(self._layered_configs, self.runners)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> BFClient:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()
