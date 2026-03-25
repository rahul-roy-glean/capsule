from __future__ import annotations

from capsule_sdk._http_async import AsyncHttpClient
from capsule_sdk.models.host import Host, HostListResponse


class AsyncHosts:
    """Read-only access to the host fleet registered with the control plane."""

    def __init__(self, http: AsyncHttpClient) -> None:
        self._http = http

    async def list(self) -> list[Host]:
        """List all hosts known to the control plane."""
        data = await self._http.get("/api/v1/hosts")
        return HostListResponse.model_validate(data).hosts
