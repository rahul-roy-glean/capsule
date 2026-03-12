from __future__ import annotations

import os

from capsule_sdk._config import ConnectionConfig


class TestConnectionConfig:
    def test_explicit_values(self) -> None:
        cfg = ConnectionConfig.resolve(
            base_url="http://example.com",
            token="token123",
            request_timeout=10.0,
            startup_timeout=50.0,
            operation_timeout=90.0,
        )
        assert cfg.base_url == "http://example.com"
        assert cfg.token == "token123"
        assert cfg.timeout == 10.0
        assert cfg.request_timeout == 10.0
        assert cfg.startup_timeout == 50.0
        assert cfg.operation_timeout == 90.0
        assert "capsule-sdk-python" in cfg.user_agent

    def test_env_fallback(self, monkeypatch: object) -> None:
        import pytest

        mp = pytest.MonkeyPatch()
        mp.setenv("CAPSULE_BASE_URL", "http://env-host:9090")
        mp.setenv("CAPSULE_TOKEN", "env-token")
        try:
            cfg = ConnectionConfig.resolve()
            assert cfg.base_url == "http://env-host:9090"
            assert cfg.token == "env-token"
        finally:
            mp.undo()

    def test_defaults(self) -> None:
        # Clear env vars if set
        env_backup = {}
        for key in ("CAPSULE_BASE_URL", "CAPSULE_TOKEN"):
            if key in os.environ:
                env_backup[key] = os.environ.pop(key)
        try:
            cfg = ConnectionConfig.resolve()
            assert cfg.base_url == "http://localhost:8080"
            assert cfg.token is None
            assert cfg.timeout == 30.0
            assert cfg.startup_timeout == 45.0
            assert cfg.operation_timeout == 120.0
        finally:
            os.environ.update(env_backup)

    def test_trailing_slash_stripped(self) -> None:
        cfg = ConnectionConfig.resolve(base_url="http://example.com/")
        assert cfg.base_url == "http://example.com"
