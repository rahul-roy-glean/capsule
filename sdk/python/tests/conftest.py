import pytest


@pytest.fixture
def base_url() -> str:
    return "http://testserver:8080"


@pytest.fixture
def token() -> str:
    return "test-token"
