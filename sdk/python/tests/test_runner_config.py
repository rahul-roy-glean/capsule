from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from capsule_sdk._config import ConnectionConfig
from capsule_sdk._http import HttpClient
from capsule_sdk.models.layered_config import BuildResponse, CreateConfigResponse, LayerDef
from capsule_sdk.resources.layered_configs import LayeredConfigs
from capsule_sdk.runner_config import RunnerConfig, RunnerConfigs


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
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
            RunnerConfig("my-sandbox")
            .with_base_image("ubuntu:22.04")
            .with_commands(["pip install -e .", "pytest -q"])
            .with_tier("m")
            .with_ttl(3600)
            .with_auto_pause(True)
            .with_auto_rollout(True)
            .with_network_policy_preset("restricted-egress")
        )
        assert cfg.display_name == "my-sandbox"
        assert cfg._base_image == "ubuntu:22.04"
        assert len(cfg._commands) == 2
        assert cfg._commands[0] == {"type": "shell", "args": ["bash", "-lc", "pip install -e ."]}
        assert cfg._tier == "m"
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
            RunnerConfig("sandbox")
            .with_base_image("ubuntu:22.04")
            .with_commands(["echo hi"])
            .with_tier("s")
            .with_ttl(1800)
            .with_auto_pause(True)
        )
        body = cfg.to_create_body()
        assert body["display_name"] == "sandbox"
        assert body["base_image"] == "ubuntu:22.04"
        assert body["layers"] == [
            {"name": "main", "init_commands": [{"type": "shell", "args": ["bash", "-lc", "echo hi"]}]},
        ]
        assert body["config"]["tier"] == "s"
        assert body["config"]["ttl"] == 1800
        assert body["config"]["auto_pause"] is True

    def test_to_create_body_no_config(self) -> None:
        cfg = RunnerConfig("bare-cfg").with_commands(["echo hi"])
        body = cfg.to_create_body()
        assert "config" not in body
        assert body["layers"] == [
            {"name": "main", "init_commands": [{"type": "shell", "args": ["bash", "-lc", "echo hi"]}]},
        ]

    def test_to_create_body_with_explicit_layers(self) -> None:
        layers = [
            LayerDef(name="deps", init_commands=[{"type": "shell", "args": ["bash", "-lc", "apt install -y curl"]}]),
            LayerDef(name="app", init_commands=[{"type": "shell", "args": ["bash", "-lc", "pip install ."]}]),
        ]
        cfg = RunnerConfig("multi-layer").with_layers(layers).with_tier("l")
        body = cfg.to_create_body()
        assert len(body["layers"]) == 2
        assert body["layers"][0]["name"] == "deps"
        assert body["layers"][1]["name"] == "app"

    def test_with_legacy_command_dicts(self) -> None:
        cfg = RunnerConfig("wk-1").with_commands([{"command": "echo hi", "run_as_root": True}])
        assert cfg._commands == [{"type": "shell", "args": ["bash", "-lc", "echo hi"], "run_as_root": True}]

    def test_with_extended_config_fields(self) -> None:
        cfg = (
            RunnerConfig("wk-1")
            .with_commands(["echo hi"])
            .with_rootfs_size_gb(16)
            .with_runner_user("sandbox")
            .with_workspace_size_gb(100)
            .with_auth({"type": "delegated"})
        )
        body = cfg.to_create_body()
        assert body["config"]["rootfs_size_gb"] == 16
        assert body["config"]["runner_user"] == "sandbox"
        assert body["config"]["workspace_size_gb"] == 100
        assert body["config"]["auth"] == {"type": "delegated"}

    def test_auto_rollout(self) -> None:
        cfg = RunnerConfig("wk-1").with_commands(["echo"]).with_auto_rollout(True)
        body = cfg.to_create_body()
        assert body["config"]["auto_rollout"] is True

    def test_invalid_config_id_rejected(self) -> None:
        with pytest.raises(ValueError, match="config_id"):
            RunnerConfig("My Sandbox").to_create_body()

    def test_invalid_config_id_too_short(self) -> None:
        with pytest.raises(ValueError, match="config_id"):
            RunnerConfig("ab").to_create_body()

    def test_invalid_config_id_uppercase(self) -> None:
        with pytest.raises(ValueError, match="config_id"):
            RunnerConfig("MyWorkload").to_create_body()


class TestRunnerConfigs:
    def test_apply(self, runner_configs: RunnerConfigs, http_client: HttpClient) -> None:
        resp_data = {"config_id": "abc123", "leaf_workload_key": "wk-leaf"}
        mock_resp = httpx.Response(201, json=resp_data)
        cfg = RunnerConfig("sandbox").with_commands(["echo hi"])
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

    def test_build_clean(self, runner_configs: RunnerConfigs, http_client: HttpClient) -> None:
        build_data = {"config_id": "c1", "status": "build_enqueued", "clean": "true"}
        mock_resp = httpx.Response(202, json=build_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = runner_configs.build("c1", clean=True)
        assert isinstance(result, BuildResponse)
        assert result.clean == "true"
