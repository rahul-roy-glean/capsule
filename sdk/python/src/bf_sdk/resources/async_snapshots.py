from __future__ import annotations

from bf_sdk._http_async import AsyncHttpClient
from bf_sdk.models.snapshot import Snapshot, SnapshotListResponse


class AsyncSnapshots:
    """Snapshot listing."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self) -> list[Snapshot]:
        data = await self._http.get("/api/v1/snapshots")
        return SnapshotListResponse.model_validate(data).snapshots
