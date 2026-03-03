from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any

from bf_sdk.models.layered_config import (
    BuildResponse,
    CreateConfigResponse,
    LayerDef,
)
from bf_sdk.resources.layered_configs import LayeredConfigs


@dataclass(frozen=True)
class RunnerConfig:
    """Declarative runner configuration.

    A RunnerConfig is a pure-data description of a desired runner shape.
    It maps 1:1 to a LayeredConfig on the server, with fluent builder
    methods for ergonomic construction.

    Usage::

        cfg = (
            RunnerConfig("my-workload")
            .with_display_name("My sandbox")
            .with_base_image("ubuntu:22.04")
            .with_commands(["pip install -e .[dev]"])
            .with_tier("small")
            .with_auto_pause(True)
        )
    """

    display_name: str
    _base_image: str | None = field(default=None, repr=False)
    _layers: list[LayerDef] | None = field(default=None, repr=False)
    _commands: list[dict[str, Any]] = field(default_factory=lambda: list[dict[str, Any]](), repr=False)
    _start_command: dict[str, Any] | None = field(default=None, repr=False)
    _auto_pause: bool | None = field(default=None, repr=False)
    _ttl: int | None = field(default=None, repr=False)
    _tier: str | None = field(default=None, repr=False)
    _ci_system: str | None = field(default=None, repr=False)
    _auto_rollout: bool | None = field(default=None, repr=False)
    _session_max_age_seconds: int | None = field(default=None, repr=False)
    _network_policy_preset: str | None = field(default=None, repr=False)
    _network_policy: Any | None = field(default=None, repr=False)

    # -- Fluent withers (return new immutable copy) ----------------------------

    def with_display_name(self, name: str) -> RunnerConfig:
        return replace(self, display_name=name)

    def with_base_image(self, image: str) -> RunnerConfig:
        return replace(self, _base_image=image)

    def with_layers(self, layers: list[LayerDef]) -> RunnerConfig:
        return replace(self, _layers=layers)

    def with_commands(self, cmds: list[str | dict[str, Any]]) -> RunnerConfig:
        normalized = [{"command": c} if isinstance(c, str) else c for c in cmds]
        return replace(self, _commands=normalized)

    def with_start_command(self, cmd: dict[str, Any]) -> RunnerConfig:
        return replace(self, _start_command=cmd)

    def with_auto_pause(self, enabled: bool = True) -> RunnerConfig:
        return replace(self, _auto_pause=enabled)

    def with_ttl(self, seconds: int) -> RunnerConfig:
        return replace(self, _ttl=seconds)

    def with_tier(self, tier: str) -> RunnerConfig:
        return replace(self, _tier=tier)

    def with_ci_system(self, system: str) -> RunnerConfig:
        return replace(self, _ci_system=system)

    def with_auto_rollout(self, enabled: bool = True) -> RunnerConfig:
        return replace(self, _auto_rollout=enabled)

    def with_session_max_age(self, seconds: int) -> RunnerConfig:
        return replace(self, _session_max_age_seconds=seconds)

    def with_network_policy_preset(self, preset: str) -> RunnerConfig:
        return replace(self, _network_policy_preset=preset)

    def with_network_policy(self, policy: Any) -> RunnerConfig:
        return replace(self, _network_policy=policy)

    # -- Serialization ---------------------------------------------------------

    def to_create_body(self) -> dict[str, Any]:
        """Return a dict suitable for ``LayeredConfigs.create()``."""
        if self._layers is not None:
            layers = [ld.model_dump(exclude_none=True) for ld in self._layers]
        elif self._commands:
            layers = [{"name": "main", "init_commands": self._commands}]
        else:
            layers = []

        body: dict[str, Any] = {
            "display_name": self.display_name,
            "layers": layers,
        }

        if self._base_image is not None:
            body["base_image"] = self._base_image

        config: dict[str, Any] = {}
        if self._auto_pause is not None:
            config["auto_pause"] = self._auto_pause
        if self._ttl is not None:
            config["ttl"] = self._ttl
        if self._tier is not None:
            config["tier"] = self._tier
        if self._ci_system is not None:
            config["ci_system"] = self._ci_system
        if self._auto_rollout is not None:
            config["auto_rollout"] = self._auto_rollout
        if self._session_max_age_seconds is not None:
            config["session_max_age_seconds"] = self._session_max_age_seconds
        if self._network_policy_preset is not None:
            config["network_policy_preset"] = self._network_policy_preset
        if self._network_policy is not None:
            config["network_policy"] = self._network_policy
        if config:
            body["config"] = config

        if self._start_command is not None:
            body["start_command"] = self._start_command

        return body


class RunnerConfigs:
    """High-level declarative runner config management.

    Composes ``LayeredConfigs`` to provide a "declare -> build -> spawn"
    workflow.
    """

    def __init__(self, layered_configs: LayeredConfigs) -> None:
        self._lc = layered_configs

    def apply(self, cfg: RunnerConfig) -> CreateConfigResponse:
        """Register a runner config on the control plane."""
        return self._lc.create(cfg.to_create_body())

    def build(self, config_id: str, *, force: bool = False) -> BuildResponse:
        """Trigger a build for a layered config."""
        return self._lc.build(config_id, force=force)
