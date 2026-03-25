from __future__ import annotations

from capsule_sdk._http import HttpClient
from capsule_sdk.models.host import Host, HostListResponse


class Hosts:
    """Read-only access to the host fleet registered with the control plane."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> list[Host]:
        """List all hosts known to the control plane."""
        data = self._http.get("/api/v1/hosts")
        return HostListResponse.model_validate(data).hosts
