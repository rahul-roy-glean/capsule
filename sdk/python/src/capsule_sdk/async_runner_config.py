from __future__ import annotations

from capsule_sdk.models.layered_config import BuildResponse, CreateConfigResponse
from capsule_sdk.resources.async_layered_configs import AsyncLayeredConfigs
from capsule_sdk.runner_config import RunnerConfig


class AsyncRunnerConfigs:
    """Async high-level declarative runner config management."""

    def __init__(self, layered_configs: AsyncLayeredConfigs) -> None:
        self._lc = layered_configs

    async def apply(self, cfg: RunnerConfig) -> CreateConfigResponse:
        return await self._lc.create(cfg.to_create_body())

    async def build(self, config_id: str, *, force: bool = False, clean: bool = False) -> BuildResponse:
        return await self._lc.build(config_id, force=force, clean=clean)
