from __future__ import annotations

from collections.abc import Iterator
from typing import Any

from bf_sdk._errors import BFServiceUnavailable
from bf_sdk._http import HttpClient
from bf_sdk._shell import ShellSession
from bf_sdk.models.runner import (
    AllocateRunnerResponse,
    ConnectResult,
    ExecEvent,
    PauseResult,
    RunnerStatus,
)
from bf_sdk.runner_session import RunnerSession


class Runners:
    """Runner management — control plane + host agent operations."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http
        self._host_cache: dict[str, str] = {}  # runner_id -> host_address

    def set_host_cache(self, runner_id: str, host_address: str) -> None:
        """Cache a host address for a runner (used by RunnerSession)."""
        self._host_cache[runner_id] = host_address

    # -- Control plane ---------------------------------------------------------

    def allocate(
        self,
        workload_key: str,
        *,
        request_id: str | None = None,
        labels: dict[str, str] | None = None,
        session_id: str | None = None,
        snapshot_tag: str | None = None,
        network_policy_preset: str | None = None,
        network_policy_json: str | None = None,
    ) -> AllocateRunnerResponse:
        body: dict[str, Any] = {"workload_key": workload_key}
        if request_id:
            body["request_id"] = request_id
        if labels:
            body["labels"] = labels
        if session_id:
            body["session_id"] = session_id
        if snapshot_tag:
            body["snapshot_tag"] = snapshot_tag
        if network_policy_preset:
            body["network_policy_preset"] = network_policy_preset
        if network_policy_json:
            body["network_policy_json"] = network_policy_json

        data = self._http.post("/api/v1/runners/allocate", json_body=body)
        resp = AllocateRunnerResponse.model_validate(data)
        if resp.host_address:
            self._host_cache[resp.runner_id] = resp.host_address
        return resp

    def status(self, runner_id: str) -> RunnerStatus:
        data = self._http.get("/api/v1/runners/status", params={"runner_id": runner_id})
        result = RunnerStatus.model_validate(data)
        if result.host_address:
            self._host_cache[result.runner_id] = result.host_address
        return result

    def list(self) -> list[dict[str, Any]]:
        data = self._http.get("/api/v1/runners")
        return data.get("runners", [])  # type: ignore[no-any-return]

    def release(self, runner_id: str) -> bool:
        data = self._http.post("/api/v1/runners/release", json_body={"runner_id": runner_id})
        self._host_cache.pop(runner_id, None)
        return data.get("success", False)  # type: ignore[no-any-return]

    def pause(self, runner_id: str) -> PauseResult:
        data = self._http.post("/api/v1/runners/pause", json_body={"runner_id": runner_id})
        return PauseResult.model_validate(data)

    def connect(self, runner_id: str) -> ConnectResult:
        data = self._http.post("/api/v1/runners/connect", json_body={"runner_id": runner_id})
        result = ConnectResult.model_validate(data)
        if result.host_address:
            self._host_cache[result.runner_id] = result.host_address
        return result

    def quarantine(
        self,
        runner_id: str,
        *,
        reason: str | None = None,
        block_egress: bool = True,
        pause_vm: bool = True,
    ) -> dict[str, Any]:
        params: dict[str, str] = {"runner_id": runner_id}
        if reason:
            params["reason"] = reason
        params["block_egress"] = str(block_egress).lower()
        params["pause_vm"] = str(pause_vm).lower()
        return self._http.post(
            "/api/v1/runners/quarantine?" + "&".join(f"{k}={v}" for k, v in params.items()),
        )

    def unquarantine(
        self,
        runner_id: str,
        *,
        unblock_egress: bool = True,
        resume_vm: bool = True,
    ) -> dict[str, Any]:
        params: dict[str, str] = {
            "runner_id": runner_id,
            "unblock_egress": str(unblock_egress).lower(),
            "resume_vm": str(resume_vm).lower(),
        }
        return self._http.post(
            "/api/v1/runners/unquarantine?" + "&".join(f"{k}={v}" for k, v in params.items()),
        )

    def wait_ready(self, runner_id: str, *, timeout: float = 120.0, poll_interval: float = 2.0) -> RunnerStatus:
        """Poll status until runner is ready or timeout."""
        import time

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = self.status(runner_id)
            if result.status == "ready":
                return result
            time.sleep(poll_interval)
        raise TimeoutError(f"Runner {runner_id} did not become ready within {timeout}s")

    def from_config(
        self,
        workload_key: str,
        *,
        tag: str = "stable",
        labels: dict[str, str] | None = None,
        session_id: str | None = None,
        network_policy_preset: str | None = None,
        network_policy_json: str | None = None,
    ) -> RunnerSession:
        """Allocate a runner from a runner config tag and return a RunnerSession handle.

        Usage::

            with client.runners.from_config("my-workload", tag="stable") as r:
                r.exec("python", "-c", "print(42)")
        """
        alloc = self.allocate(
            workload_key,
            labels=labels,
            session_id=session_id,
            snapshot_tag=tag,
            network_policy_preset=network_policy_preset,
            network_policy_json=network_policy_json,
        )
        return RunnerSession(
            self,
            alloc.runner_id,
            host_address=alloc.host_address,
            session_id=alloc.session_id,
        )

    # -- Host agent operations -------------------------------------------------

    def shell(
        self,
        runner_id: str,
        *,
        command: str | None = None,
        cols: int = 80,
        rows: int = 24,
    ) -> ShellSession:
        """Open a PTY shell session. Returns an unconnected ShellSession; use as context manager."""
        host = self._resolve_host(runner_id)
        qs_parts = [f"cols={cols}", f"rows={rows}"]
        if command:
            qs_parts.append(f"command={command}")
        qs = "&".join(qs_parts)

        scheme = "wss" if host.startswith("https://") else "ws"
        host_addr = host.replace("https://", "").replace("http://", "")
        ws_url = f"{scheme}://{host_addr}/api/v1/runners/{runner_id}/pty?{qs}"
        return ShellSession(ws_url)

    def exec(
        self,
        runner_id: str,
        command: list[str],
        *,
        env: dict[str, str] | None = None,
        working_dir: str | None = None,
        timeout_seconds: int | None = None,
    ) -> Iterator[ExecEvent]:
        """Execute a command and stream ndjson events. Retries on connection failure before first output."""
        body: dict[str, Any] = {"command": command}
        if env:
            body["env"] = env
        if working_dir:
            body["working_dir"] = working_dir
        if timeout_seconds:
            body["timeout_seconds"] = timeout_seconds

        return self._exec_with_host_retry(runner_id, body)

    # -- Internal host resolution ----------------------------------------------

    def _resolve_host(self, runner_id: str) -> str:
        if runner_id in self._host_cache:
            return self._host_cache[runner_id]
        result = self.connect(runner_id)
        if result.host_address:
            self._host_cache[runner_id] = result.host_address
            return result.host_address
        raise BFServiceUnavailable(f"No host address available for runner {runner_id}")

    def _exec_with_host_retry(self, runner_id: str, body: dict[str, Any]) -> Iterator[ExecEvent]:
        """Attempt exec; on 503 from host, reconnect and retry once."""
        host = self._resolve_host(runner_id)
        url = f"/api/v1/runners/{runner_id}/exec"

        received_any = False
        try:
            for event_dict in self._http.post_stream_ndjson(url, json_body=body, base_url=host):
                received_any = True
                yield ExecEvent.model_validate(event_dict)
            return
        except BFServiceUnavailable:
            if received_any:
                raise
            # Safe to retry: no output received yet
            self._host_cache.pop(runner_id, None)
            new_host = self._resolve_host(runner_id)
            for event_dict in self._http.post_stream_ndjson(url, json_body=body, base_url=new_host):
                yield ExecEvent.model_validate(event_dict)
