from __future__ import annotations

from pydantic import Field

from capsule_sdk.models.common import CapsuleModel


def _empty_entries() -> list[FileEntry]:
    return []


class FileEntry(CapsuleModel):
    name: str
    path: str | None = None
    is_dir: bool | None = None
    size: int | None = None
    mode: str | None = None
    mod_time: str | None = None


class FileReadResult(CapsuleModel):
    content: str | None = None
    encoding: str | None = None
    size: int | None = None
    path: str | None = None
    base64: bool | None = None


class FileWriteResult(CapsuleModel):
    path: str | None = None
    bytes_written: int | None = None


class FileUploadResult(CapsuleModel):
    path: str | None = None
    bytes_written: int | None = None


class FileListResult(CapsuleModel):
    entries: list[FileEntry] = Field(default_factory=_empty_entries)
    path: str | None = None


class FileStatResult(CapsuleModel):
    exists: bool | None = None
    size: int | None = None
    mode: str | None = None
    is_dir: bool | None = None
    name: str | None = None
    path: str | None = None
    mod_time: str | None = None


class FileRemoveResult(CapsuleModel):
    removed: bool | None = None
    path: str | None = None


class FileMkdirResult(CapsuleModel):
    created: bool | None = None
    path: str | None = None
