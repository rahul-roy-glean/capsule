from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.snapshot import Snapshot
from bf_sdk.resources.snapshots import Snapshots


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    return HttpClient(config)


@pytest.fixture
def snapshots(http_client: HttpClient) -> Snapshots:
    return Snapshots(http_client)


class TestSnapshots:
    def test_list_accepts_control_plane_casing(self, snapshots: Snapshots, http_client: HttpClient) -> None:
        resp_data = {
            "snapshots": [
                {
                    "Version": "v1",
                    "Status": "active",
                    "GCSPath": "gs://bucket/v1",
                    "RepoCommit": "abc123",
                    "SizeBytes": 1024,
                    "CreatedAt": "2026-03-09T00:00:00Z",
                    "Metrics": {
                        "avg_analysis_time_ms": 12,
                        "cache_hit_ratio": 0.5,
                        "sample_count": 3,
                    },
                },
            ],
            "count": 1,
            "current_version": "v1",
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = snapshots.list()

        assert len(result) == 1
        snapshot = result[0]
        assert isinstance(snapshot, Snapshot)
        assert snapshot.version == "v1"
        assert snapshot.gcs_path == "gs://bucket/v1"
        assert snapshot.repo_commit == "abc123"
        assert snapshot.size_bytes == 1024
        assert snapshot.metrics is not None
        assert snapshot.metrics.sample_count == 3

    def test_list_still_accepts_snake_case(self, snapshots: Snapshots, http_client: HttpClient) -> None:
        resp_data = {
            "snapshots": [
                {
                    "version": "v2",
                    "status": "ready",
                    "gcs_path": "gs://bucket/v2",
                    "repo_commit": "def456",
                    "size_bytes": 2048,
                    "created_at": "2026-03-09T00:00:00Z",
                },
            ],
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = snapshots.list()

        assert len(result) == 1
        assert result[0].version == "v2"
        assert result[0].repo_commit == "def456"
