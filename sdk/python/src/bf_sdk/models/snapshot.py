from __future__ import annotations

from bf_sdk.models.common import BFModel


class Snapshot(BFModel):
    """A snapshot version."""

    version: str | None = None
    status: str | None = None
    gcs_path: str | None = None
    size_bytes: int | None = None
    created_at: str | None = None
