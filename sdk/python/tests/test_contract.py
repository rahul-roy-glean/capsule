from __future__ import annotations

import os
import uuid
from contextlib import suppress

import pytest

from bf_sdk import BFAuthError, BFClient, BFHTTPError, BFNotFound


@pytest.fixture
def base_url() -> str:
    return os.environ.get("BF_BASE_URL", "http://localhost:8080").rstrip("/")


@pytest.fixture
def token() -> str:
    return os.environ.get("BF_TOKEN", "test-token")


@pytest.fixture
def client(base_url: str, token: str) -> BFClient:
    with BFClient(base_url=base_url, token=token) as bf:
        yield bf


@pytest.fixture
def unauthenticated_client(base_url: str) -> BFClient:
    with BFClient(base_url=base_url, token="") as bf:
        yield bf


def _layered_config_body(name: str) -> dict[str, object]:
    return {
        "display_name": name,
        "base_image": "ubuntu:22.04",
        "layers": [
            {
                "name": "workspace",
                "init_commands": [
                    {
                        "type": "shell",
                        "args": ["bash", "-lc", f"echo {name} > /workspace/{name}.txt"],
                    }
                ],
            }
        ],
        "config": {
            "tier": "m",
            "auto_rollout": False,
        },
    }


@pytest.mark.contract
class TestContract:
    def test_auth_required(self, unauthenticated_client: BFClient) -> None:
        with pytest.raises(BFAuthError):
            unauthenticated_client.runners.list()

    def test_auth_works(self, client: BFClient) -> None:
        runners = client.runners.list()
        assert isinstance(runners, list)

    def test_layered_config_crud(self, client: BFClient) -> None:
        name = f"contract-{uuid.uuid4().hex[:8]}"
        created = client.layered_configs.create(_layered_config_body(name))

        config_id = created.config_id
        assert config_id
        assert created.leaf_workload_key

        try:
            listed_ids = {cfg.config_id for cfg in client.layered_configs.list()}
            assert config_id in listed_ids

            detail = client.layered_configs.get(config_id)
            assert detail.config.config_id == config_id
            assert detail.config.display_name == name
            assert detail.layers
            assert detail.layers[0].name
        finally:
            with suppress(Exception):
                client.layered_configs.delete(config_id)

        with pytest.raises(BFNotFound):
            client.layered_configs.get(config_id)

    def test_invalid_layered_config_returns_http_400(self, client: BFClient) -> None:
        with pytest.raises(BFHTTPError) as exc:
            client.layered_configs.create({"display_name": "invalid", "layers": []})

        assert exc.value.status_code == 400
        assert "at least one layer is required" in exc.value.message
