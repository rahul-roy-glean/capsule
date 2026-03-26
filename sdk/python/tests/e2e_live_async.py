from __future__ import annotations

import asyncio
import os
import time
import uuid
from contextlib import suppress
from typing import Any

import httpx

from capsule_sdk import AsyncCapsuleClient, AsyncRunnerSession, RunnerConfig


def _preflight_local_stack(base_url: str) -> None:
    try:
        health = httpx.get(f"{base_url}/health", timeout=2.0)
        health.raise_for_status()
    except httpx.HTTPError as exc:
        raise RuntimeError(
            f"Control plane is not reachable at {base_url}. "
            "Spin up the local stack first with `bash dev/run-stack.sh`.",
        ) from exc

    try:
        hosts = httpx.get(f"{base_url}/api/v1/hosts", timeout=2.0)
        hosts.raise_for_status()
    except httpx.HTTPError as exc:
        raise RuntimeError(
            f"Control plane is up at {base_url}, but `/api/v1/hosts` is unavailable. "
            "Spin up the local stack first with `bash dev/run-stack.sh`.",
        ) from exc

    host_list = hosts.json().get("hosts", [])
    if not host_list:
        raise RuntimeError(
            f"Control plane is reachable at {base_url}, but no hosts are registered. "
            "Spin up the local stack first with `bash dev/run-stack.sh`.",
        )


async def _wait_for_shell_marker(session: Any, marker: str, *, timeout: float = 10.0) -> str:
    deadline = time.monotonic() + timeout
    chunks: list[str] = []
    async with session.shell(command="/bin/sh", cols=100, rows=30) as shell:
        await shell.send(f"echo {marker}\n")
        while time.monotonic() < deadline:
            try:
                chunk = await shell.recv_stdout(timeout=1.0)
            except TimeoutError:
                continue
            if not chunk:
                continue
            text = chunk.decode(errors="replace")
            chunks.append(text)
            combined = "".join(chunks)
            if marker in combined:
                await shell.send("exit\n")
                return combined
        await shell.send("exit\n")
    raise AssertionError(f"PTY session did not echo marker {marker!r}. Output: {''.join(chunks)!r}")


def _entry_names(entries: list[dict[str, Any]]) -> set[str]:
    names: set[str] = set()
    for entry in entries:
        name = entry.get("name")
        if isinstance(name, str):
            names.add(name)
    return names


def _normalized_stdout(text: str) -> str:
    return text.rstrip("\r\n")


def test_sdk_live_async_e2e() -> None:
    asyncio.run(_run_live_async_e2e())


async def _run_live_async_e2e() -> None:
    base_url = os.getenv("CAPSULE_BASE_URL", "http://localhost:8080").rstrip("/")
    _preflight_local_stack(base_url)

    unique = f"sdk-async-e2e-{int(time.time())}-{uuid.uuid4().hex[:8]}"
    session_id = f"{unique}-session"
    text_path = f"/workspace/{unique}.txt"
    binary_path = f"/workspace/{unique}.bin"
    config_id: str | None = None
    workload = None
    released = False

    async with AsyncCapsuleClient(base_url=base_url) as client:
        config = (
            RunnerConfig(unique)
            .with_commands([f"echo {unique}"])
            .with_ttl(300)
            .with_auto_pause(False)
        )

        try:
            workload = await client.workloads.onboard(config)
            config_id = workload.config_id
            assert config_id

            listed_ids = {cfg.config_id for cfg in await client.workloads.list() if cfg.config_id}
            assert config_id in listed_ids

            detail = await client.workloads.get(unique)
            assert detail.config_id == config_id
            assert detail.display_name == unique

            allocation = await client.workloads.allocate(
                workload,
                session_id=session_id,
                network_policy_preset="restricted-egress",
                startup_timeout=45.0,
            )

            runner = AsyncRunnerSession(
                client.runners,
                allocation.runner_id,
                host_address=allocation.host_address,
                session_id=allocation.session_id,
            )

            try:
                await client.runners.wait_ready(allocation.runner_id, timeout=120.0, poll_interval=2.0)

                exec_result = await runner.exec_collect("sh", "-lc", f"printf {unique}")
                assert exec_result.exit_code == 0
                assert _normalized_stdout(exec_result.stdout) == unique

                write_result = await runner.write_text(text_path, unique)
                assert write_result["bytes_written"] == len(unique)

                read_result = await runner.read_file(text_path)
                assert read_result.content == unique
                assert await runner.read_text(text_path) == unique

                stat_result = await runner.stat_file(text_path)
                assert stat_result["exists"] is True

                list_result = await runner.list_files("/workspace")
                assert f"{unique}.txt" in _entry_names([entry.model_dump() for entry in list_result.entries])

                payload = b"\x00\x01sdk-e2e"
                upload_result = await runner.upload(binary_path, payload)
                assert upload_result["bytes_written"] == len(payload)
                assert await runner.download(binary_path) == payload

                shell_output = await _wait_for_shell_marker(runner, f"{unique}-pty")
                assert f"{unique}-pty" in shell_output

                pause = await runner.pause()
                assert pause.success is True
                assert runner.session_id

                # Resume is done via allocate with session_id (not via the removed connect endpoint).
                resumed_alloc = await client.runners.allocate(
                    workload,
                    session_id=runner.session_id,
                    startup_timeout=120.0,
                )
                assert resumed_alloc.runner_id

                resumed_status = await client.runners.wait_ready(
                    resumed_alloc.runner_id, timeout=120.0, poll_interval=2.0,
                )
                assert resumed_status.status == "ready"

                resumed_exec = await runner.exec_collect("sh", "-lc", "printf resumed")
                assert resumed_exec.exit_code == 0
                assert _normalized_stdout(resumed_exec.stdout) == "resumed"

                remove_text = await runner.remove(text_path)
                assert remove_text["removed"] is True
                remove_binary = await runner.remove(binary_path)
                assert remove_binary["removed"] is True

                assert await runner.release() is True
                released = True
            finally:
                if not released:
                    with suppress(Exception):
                        await runner.release()
        finally:
            if config_id is not None:
                with suppress(Exception):
                    await client.workloads.delete(workload or config_id)
