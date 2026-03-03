from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.layered_config import BuildResponse, CreateConfigResponse, LayerDef
from bf_sdk.resources.layered_configs import LayeredConfigs
from bf_sdk.runner_config import RunnerConfig, RunnerConfigs


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", api_key="test")
    return HttpClient(config)


@pytest.fixture
def layered_configs(http_client: HttpClient) -> LayeredConfigs:
    return LayeredConfigs(http_client)


@pytest.fixture
def runner_configs(layered_configs: LayeredConfigs) -> RunnerConfigs:
    return RunnerConfigs(layered_configs)


class TestRunnerConfig:
    def test_basic_construction(self) -> None:
        cfg = RunnerConfig("my-workload")
        assert cfg.display_name == "my-workload"

    def test_fluent_builder(self) -> None:
        cfg = (
            RunnerConfig("My Sandbox")
            .with_base_image("ubuntu:22.04")
            .with_commands(["pip install -e .", "pytest -q"])
            .with_tier("small")
            .with_ci_system("github-actions")
            .with_ttl(3600)
            .with_auto_pause(True)
            .with_auto_rollout(True)
            .with_network_policy_preset("default")
        )
        assert cfg.display_name == "My Sandbox"
        assert cfg._base_image == "ubuntu:22.04"
        assert len(cfg._commands) == 2
        assert cfg._commands[0] == {"command": "pip install -e ."}
        assert cfg._tier == "small"
        assert cfg._auto_pause is True
        assert cfg._auto_rollout is True

    def test_immutability(self) -> None:
        cfg1 = RunnerConfig("wk-1")
        cfg2 = cfg1.with_tier("large")
        assert cfg1._tier is None
        assert cfg2._tier == "large"
        assert cfg1 is not cfg2

    def test_to_create_body_simple(self) -> None:
        cfg = (
            RunnerConfig("Sandbox")
            .with_base_image("ubuntu:22.04")
            .with_commands(["echo hi"])
            .with_tier("small")
            .with_ttl(1800)
            .with_auto_pause(True)
        )
        body = cfg.to_create_body()
        assert body["display_name"] == "Sandbox"
        assert body["base_image"] == "ubuntu:22.04"
        assert body["layers"] == [{"name": "main", "init_commands": [{"command": "echo hi"}]}]
        assert body["config"]["tier"] == "small"
        assert body["config"]["ttl"] == 1800
        assert body["config"]["auto_pause"] is True

    def test_to_create_body_no_config(self) -> None:
        cfg = RunnerConfig("bare").with_commands(["echo hi"])
        body = cfg.to_create_body()
        assert "config" not in body
        assert body["layers"] == [{"name": "main", "init_commands": [{"command": "echo hi"}]}]

    def test_to_create_body_with_explicit_layers(self) -> None:
        layers = [
            LayerDef(name="deps", init_commands=[{"command": "apt install -y curl"}]),
            LayerDef(name="app", init_commands=[{"command": "pip install ."}]),
        ]
        cfg = RunnerConfig("multi").with_layers(layers).with_tier("large")
        body = cfg.to_create_body()
        assert len(body["layers"]) == 2
        assert body["layers"][0]["name"] == "deps"
        assert body["layers"][1]["name"] == "app"

    def test_with_dict_commands(self) -> None:
        cfg = RunnerConfig("wk-1").with_commands([{"command": "echo hi", "timeout": 30}])
        assert cfg._commands == [{"command": "echo hi", "timeout": 30}]

    def test_auto_rollout(self) -> None:
        cfg = RunnerConfig("wk-1").with_commands(["echo"]).with_auto_rollout(True)
        body = cfg.to_create_body()
        assert body["config"]["auto_rollout"] is True


class TestRunnerConfigs:
    def test_apply(self, runner_configs: RunnerConfigs, http_client: HttpClient) -> None:
        resp_data = {"config_id": "abc123", "leaf_workload_key": "wk-leaf"}
        mock_resp = httpx.Response(201, json=resp_data)
        cfg = RunnerConfig("Sandbox").with_commands(["echo hi"])
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runner_configs.apply(cfg)
        assert isinstance(result, CreateConfigResponse)
        assert result.config_id == "abc123"

    def test_build(self, runner_configs: RunnerConfigs, http_client: HttpClient) -> None:
        build_data = {"config_id": "c1", "status": "build_enqueued", "force": "false"}
        mock_resp = httpx.Response(202, json=build_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runner_configs.build("c1")
        assert isinstance(result, BuildResponse)
        assert result.status == "build_enqueued"

    def test_build_force(self, runner_configs: RunnerConfigs, http_client: HttpClient) -> None:
        build_data = {"config_id": "c1", "status": "build_enqueued", "force": "true"}
        mock_resp = httpx.Response(202, json=build_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runner_configs.build("c1", force=True)
        assert isinstance(result, BuildResponse)
        assert result.force == "true"
