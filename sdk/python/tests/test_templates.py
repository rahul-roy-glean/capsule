from __future__ import annotations

from unittest.mock import patch

import httpx
import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.models.snapshot import BuildResult, SnapshotConfig, SnapshotTag
from bf_sdk.resources.snapshot_configs import SnapshotConfigs
from bf_sdk.templates import Template, Templates


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", api_key="test")
    return HttpClient(config)


@pytest.fixture
def snapshot_configs(http_client: HttpClient) -> SnapshotConfigs:
    return SnapshotConfigs(http_client)


@pytest.fixture
def templates(snapshot_configs: SnapshotConfigs) -> Templates:
    return Templates(snapshot_configs)


class TestTemplate:
    def test_basic_construction(self) -> None:
        tpl = Template("my-workload")
        assert tpl.workload_key == "my-workload"

    def test_fluent_builder(self) -> None:
        tpl = (
            Template("wk-1")
            .with_display_name("My Sandbox")
            .with_commands(["pip install -e .", "pytest -q"])
            .with_tier("small")
            .with_ci_system("github-actions")
            .with_runner_ttl(3600)
            .with_auto_pause(True)
            .with_network_policy_preset("default")
            .with_labels({"team": "devprod"})
        )
        assert tpl.workload_key == "wk-1"
        assert tpl._display_name == "My Sandbox"
        assert len(tpl._commands) == 2
        # Commands should be normalized to dicts
        assert tpl._commands[0] == {"command": "pip install -e ."}
        assert tpl._tier == "small"
        assert tpl._auto_pause is True
        assert tpl._labels == {"team": "devprod"}

    def test_immutability(self) -> None:
        tpl1 = Template("wk-1")
        tpl2 = tpl1.with_tier("large")
        assert tpl1._tier is None
        assert tpl2._tier == "large"
        assert tpl1 is not tpl2

    def test_to_create_kwargs(self) -> None:
        tpl = (
            Template("wk-1")
            .with_display_name("Sandbox")
            .with_commands(["echo hi"])
            .with_tier("small")
            .with_runner_ttl(1800)
            .with_auto_pause(True)
        )
        kw = tpl.to_create_kwargs()
        assert kw["display_name"] == "Sandbox"
        assert kw["commands"] == [{"command": "echo hi"}]
        assert kw["tier"] == "small"
        assert kw["runner_ttl_seconds"] == 1800
        assert kw["auto_pause"] is True
        # Unset fields should not be present
        assert "ci_system" not in kw
        assert "network_policy_preset" not in kw

    def test_to_create_kwargs_default_display_name(self) -> None:
        tpl = Template("wk-1").with_commands(["echo hi"])
        kw = tpl.to_create_kwargs()
        assert kw["display_name"] == "wk-1"

    def test_with_dict_commands(self) -> None:
        tpl = Template("wk-1").with_commands([{"command": "echo hi", "timeout": 30}])
        assert tpl._commands == [{"command": "echo hi", "timeout": 30}]

    def test_with_incremental_commands(self) -> None:
        tpl = Template("wk-1").with_commands(["base"]).with_incremental_commands(["update"])
        kw = tpl.to_create_kwargs()
        assert kw["incremental_commands"] == [{"command": "update"}]


class TestTemplates:
    def test_apply(self, templates: Templates, http_client: HttpClient) -> None:
        resp_data = {"workload_key": "wk-abc", "display_name": "Sandbox"}
        mock_resp = httpx.Response(201, json=resp_data)
        tpl = Template("wk-abc").with_display_name("Sandbox").with_commands(["echo hi"])
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = templates.apply(tpl)
        assert isinstance(result, SnapshotConfig)
        assert result.workload_key == "wk-abc"

    def test_build_without_tag(self, templates: Templates, http_client: HttpClient) -> None:
        build_data = {"workload_key": "wk-1", "version": "v5", "status": "building"}
        mock_resp = httpx.Response(202, json=build_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = templates.build("wk-1")
        assert isinstance(result, BuildResult)
        assert result.version == "v5"

    def test_build_with_tag(self, templates: Templates, http_client: HttpClient) -> None:
        build_data = {"workload_key": "wk-1", "version": "v5", "status": "building"}
        tag_data = {"tag": "dev", "workload_key": "wk-1", "version": "v5"}
        mock_build = httpx.Response(202, json=build_data)
        mock_tag = httpx.Response(201, json=tag_data)
        with patch.object(http_client._client, "request", side_effect=[mock_build, mock_tag]):
            result = templates.build("wk-1", tag="dev")
        assert result.version == "v5"

    def test_build_with_template_object(self, templates: Templates, http_client: HttpClient) -> None:
        build_data = {"workload_key": "wk-1", "version": "v3", "status": "building"}
        mock_resp = httpx.Response(202, json=build_data)
        tpl = Template("wk-1")
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = templates.build(tpl)
        assert result.version == "v3"

    def test_promote(self, templates: Templates, http_client: HttpClient) -> None:
        source_tag = {"tag": "dev", "workload_key": "wk-1", "version": "v5"}
        created_tag = {"tag": "stable", "workload_key": "wk-1", "version": "v5"}
        mock_get = httpx.Response(200, json=source_tag)
        mock_create = httpx.Response(201, json=created_tag)
        with patch.object(http_client._client, "request", side_effect=[mock_get, mock_create]):
            result = templates.promote("wk-1", tag="dev", to="stable")
        assert isinstance(result, SnapshotTag)
        assert result.tag == "stable"
        assert result.version == "v5"

    def test_get(self, templates: Templates, http_client: HttpClient) -> None:
        resp_data = {"workload_key": "wk-1", "display_name": "Config"}
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = templates.get("wk-1")
        assert result.workload_key == "wk-1"

    def test_list_tags(self, templates: Templates, http_client: HttpClient) -> None:
        resp_data = {
            "tags": [
                {"tag": "stable", "workload_key": "wk-1", "version": "v1"},
                {"tag": "dev", "workload_key": "wk-1", "version": "v2"},
            ],
            "count": 2,
        }
        mock_resp = httpx.Response(200, json=resp_data)
        with patch.object(http_client._client, "request", return_value=mock_resp):
            result = templates.list_tags("wk-1")
        assert len(result) == 2
