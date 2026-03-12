from __future__ import annotations

from typing import Any

from pydantic import BaseModel, ConfigDict


class CapsuleModel(BaseModel):
    """Base model for all capsule-sdk types."""

    model_config = ConfigDict(extra="ignore", frozen=True)

    def __getitem__(self, key: str) -> Any:
        try:
            return getattr(self, key)
        except AttributeError as exc:  # pragma: no cover - defensive
            raise KeyError(key) from exc

    def get(self, key: str, default: Any = None) -> Any:
        return getattr(self, key, default)

    def items(self):
        return self.model_dump().items()


class Resources(CapsuleModel):
    """Resource requirements for a runner."""

    cpu: int | None = None
    memory_mb: int | None = None
    disk_gb: int | None = None
