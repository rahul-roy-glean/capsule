#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
import uuid
import urllib.error
import urllib.request

from capsule_sdk import CapsuleClient
from capsule_sdk import RunnerSession


def _normalize_http_addr(addr: str) -> str:
    if addr.startswith(("http://", "https://")):
        return addr.rstrip("/")
    return f"http://{addr}".rstrip("/")


def _resolve_runner_proxy_base(client: CapsuleClient, runner: RunnerSession, host_override: str | None) -> str:
    if host_override:
        return _normalize_http_addr(host_override)

    status = client.runners.status(runner.runner_id)
    if status.host_address:
        return _normalize_http_addr(status.host_address)

    connect_result = client.runners.connect(runner.runner_id)
    if connect_result.host_address:
        return _normalize_http_addr(connect_result.host_address)

    raise RuntimeError(f"No host address available for runner {runner.runner_id}")


def _service_http(
    client: CapsuleClient,
    runner: RunnerSession,
    *,
    host_override: str | None,
    method: str,
    path: str,
    body: dict | None = None,
    timeout_seconds: float = 30.0,
) -> str:
    proxy_base = _resolve_runner_proxy_base(client, runner, host_override)
    normalized_path = path if path.startswith("/") else f"/{path}"
    url = f"{proxy_base}/api/v1/runners/{runner.runner_id}/proxy{normalized_path}"
    payload = json.dumps(body).encode("utf-8") if body is not None else None

    request = urllib.request.Request(url, data=payload, method=method)
    request.add_header("Content-Type", "application/json")

    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            return response.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        response_body = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(
            f"Request {method} {url} failed with status={exc.code}\n"
            f"body:\n{response_body}"
        ) from exc


def _service_http_json(
    client: CapsuleClient,
    runner: RunnerSession,
    *,
    host_override: str | None,
    method: str,
    path: str,
    body: dict | None = None,
    timeout_seconds: float = 30.0,
) -> dict:
    raw = _service_http(
        client,
        runner,
        host_override=host_override,
        method=method,
        path=path,
        body=body,
        timeout_seconds=timeout_seconds,
    )
    return json.loads(raw)


def _service_http_stream(
    client: CapsuleClient,
    runner: RunnerSession,
    *,
    host_override: str | None,
    path: str,
    body: dict,
    timeout_seconds: float = 60.0,
) -> list[dict]:
    raw = _service_http(
        client,
        runner,
        host_override=host_override,
        method="POST",
        path=path,
        body=body,
        timeout_seconds=timeout_seconds,
    )
    messages = []
    for line in raw.splitlines():
        line = line.strip()
        if not line:
            continue
        messages.append(json.loads(line))
    return messages


def _extract_last_human_message(messages: list[dict], message_type: str) -> str | None:
    for message in reversed(messages):
        if message.get("type") == message_type:
            return message.get("human_readable_message")
    return None


