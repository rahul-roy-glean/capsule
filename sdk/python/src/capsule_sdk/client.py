from __future__ import annotations

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._http import HttpClient
from capsule_sdk.resources.hosts import Hosts
from capsule_sdk.resources.layered_configs import LayeredConfigs
from capsule_sdk.resources.runners import Runners
from capsule_sdk.resources.snapshots import Snapshots
from capsule_sdk.resources.workloads import Workloads
from capsule_sdk.runner_config import RunnerConfigs


class CapsuleClient:
    """Main entry point for the capsule Python SDK."""

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
        self.hosts = Hosts(self._http)
        self.runner_configs = RunnerConfigs(self._layered_configs)
        self.workloads = Workloads(self._layered_configs, self.runners)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> CapsuleClient:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()
