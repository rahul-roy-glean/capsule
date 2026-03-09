from __future__ import annotations

import os
import time
import uuid
from contextlib import suppress
from typing import Any

import httpx

from bf_sdk import BFClient, BFServiceUnavailable, RunnerConfig, RunnerSession


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


def _wait_for_shell_marker(session: RunnerSession, marker: str, *, timeout: float = 10.0) -> str:
    deadline = time.monotonic() + timeout
    chunks: list[str] = []
    with session.shell(command="/bin/sh", cols=100, rows=30) as shell:
        shell.send(f"echo {marker}\n")
        while time.monotonic() < deadline:
            try:
                chunk = shell.recv_stdout(timeout=1.0)
            except TimeoutError:
                continue
            if not chunk:
                continue
            text = chunk.decode(errors="replace")
            chunks.append(text)
            combined = "".join(chunks)
            if marker in combined:
                shell.send("exit\n")
                return combined
        shell.send("exit\n")
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


def _allocate_with_retry(
    client: BFClient,
    workload_key: str,
    *,
    session_id: str,
    network_policy_preset: str,
    timeout: float = 45.0,
    poll_interval: float = 2.0,
) -> Any:
    deadline = time.monotonic() + timeout
    last_error: BFServiceUnavailable | None = None

    while time.monotonic() < deadline:
        try:
            return client.runners.allocate(
                workload_key,
                session_id=session_id,
                network_policy_preset=network_policy_preset,
            )
        except BFServiceUnavailable as exc:
            last_error = exc
            time.sleep(poll_interval)

    raise RuntimeError(
        "The control plane is reachable, but there are no allocatable hosts yet. "
        "If you just started the stack, wait a few seconds for firecracker-manager "
        "to heartbeat and retry. Otherwise inspect `/tmp/fc-dev/logs/control-plane.log` "
        "and `/tmp/fc-dev/logs/firecracker-manager.log`."
    ) from last_error


def test_sdk_live_e2e() -> None:
    base_url = os.getenv("BF_BASE_URL", "http://localhost:8080").rstrip("/")
    _preflight_local_stack(base_url)

    unique = f"sdk-e2e-{int(time.time())}-{uuid.uuid4().hex[:8]}"
    session_id = f"{unique}-session"
    text_path = f"/workspace/{unique}.txt"
    binary_path = f"/workspace/{unique}.bin"
    config_id: str | None = None
    released = False

    with BFClient(base_url=base_url) as client:
        config = (
            RunnerConfig(unique)
            .with_commands([f"echo {unique}"])
            .with_ttl(300)
            .with_auto_pause(False)
        )

        try:
            created = client.runner_configs.apply(config)
            config_id = created.config_id
            assert created.leaf_workload_key

            build = client.runner_configs.build(config_id)
            assert build.config_id == config_id
            assert build.status == "build_enqueued"

            listed_ids = {cfg.config_id for cfg in client.layered_configs.list()}
            assert config_id in listed_ids

            detail = client.layered_configs.get(config_id)
            assert detail.config.config_id == config_id
            assert detail.config.leaf_workload_key == created.leaf_workload_key

            allocation = _allocate_with_retry(
                client,
                created.leaf_workload_key,
                session_id=session_id,
                network_policy_preset="restricted-egress",
            )

            runner = RunnerSession(
                client.runners,
                allocation.runner_id,
                host_address=allocation.host_address,
                session_id=allocation.session_id,
            )

            try:
                status = client.runners.wait_ready(allocation.runner_id, timeout=120.0, poll_interval=2.0)
                assert status.status == "ready"

                exec_result = runner.exec_collect("sh", "-lc", f"printf {unique}")
                assert exec_result.exit_code == 0
                assert _normalized_stdout(exec_result.stdout) == unique

                write_result = runner.write_file(text_path, unique)
                assert write_result["bytes_written"] == len(unique)

                read_result = runner.read_file(text_path)
                assert read_result["content"] == unique

                stat_result = runner.stat_file(text_path)
                assert stat_result["exists"] is True

                list_result = runner.list_files("/workspace")
                entries = list_result.get("entries", [])
                assert isinstance(entries, list)
                assert f"{unique}.txt" in _entry_names(entries)

                payload = b"\x00\x01sdk-e2e"
                upload_result = runner.upload(binary_path, payload)
                assert upload_result["bytes_written"] == len(payload)
                assert runner.download(binary_path) == payload

                shell_output = _wait_for_shell_marker(runner, f"{unique}-pty")
                assert f"{unique}-pty" in shell_output

                pause = runner.pause()
                assert pause.success is True
                assert runner.session_id

                previous_runner_id = runner.runner_id
                resumed = runner.resume()
                assert resumed.status in {"connected", "resumed"}
                assert runner.runner_id == resumed.runner_id
                assert runner.runner_id
                if resumed.status == "resumed":
                    # A restore may keep the same runner_id or allocate a fresh one.
                    # The SDK contract is that the session follows the returned ID.
                    assert runner.runner_id in {previous_runner_id, resumed.runner_id}

                resumed_status = client.runners.wait_ready(runner.runner_id, timeout=120.0, poll_interval=2.0)
                assert resumed_status.status == "ready"

                resumed_exec = runner.exec_collect("sh", "-lc", "printf resumed")
                assert resumed_exec.exit_code == 0
                assert _normalized_stdout(resumed_exec.stdout) == "resumed"

                remove_text = runner.remove(text_path)
                assert remove_text["removed"] is True
                remove_binary = runner.remove(binary_path)
                assert remove_binary["removed"] is True

                assert runner.release() is True
                released = True
            finally:
                if not released:
                    with suppress(Exception):
                        runner.release()
        finally:
            if config_id is not None:
                with suppress(Exception):
                    client.layered_configs.delete(config_id)
