from __future__ import annotations

from dataclasses import dataclass, field, replace
from typing import Any

from bf_sdk.models.snapshot import BuildResult, SnapshotConfig, SnapshotTag
from bf_sdk.resources.snapshot_configs import SnapshotConfigs


@dataclass(frozen=True)
class Template:
    """Declarative sandbox template definition.

    A Template is a pure-data description of a desired runner shape.
    It maps 1:1 to a SnapshotConfig on the server, with fluent builder
    methods for ergonomic construction.

    Usage::

        tpl = (
            Template("my-workload")
            .with_display_name("My sandbox")
            .with_commands(["pip install -e .[dev]"])
            .with_tier("small")
            .with_auto_pause(True)
        )
    """

    workload_key: str
    _display_name: str | None = field(default=None, repr=False)
    _commands: list[dict[str, Any]] = field(default_factory=lambda: list[dict[str, Any]](), repr=False)
    _incremental_commands: list[dict[str, Any]] | None = field(default=None, repr=False)
    _build_schedule: str | None = field(default=None, repr=False)
    _max_concurrent_runners: int | None = field(default=None, repr=False)
    _ci_system: str | None = field(default=None, repr=False)
    _start_command: dict[str, Any] | None = field(default=None, repr=False)
    _runner_ttl_seconds: int | None = field(default=None, repr=False)
    _session_max_age_seconds: int | None = field(default=None, repr=False)
    _auto_pause: bool | None = field(default=None, repr=False)
    _tier: str | None = field(default=None, repr=False)
    _network_policy_preset: str | None = field(default=None, repr=False)
    _network_policy: Any | None = field(default=None, repr=False)
    _labels: dict[str, str] = field(default_factory=lambda: dict[str, str](), repr=False)

    # -- Fluent withers (return new immutable copy) ----------------------------

    def with_display_name(self, name: str) -> Template:
        return replace(self, _display_name=name)

    def with_commands(self, cmds: list[str | dict[str, Any]]) -> Template:
        normalized = [{"command": c} if isinstance(c, str) else c for c in cmds]
        return replace(self, _commands=normalized)

    def with_incremental_commands(self, cmds: list[str | dict[str, Any]]) -> Template:
        normalized = [{"command": c} if isinstance(c, str) else c for c in cmds]
        return replace(self, _incremental_commands=normalized)

    def with_build_schedule(self, schedule: str) -> Template:
        return replace(self, _build_schedule=schedule)

    def with_max_concurrent_runners(self, n: int) -> Template:
        return replace(self, _max_concurrent_runners=n)

    def with_ci_system(self, system: str) -> Template:
        return replace(self, _ci_system=system)

    def with_start_command(self, cmd: dict[str, Any]) -> Template:
        return replace(self, _start_command=cmd)

    def with_runner_ttl(self, seconds: int) -> Template:
        return replace(self, _runner_ttl_seconds=seconds)

    def with_session_max_age(self, seconds: int) -> Template:
        return replace(self, _session_max_age_seconds=seconds)

    def with_auto_pause(self, enabled: bool = True) -> Template:
        return replace(self, _auto_pause=enabled)

    def with_tier(self, tier: str) -> Template:
        return replace(self, _tier=tier)

    def with_network_policy_preset(self, preset: str) -> Template:
        return replace(self, _network_policy_preset=preset)

    def with_network_policy(self, policy: Any) -> Template:
        return replace(self, _network_policy=policy)

    def with_labels(self, labels: dict[str, str]) -> Template:
        return replace(self, _labels=labels)

    # -- Serialization ---------------------------------------------------------

    def to_create_kwargs(self) -> dict[str, Any]:
        """Return kwargs suitable for ``SnapshotConfigs.create()``."""
        kw: dict[str, Any] = {
            "display_name": self._display_name or self.workload_key,
            "commands": self._commands,
        }
        if self._incremental_commands is not None:
            kw["incremental_commands"] = self._incremental_commands
        if self._build_schedule is not None:
            kw["build_schedule"] = self._build_schedule
        if self._max_concurrent_runners is not None:
            kw["max_concurrent_runners"] = self._max_concurrent_runners
        if self._ci_system is not None:
            kw["ci_system"] = self._ci_system
        if self._start_command is not None:
            kw["start_command"] = self._start_command
        if self._runner_ttl_seconds is not None:
            kw["runner_ttl_seconds"] = self._runner_ttl_seconds
        if self._session_max_age_seconds is not None:
            kw["session_max_age_seconds"] = self._session_max_age_seconds
        if self._auto_pause is not None:
            kw["auto_pause"] = self._auto_pause
        if self._tier is not None:
            kw["tier"] = self._tier
        if self._network_policy_preset is not None:
            kw["network_policy_preset"] = self._network_policy_preset
        if self._network_policy is not None:
            kw["network_policy"] = self._network_policy
        return kw


class Templates:
    """High-level declarative template management.

    Composes ``SnapshotConfigs`` to provide a "declare → build → tag → spawn"
    workflow similar to E2B's template DSL.
    """

    def __init__(self, snapshot_configs: SnapshotConfigs) -> None:
        self._sc = snapshot_configs

    def apply(self, tpl: Template) -> SnapshotConfig:
        """Upsert a template's SnapshotConfig on the control plane."""
        return self._sc.create(**tpl.to_create_kwargs())

    def build(
        self,
        tpl: Template | str,
        *,
        tag: str | None = None,
        incremental: bool = False,
    ) -> BuildResult:
        """Trigger a snapshot build, optionally tagging the resulting version."""
        wk = tpl.workload_key if isinstance(tpl, Template) else tpl
        result = self._sc.trigger_build(wk, incremental=incremental)
        if tag:
            self._sc.create_tag(wk, tag=tag, version=result.version)
        return result

    def promote(
        self,
        tpl: Template | str,
        *,
        tag: str,
        to: str = "stable",
    ) -> SnapshotTag:
        """Promote a tag by copying its version to a target tag.

        E.g. ``promote(tpl, tag="dev", to="stable")`` resolves the ``dev``
        tag's version and creates/updates the ``stable`` tag to point to it.
        """
        wk = tpl.workload_key if isinstance(tpl, Template) else tpl
        source = self._sc.get_tag(wk, tag)
        return self._sc.create_tag(wk, tag=to, version=source.version)

    def get(self, tpl: Template | str) -> SnapshotConfig:
        """Get the current SnapshotConfig for a template."""
        wk = tpl.workload_key if isinstance(tpl, Template) else tpl
        return self._sc.get(wk)

    def list_tags(self, tpl: Template | str) -> list[SnapshotTag]:
        """List all tags for a template."""
        wk = tpl.workload_key if isinstance(tpl, Template) else tpl
        return self._sc.list_tags(wk)
