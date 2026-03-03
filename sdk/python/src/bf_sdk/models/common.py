from __future__ import annotations

from pydantic import BaseModel, ConfigDict


class BFModel(BaseModel):
    """Base model for all bf-sdk types."""

    model_config = ConfigDict(extra="ignore", frozen=True)


class Resources(BFModel):
    """Resource requirements for a runner."""

    cpu: int | None = None
    memory_mb: int | None = None
    disk_gb: int | None = None
