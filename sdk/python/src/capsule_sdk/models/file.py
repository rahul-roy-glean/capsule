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


class FileReadResult(CapsuleModel):
    content: str | None = None
    encoding: str | None = None
    size: int | None = None


class FileWriteResult(CapsuleModel):
    success: bool | None = None
    bytes_written: int | None = None


class FileUploadResult(CapsuleModel):
    success: bool | None = None
    bytes_written: int | None = None


class FileListResult(CapsuleModel):
    entries: list[FileEntry] = Field(default_factory=_empty_entries)


class FileStatResult(CapsuleModel):
    exists: bool | None = None
    size: int | None = None
    mode: str | None = None
    is_dir: bool | None = None


class FileRemoveResult(CapsuleModel):
    success: bool | None = None
    removed: bool | None = None


class FileMkdirResult(CapsuleModel):
    success: bool | None = None
