import pytest


@pytest.fixture
def base_url() -> str:
    return "http://testserver:8080"


@pytest.fixture
def api_key() -> str:
    return "test-api-key"
