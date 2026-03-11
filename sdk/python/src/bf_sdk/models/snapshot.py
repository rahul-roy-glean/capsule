from __future__ import annotations

from pydantic import AliasChoices, Field

from bf_sdk.models.common import BFModel


def _empty_snapshots() -> list[Snapshot]:
    return []


class SnapshotMetrics(BFModel):
    """Performance metadata recorded for a snapshot."""

    avg_analysis_time_ms: int | None = None
    cache_hit_ratio: float | None = None
    sample_count: int | None = None


class Snapshot(BFModel):
    """A snapshot version."""

    version: str | None = Field(default=None, validation_alias=AliasChoices("version", "Version"))
    status: str | None = Field(default=None, validation_alias=AliasChoices("status", "Status"))
    gcs_path: str | None = Field(default=None, validation_alias=AliasChoices("gcs_path", "GCSPath"))
    repo_commit: str | None = Field(default=None, validation_alias=AliasChoices("repo_commit", "RepoCommit"))
    size_bytes: int | None = Field(default=None, validation_alias=AliasChoices("size_bytes", "SizeBytes"))
    created_at: str | None = Field(default=None, validation_alias=AliasChoices("created_at", "CreatedAt"))
    metrics: SnapshotMetrics | None = Field(default=None, validation_alias=AliasChoices("metrics", "Metrics"))


class SnapshotListResponse(BFModel):
    """Response from listing snapshots."""

    snapshots: list[Snapshot] = Field(default_factory=_empty_snapshots)
    count: int | None = None
    current_version: str | None = None