def _print_section(title: str) -> None:
    print(f"\n== {title} ==")


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Demonstrate happy-path Claude sandbox interactions through the Capsule SDK.",
    )
    parser.add_argument(
        "--workload",
        required=True,
        help="Deployed Capsule workload name or workload key for the Claude sandbox service.",
    )
    parser.add_argument(
        "--base-url",
        default=os.environ.get("CAPSULE_BASE_URL", "http://localhost:8080"),
        help="Capsule control plane base URL.",
    )
    parser.add_argument(
        "--host-override",
        default=os.environ.get("CAPSULE_HOST_OVERRIDE"),
        help="Optional host proxy override used for runner-facing HTTP requests.",
    )
    parser.add_argument(
        "--token",
        default=os.environ.get("CAPSULE_TOKEN"),
        help="Capsule API token, if required.",
    )
    parser.add_argument(
        "--session-id",
        default=None,
        help="Optional Capsule session ID. Defaults to a generated session.",
    )
    parser.add_argument(
        "--startup-timeout",
        type=float,
        default=120.0,
        help="Seconds to wait for the runner to become ready.",
    )
    parser.add_argument(
        "--query-timeout",
        type=float,
        default=90.0,
        help="Seconds to allow each Claude query request.",
    )
    parser.add_argument(
        "--keep-runner",
        action="store_true",
        help="Do not release the runner at the end.",
    )
    parser.add_argument(
        "--exercise-pause-resume",
        action="store_true",
        help="Also demonstrate continuity across Capsule pause/resume after the happy path succeeds.",
    )
    args = parser.parse_args()

    requested_session_id = args.session_id or f"claude-demo-{uuid.uuid4().hex[:12]}"
    expected_token = f"ORCHID-{uuid.uuid4().hex[:8].upper()}"
    runner: RunnerSession | None = None

    print(f"Using workload: {args.workload}")
    print(f"Using Capsule control plane: {args.base_url}")
    if args.host_override:
        print(f"Using runner host override: {args.host_override}")
    print(f"Requested Capsule session ID: {requested_session_id}")

    with CapsuleClient(base_url=args.base_url.rstrip("/"), token=args.token) as client:
        allocation = client.runners.allocate(
            args.workload,
            session_id=requested_session_id,
            startup_timeout=args.startup_timeout,
        )
        _print_section("Raw Allocate Response")
        print(allocation.model_dump_json(indent=2))

        runner = RunnerSession(
            client.runners,
            allocation.runner_id,
            host_address=allocation.host_address,
            session_id=allocation.session_id,
            request_id=allocation.request_id,
        )

        release_runner = not args.keep_runner

        try:
            runner.wait_ready(timeout=args.startup_timeout, poll_interval=2.0)
            capsule_session_id = runner.session_id or requested_session_id
            print(f"Allocated runner: {runner.runner_id}")
            print(f"Resolved Capsule session ID: {capsule_session_id}")

            _print_section("Service Health")
            health = _service_http_json(
                client,
                runner,
                host_override=args.host_override,
                method="GET",
                path="/health",
            )
            print(json.dumps(health, indent=2))

            _print_section("Service Info")
            info = _service_http_json(
                client,
                runner,
                host_override=args.host_override,
                method="GET",
                path="/info",
            )
            print(json.dumps(info, indent=2))

            _print_section("Create Claude Session")
            session_info = _service_http_json(
                client,
                runner,
                host_override=args.host_override,
                method="POST",
                path="/sessions",
                body={"session_id": capsule_session_id},
            )
            print(json.dumps(session_info, indent=2))

            _print_section("Session Info")
            created_session = _service_http_json(
                client,
                runner,
                host_override=args.host_override,
                method="GET",
                path=f"/sessions/{capsule_session_id}",
            )
            print(json.dumps(created_session, indent=2))

            _print_section("First Query")
            first_messages = _service_http_stream(
                client,
                runner,
                host_override=args.host_override,
                path=f"/sessions/{capsule_session_id}/query",
                body={
                    "prompt": (
                        f"Remember the token {expected_token} for the rest of this session. "
                        "Reply with only that token."
                    ),
                    "timeout": args.query_timeout,
                },
                timeout_seconds=args.query_timeout,
            )
            print(json.dumps(first_messages, indent=2))

            first_assistant = _extract_last_human_message(first_messages, "assistant")
            if first_assistant is None or expected_token not in first_assistant:
                raise AssertionError(
                    "Claude did not acknowledge the expected token on the first turn.\n"
                    f"Expected token: {expected_token}\n"
                    f"Assistant message: {first_assistant!r}"
                )

            _print_section("Second Query In Same Live Session")
            second_messages = _service_http_stream(
                client,
                runner,
                host_override=args.host_override,
                path=f"/sessions/{capsule_session_id}/query",
                body={
                    "prompt": "What token did I ask you to remember? Reply with only that token.",
                    "timeout": args.query_timeout,
                },
                timeout_seconds=args.query_timeout,
            )
            print(json.dumps(second_messages, indent=2))

            second_assistant = _extract_last_human_message(second_messages, "assistant")
            if second_assistant is None or expected_token not in second_assistant:
                raise AssertionError(
                    "Claude did not preserve context across happy-path session turns.\n"
                    f"Expected token: {expected_token}\n"
                    f"Assistant message: {second_assistant!r}"
                )

            _print_section("Session Info After Queries")
            after_queries = _service_http_json(
                client,
                runner,
                host_override=args.host_override,
                method="GET",
                path=f"/sessions/{capsule_session_id}",
            )
            print(json.dumps(after_queries, indent=2))

            _print_section("Reset Session")
            reset_response = _service_http_json(
                client,
                runner,
                host_override=args.host_override,
                method="POST",
                path=f"/sessions/{capsule_session_id}/reset",
                body={},
            )
            print(json.dumps(reset_response, indent=2))

            print("\nSuccess: happy-path Claude session interactions worked end-to-end.")

            if args.exercise_pause_resume:
                _print_section("Recreate Session Before Pause/Resume")
                recreated_session = _service_http_json(
                    client,
                    runner,
                    host_override=args.host_override,
                    method="POST",
                    path="/sessions",
                    body={"session_id": capsule_session_id},
                )
                print(json.dumps(recreated_session, indent=2))

                _print_section("Prime Session Before Pause")
                pre_pause_messages = _service_http_stream(
                    client,
                    runner,
                    host_override=args.host_override,
                    path=f"/sessions/{capsule_session_id}/query",
                    body={
                        "prompt": (
                            f"Remember the token {expected_token} for the rest of this session. "
                            "Reply with only that token."
                        ),
                        "timeout": args.query_timeout,
                    },
                    timeout_seconds=args.query_timeout,
                )
                print(json.dumps(pre_pause_messages, indent=2))

                _print_section("Session Info Before Pause")
                before_pause = _service_http_json(
                    client,
                    runner,
                    host_override=args.host_override,
                    method="GET",
                    path=f"/sessions/{capsule_session_id}",
                )
                print(json.dumps(before_pause, indent=2))
                # if not before_pause.get("can_pause", False):
                #     raise AssertionError(f"Claude session {capsule_session_id} is unexpectedly busy before pause.")
                #
                # _print_section("Pause Runner")
                # pause_result = runner.pause()
                # print(pause_result.model_dump_json(indent=2))
                #
                # _print_section("Resume Runner")
                # resume_result = runner.resume()
                # print(resume_result.model_dump_json(indent=2))
                # runner.wait_ready(timeout=args.startup_timeout, poll_interval=2.0)
                #
                # _print_section("Query After Resume")
                # resumed_messages = _runner_http_stream(
                #     runner,
                #     path=f"/sessions/{capsule_session_id}/query",
                #     body={
                #         "prompt": "What token did I ask you to remember? Reply with only that token.",
                #         "timeout": args.query_timeout,
                #     },
                #     timeout_seconds=args.query_timeout,
                # )
                # print(json.dumps(resumed_messages, indent=2))
                #
                # resumed_assistant = _extract_last_human_message(resumed_messages, "assistant")
                # if resumed_assistant is None or expected_token not in resumed_assistant:
                #     raise AssertionError(
                #         "Claude session continuity failed across pause/resume.\n"
                #         f"Expected token: {expected_token}\n"
                #         f"Assistant message: {resumed_assistant!r}"
                #     )
                #
                # _print_section("Session Info After Resume")
                # after_resume = _runner_http_json(
                #     runner,
                #     method="GET",
                #     path=f"/sessions/{capsule_session_id}",
                # )
                # print(json.dumps(after_resume, indent=2))
                #
                # print("\nPause/resume continuity also succeeded.")
                # print(
                #     "Note: `reconnect_count` may stay 0 if the SDK transport survives resume cleanly, "
                #     "or increment if the service had to recover a stale client."
                # )
        finally:
            if runner is not None and release_runner:
                try:
                    runner.release()
                    print("\nReleased runner.")
                except Exception as exc:  # pragma: no cover - best effort cleanup for demo script
                    print(f"\nWarning: failed to release runner: {exc}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
