from __future__ import annotations

from capsule_sdk._http_async import AsyncHttpClient
from capsule_sdk.models.snapshot import Snapshot, SnapshotListResponse


class AsyncSnapshots:
    """Snapshot listing."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self) -> list[Snapshot]:
        data = await self._http.get("/api/v1/snapshots")
        return SnapshotListResponse.model_validate(data).snapshots
