from __future__ import annotations

import asyncio
from unittest.mock import AsyncMock, patch

import httpx
import pytest

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._http_async import AsyncHttpClient
from capsule_sdk.models.snapshot import Snapshot
from capsule_sdk.resources.async_snapshots import AsyncSnapshots


@pytest.fixture
def http_client() -> AsyncHttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    client = AsyncHttpClient(config)
    yield client
    asyncio.run(client.close())


@pytest.fixture
def snapshots(http_client: AsyncHttpClient) -> AsyncSnapshots:
    return AsyncSnapshots(http_client)


class TestAsyncSnapshots:
    def test_list_accepts_control_plane_casing(self, snapshots: AsyncSnapshots, http_client: AsyncHttpClient) -> None:
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

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)):
                result = await snapshots.list()

            assert len(result) == 1
            snapshot = result[0]
            assert isinstance(snapshot, Snapshot)
            assert snapshot.version == "v1"
            assert snapshot.gcs_path == "gs://bucket/v1"
            assert snapshot.repo_commit == "abc123"
            assert snapshot.size_bytes == 1024
            assert snapshot.metrics is not None
            assert snapshot.metrics.sample_count == 3

        asyncio.run(run())

    def test_list_still_accepts_snake_case(self, snapshots: AsyncSnapshots, http_client: AsyncHttpClient) -> None:
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

        async def run() -> None:
            with patch.object(http_client._client, "request", AsyncMock(return_value=mock_resp)):
                result = await snapshots.list()

            assert len(result) == 1
            assert result[0].version == "v2"
            assert result[0].repo_commit == "def456"

        asyncio.run(run())
