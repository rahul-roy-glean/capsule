from __future__ import annotations

from unittest.mock import patch

import pytest

from bf_sdk._config import ConnectionConfig
from bf_sdk._http import HttpClient
from bf_sdk.resources.runners import Runners
from bf_sdk.runner_session import RunnerSession


@pytest.fixture
def http_client() -> HttpClient:
    config = ConnectionConfig.resolve(base_url="http://testserver:8080", api_key="test")
    return HttpClient(config)


@pytest.fixture
def runners(http_client: HttpClient) -> Runners:
    return Runners(http_client)


@pytest.fixture
def session(runners: Runners) -> RunnerSession:
    return RunnerSession(runners, "r-1", host_address="10.0.0.1:8080")


class TestFileDownload:
    def test_download(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_download", return_value=b"file content") as mock:
            result = session.download("/workspace/hello.py")
        assert result == b"file content"
        mock.assert_called_once_with("r-1", "/workspace/hello.py")

    def test_download_delegates_to_http(self, runners: Runners, http_client: HttpClient) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        with patch.object(http_client, "get_bytes", return_value=b"data") as mock:
            result = runners.file_download("r-1", "/tmp/test.txt")
        assert result == b"data"
        mock.assert_called_once_with(
            "/api/v1/runners/r-1/files/download",
            base_url="http://10.0.0.1:8080",
            params={"path": "/tmp/test.txt"},
        )


class TestFileUpload:
    def test_upload_bytes(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_upload", return_value={"success": True}) as mock:
            result = session.upload("/workspace/file.bin", b"raw bytes")
        assert result == {"success": True}
        mock.assert_called_once_with("r-1", "/workspace/file.bin", b"raw bytes", mode="overwrite", perm=None)

    def test_upload_str(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_upload", return_value={"success": True}) as mock:
            session.upload("/workspace/file.txt", "hello world")
        mock.assert_called_once_with("r-1", "/workspace/file.txt", b"hello world", mode="overwrite", perm=None)

    def test_upload_with_mode_and_perm(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_upload", return_value={"success": True}) as mock:
            session.upload("/workspace/file.txt", b"data", mode="append", perm="0644")
        mock.assert_called_once_with("r-1", "/workspace/file.txt", b"data", mode="append", perm="0644")

    def test_upload_delegates_to_http(self, runners: Runners, http_client: HttpClient) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        with patch.object(http_client, "post_bytes", return_value={"success": True}) as mock:
            runners.file_upload("r-1", "/tmp/f.txt", b"data", mode="overwrite", perm="0755")
        mock.assert_called_once_with(
            "/api/v1/runners/r-1/files/upload",
            data=b"data",
            base_url="http://10.0.0.1:8080",
            params={"path": "/tmp/f.txt", "mode": "overwrite", "perm": "0755"},
        )

    def test_upload_no_perm(self, runners: Runners, http_client: HttpClient) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        with patch.object(http_client, "post_bytes", return_value={"success": True}) as mock:
            runners.file_upload("r-1", "/tmp/f.txt", b"data")
        mock.assert_called_once_with(
            "/api/v1/runners/r-1/files/upload",
            data=b"data",
            base_url="http://10.0.0.1:8080",
            params={"path": "/tmp/f.txt", "mode": "overwrite"},
        )


class TestFileRead:
    def test_read_file(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_read", return_value={"content": "hello"}) as mock:
            result = session.read_file("/workspace/test.txt")
        assert result == {"content": "hello"}
        mock.assert_called_once_with("r-1", "/workspace/test.txt", offset=0, limit=None)

    def test_read_file_with_offset_limit(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_read", return_value={"content": "lo"}) as mock:
            session.read_file("/workspace/test.txt", offset=3, limit=2)
        mock.assert_called_once_with("r-1", "/workspace/test.txt", offset=3, limit=2)

    def test_read_delegates_to_http(self, runners: Runners, http_client: HttpClient) -> None:
        runners._host_cache["r-1"] = "10.0.0.1:8080"
        with patch.object(http_client, "post_to_host", return_value={"content": "data"}) as mock:
            runners.file_read("r-1", "/tmp/f.txt", offset=10, limit=5)
        mock.assert_called_once_with(
            "/api/v1/runners/r-1/files/read",
            json_body={"path": "/tmp/f.txt", "offset": 10, "limit": 5},
            base_url="http://10.0.0.1:8080",
        )


class TestFileWrite:
    def test_write_file(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_write", return_value={"success": True}) as mock:
            result = session.write_file("/workspace/out.txt", "content")
        assert result == {"success": True}
        mock.assert_called_once_with("r-1", "/workspace/out.txt", "content", mode="overwrite")

    def test_write_file_append(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_write", return_value={"success": True}) as mock:
            session.write_file("/workspace/log.txt", "line\n", mode="append")
        mock.assert_called_once_with("r-1", "/workspace/log.txt", "line\n", mode="append")


class TestFileList:
    def test_list_files(self, session: RunnerSession, runners: Runners) -> None:
        resp = {"entries": [{"name": "a.txt"}, {"name": "b.txt"}]}
        with patch.object(runners, "file_list", return_value=resp) as mock:
            result = session.list_files("/workspace")
        assert result == resp
        mock.assert_called_once_with("r-1", "/workspace", recursive=False)

    def test_list_files_recursive(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_list", return_value={"entries": []}) as mock:
            session.list_files("/workspace", recursive=True)
        mock.assert_called_once_with("r-1", "/workspace", recursive=True)


class TestFileStat:
    def test_stat_file(self, session: RunnerSession, runners: Runners) -> None:
        resp = {"size": 1024, "mode": "0644", "is_dir": False}
        with patch.object(runners, "file_stat", return_value=resp) as mock:
            result = session.stat_file("/workspace/hello.py")
        assert result == resp
        mock.assert_called_once_with("r-1", "/workspace/hello.py")


class TestFileRemove:
    def test_remove(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_remove", return_value={"success": True}) as mock:
            result = session.remove("/workspace/old.txt")
        assert result == {"success": True}
        mock.assert_called_once_with("r-1", "/workspace/old.txt", recursive=False)

    def test_remove_recursive(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_remove", return_value={"success": True}) as mock:
            session.remove("/workspace/dir", recursive=True)
        mock.assert_called_once_with("r-1", "/workspace/dir", recursive=True)


class TestFileMkdir:
    def test_mkdir(self, session: RunnerSession, runners: Runners) -> None:
        with patch.object(runners, "file_mkdir", return_value={"success": True}) as mock:
            result = session.mkdir("/workspace/newdir")
        assert result == {"success": True}
        mock.assert_called_once_with("r-1", "/workspace/newdir")
