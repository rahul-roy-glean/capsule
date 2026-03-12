from __future__ import annotations

from dataclasses import dataclass

from capsule_sdk.models.common import CapsuleModel


class WorkloadSummary(CapsuleModel):
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
