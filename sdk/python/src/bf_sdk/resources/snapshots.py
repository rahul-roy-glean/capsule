from __future__ import annotations

from bf_sdk._http import HttpClient
from bf_sdk.models.snapshot import Snapshot


class Snapshots:
    """Snapshot listing."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> list[Snapshot]:
        data = self._http.get("/api/v1/snapshots")
        return [Snapshot.model_validate(s) for s in data.get("snapshots", [])]
