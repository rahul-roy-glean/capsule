from __future__ import annotations

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.resources.layered_configs import LayeredConfigs
from bf_sdk.resources.runners import Runners
from bf_sdk.resources.snapshots import Snapshots
from bf_sdk.runner_config import RunnerConfigs


class BFClient:
    """Main entry point for the bazel-firecracker Python SDK."""

    def __init__(
        self,
        *,
        base_url: str | None = None,
        api_key: str | None = None,
        timeout: float = 30.0,
    ) -> None:
        self._config = ConnectionConfig.resolve(base_url=base_url, api_key=api_key, timeout=timeout)
        self._http = HttpClient(self._config)
        self.runners = Runners(self._http)
        self.layered_configs = LayeredConfigs(self._http)
        self.snapshots = Snapshots(self._http)
        self.runner_configs = RunnerConfigs(self.layered_configs)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> BFClient:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()
