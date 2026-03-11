from __future__ import annotations

from pydantic import Field

from bf_sdk.models.common import BFModel


class FileEntry(BFModel):
    name: str
    path: str | None = None
    is_dir: bool | None = None
    size: int | None = None
    mode: str | None = None


class FileReadResult(BFModel):
    content: str | None = None
    encoding: str | None = None
    size: int | None = None


class FileWriteResult(BFModel):
    success: bool | None = None
    bytes_written: int | None = None


class FileUploadResult(BFModel):
    success: bool | None = None
    bytes_written: int | None = None


class FileListResult(BFModel):
    entries: list[FileEntry] = Field(default_factory=list)


class FileStatResult(BFModel):
    exists: bool | None = None
    size: int | None = None
    mode: str | None = None
    is_dir: bool | None = None


class FileRemoveResult(BFModel):
    success: bool | None = None
    removed: bool | None = None


class FileMkdirResult(BFModel):
    success: bool | None = None
