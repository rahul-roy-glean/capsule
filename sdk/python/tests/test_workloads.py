from __future__ import annotations

from pathlib import Path
from unittest.mock import patch

import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.layered_config import BuildResponse, CreateConfigResponse, StoredLayeredConfig
from bf_sdk.models.runner import AllocateRunnerResponse
from bf_sdk.models.workload import WorkloadSummary
from bf_sdk.resources.layered_configs import LayeredConfigs
from bf_sdk.resources.runners import Runners
from bf_sdk.resources.workloads import Workloads
from bf_sdk.runner_config import RunnerConfig
from bf_sdk.runner_session import RunnerSession


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", token="test-token")
    return HttpClient(config)


@pytest.fixture
def layered_configs(http_client: HttpClient) -> LayeredConfigs:
    return LayeredConfigs(http_client)


@pytest.fixture
def runners(http_client: HttpClient, layered_configs: LayeredConfigs) -> Runners:
    return Runners(http_client, layered_configs=layered_configs)


@pytest.fixture
def workloads(layered_configs: LayeredConfigs, runners: Runners) -> Workloads:
    return Workloads(layered_configs, runners)


class TestWorkloads:
    def test_onboard_runner_config(self, workloads: Workloads, layered_configs: LayeredConfigs) -> None:
        cfg = RunnerConfig("My Sandbox").with_commands(["echo hi"])
        created = CreateConfigResponse(config_id="c1", leaf_workload_key="wk-leaf")
        build = BuildResponse(config_id="c1", status="build_enqueued")
        with patch.object(layered_configs, "create", return_value=created) as create:
            with patch.object(layered_configs, "build", return_value=build) as build_call:
                result = workloads.onboard(cfg)
        assert result.display_name == "My Sandbox"
        assert result.config_id == "c1"
        assert result.workload_key == "wk-leaf"
        create.assert_called_once()
        build_call.assert_called_once_with("c1", force=False, clean=False)

    def test_onboard_yaml_string(self, workloads: Workloads, layered_configs: LayeredConfigs) -> None:
        yaml_spec = """
platform:
  region: us-central1
workload:
  display_name: yaml-sandbox
  base_image: ubuntu:22.04
  layers:
    - name: main
      init_commands:
        - type: shell
          args: ["bash", "-lc", "echo hi"]
"""
        created = CreateConfigResponse(config_id="c1", leaf_workload_key="wk-leaf")
        with patch.object(layered_configs, "create", return_value=created) as create:
            with patch.object(layered_configs, "build", return_value=BuildResponse(config_id="c1")):
                result = workloads.onboard_yaml(yaml_spec)
        assert result.display_name == "yaml-sandbox"
        create_body = create.call_args.args[0]
        assert create_body["display_name"] == "yaml-sandbox"
        assert "platform" not in create_body

    def test_onboard_yaml_path_uses_explicit_name(
        self,
        workloads: Workloads,
        layered_configs: LayeredConfigs,
        tmp_path: Path,
    ) -> None:
        yaml_path = tmp_path / "onboard.yaml"
        yaml_path.write_text(
            """
workload:
  base_image: ubuntu:22.04
  layers:
    - name: main
      init_commands:
        - type: shell
          args: ["bash", "-lc", "echo hi"]
"""
        )
        created = CreateConfigResponse(config_id="c1", leaf_workload_key="wk-leaf")
        with patch.object(layered_configs, "create", return_value=created) as create:
            with patch.object(layered_configs, "build", return_value=BuildResponse(config_id="c1")):
                result = workloads.onboard_yaml(str(yaml_path), name="afs-sandbox")
        assert result.display_name == "afs-sandbox"
        assert create.call_args.args[0]["display_name"] == "afs-sandbox"

    def test_list(self, workloads: Workloads, layered_configs: LayeredConfigs) -> None:
        configs = [StoredLayeredConfig(config_id="c1", display_name="My Sandbox", leaf_workload_key="wk-leaf")]
        with patch.object(layered_configs, "list", return_value=configs):
            result = workloads.list()
        assert result == [WorkloadSummary(display_name="My Sandbox", config_id="c1", workload_key="wk-leaf")]

    def test_get_by_name(self, workloads: Workloads, layered_configs: LayeredConfigs) -> None:
        configs = [StoredLayeredConfig(config_id="c1", display_name="My Sandbox", leaf_workload_key="wk-leaf")]
        with patch.object(layered_configs, "list", return_value=configs):
            result = workloads.get("My Sandbox")
        assert result.display_name == "My Sandbox"

    def test_build_by_name(self, workloads: Workloads, layered_configs: LayeredConfigs) -> None:
        configs = [StoredLayeredConfig(config_id="c1", display_name="My Sandbox", leaf_workload_key="wk-leaf")]
        build = BuildResponse(config_id="c1", status="build_enqueued")
        with patch.object(layered_configs, "list", return_value=configs):
            with patch.object(layered_configs, "build", return_value=build) as build_call:
                result = workloads.build("My Sandbox", clean=True)
        assert result.status == "build_enqueued"
        build_call.assert_called_once_with("c1", force=False, clean=True)

    def test_delete_by_name(self, workloads: Workloads, layered_configs: LayeredConfigs) -> None:
        configs = [StoredLayeredConfig(config_id="c1", display_name="My Sandbox", leaf_workload_key="wk-leaf")]
        with patch.object(layered_configs, "list", return_value=configs):
            with patch.object(layered_configs, "delete") as delete:
                workloads.delete("My Sandbox")
        delete.assert_called_once_with("c1")

    def test_start_delegates_to_runners(self, workloads: Workloads, runners: Runners) -> None:
        session = RunnerSession(runners, "r-1")
        with patch.object(runners, "from_config", return_value=session) as start:
            result = workloads.start("My Sandbox", poll_interval=1.0)
        assert result is session
        start.assert_called_once_with("My Sandbox", poll_interval=1.0)

    def test_allocate_delegates_to_runners(self, workloads: Workloads, runners: Runners) -> None:
        allocation = AllocateRunnerResponse(runner_id="r-1", request_id="req-1")
        with patch.object(runners, "allocate", return_value=allocation) as allocate:
            result = workloads.allocate("My Sandbox", startup_timeout=10.0)
        assert result is allocation
        allocate.assert_called_once_with("My Sandbox", startup_timeout=10.0)
