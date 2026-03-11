from __future__ import annotations

import pytest

from bf_sdk import BFClient
from bf_sdk.resources.workloads import Workloads


class TestClientSurface:
    def test_workloads_is_primary_high_level_surface(self) -> None:
        client = BFClient(base_url="http://testserver:8080", token="test-token")
        try:
            assert isinstance(client.workloads, Workloads)
        finally:
            client.close()

    def test_layered_configs_is_not_public_surface(self) -> None:
        client = BFClient(base_url="http://testserver:8080", token="test-token")
        try:
            assert not hasattr(client, "layered_configs")
            with pytest.raises(AttributeError):
                _ = client.layered_configs
        finally:
            client.close()
