from __future__ import annotations

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._http_async import AsyncHttpClient
from capsule_sdk.async_runner_config import AsyncRunnerConfigs
from capsule_sdk.resources.async_hosts import AsyncHosts
from capsule_sdk.resources.async_layered_configs import AsyncLayeredConfigs
from capsule_sdk.resources.async_runners import AsyncRunners
from capsule_sdk.resources.async_snapshots import AsyncSnapshots
from capsule_sdk.resources.async_workloads import AsyncWorkloads


class AsyncCapsuleClient:
    """Async main entry point for the capsule Python SDK."""

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
        self._http = AsyncHttpClient(self._config)
        self._layered_configs = AsyncLayeredConfigs(self._http)
        self.runners = AsyncRunners(self._http, layered_configs=self._layered_configs)
        self.snapshots = AsyncSnapshots(self._http)
        self.hosts = AsyncHosts(self._http)
        self.runner_configs = AsyncRunnerConfigs(self._layered_configs)
        self.workloads = AsyncWorkloads(self._layered_configs, self.runners)

    async def close(self) -> None:
        await self._http.close()

    async def __aenter__(self) -> AsyncCapsuleClient:
        return self

    async def __aexit__(self, *_: object) -> None:
        await self.close()
