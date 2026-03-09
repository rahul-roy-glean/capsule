from __future__ import annotations

import os

from bf_sdk._config import ConnectionConfig


class TestConnectionConfig:
    def test_explicit_values(self) -> None:
        cfg = ConnectionConfig.resolve(base_url="http://example.com", token="token123", timeout=10.0)
        assert cfg.base_url == "http://example.com"
        assert cfg.token == "token123"
        assert cfg.timeout == 10.0
        assert "bf-sdk-python" in cfg.user_agent

    def test_env_fallback(self, monkeypatch: object) -> None:
        import pytest

        mp = pytest.MonkeyPatch()
        mp.setenv("BF_BASE_URL", "http://env-host:9090")
        mp.setenv("BF_TOKEN", "env-token")
        try:
            cfg = ConnectionConfig.resolve()
            assert cfg.base_url == "http://env-host:9090"
            assert cfg.token == "env-token"
        finally:
            mp.undo()

    def test_defaults(self) -> None:
        # Clear env vars if set
        env_backup = {}
        for key in ("BF_BASE_URL", "BF_TOKEN"):
            if key in os.environ:
                env_backup[key] = os.environ.pop(key)
        try:
            cfg = ConnectionConfig.resolve()
            assert cfg.base_url == "http://localhost:8080"
            assert cfg.token is None
            assert cfg.timeout == 30.0
        finally:
            os.environ.update(env_backup)

    def test_trailing_slash_stripped(self) -> None:
        cfg = ConnectionConfig.resolve(base_url="http://example.com/")
        assert cfg.base_url == "http://example.com"
