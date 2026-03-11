from __future__ import annotations

from dataclasses import dataclass

from bf_sdk.models.common import BFModel


class WorkloadSummary(BFModel):
    """High-level user-facing workload handle."""

    display_name: str
    config_id: str | None = None
    workload_key: str | None = None

    @property
    def name(self) -> str:
        return self.display_name


@dataclass(frozen=True)
class ResolvedWorkloadRef:
    display_name: str | None = None
    config_id: str | None = None
    workload_key: str | None = None
