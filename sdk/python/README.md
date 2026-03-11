# bf-sdk: Python SDK for bazel-firecracker

## Installation

```bash
pip install bf-sdk
```

Or for development:

```bash
cd sdk/python
pip install -e ".[dev]"
```

## Live E2E

There is an explicit live SDK E2E at `sdk/python/tests/e2e_live.py`.
It talks to a real control plane, defaults to `http://localhost:8080`, and
exercises config registration, build enqueue, allocation, exec, file ops, PTY,
pause/resume, release, and config cleanup.

Run it with:

```bash
make sdk-python-e2e
```

If you are not using the default address:

```bash
BF_BASE_URL="http://localhost:8080" make sdk-python-e2e
```

If the control plane or local stack is not up, the test raises a clear error
telling you to spin it up first with `bash dev/run-stack.sh`.

## Quickstart

The fastest way to get started is the high-level `workloads` API: onboard a workload, then start it by name.

```python
from bf_sdk import BFClient, RunnerConfig

# 1. Define your runner config
cfg = (
    RunnerConfig("My dev sandbox")
    .with_base_image("ubuntu:22.04")
    .with_commands(["apt-get install -y python3", "pip install -e .[dev]"])
    .with_tier("m")
    .with_ttl(3600)
    .with_auto_pause(True)
    .with_auto_rollout(True)
)

with BFClient(base_url="http://localhost:8080", token="my-token") as bf:
    # 2. Onboard the workload and build it
    workload = bf.workloads.onboard(cfg)

    # 3. Spawn a ready runner by workload name (auto-released on exit)
    with bf.workloads.start(workload) as r:
        # Run a command and collect output
        output, code = r.exec_collect("python", "-c", "print('hello')")
        print(output, code)

        # Ergonomic file helpers
        r.write_text("/workspace/hello.txt", "hello")
        print(r.read_text("/workspace/hello.txt"))

        # Or stream events
        for event in r.exec("pytest", "-q"):
            if event.type == "stdout":
                print(event.data, end="")

        # Interactive PTY shell
        with r.shell(cols=120, rows=40) as sh:
            sh.send("ls -la\n")
            print(sh.recv_stdout(timeout=5.0).decode())

        # Pause and resume
        r.pause()
        r.resume()
```

You can also onboard from YAML directly:

```python
from bf_sdk import BFClient

with BFClient(base_url="http://localhost:8080", token="my-token") as bf:
    workload = bf.workloads.onboard_yaml(
        "examples/afs/onboard.yaml",
        name="afs-sandbox",
    )

    with bf.workloads.start("afs-sandbox") as runner:
        print(runner.read_text("/etc/hostname"))
```

## Async Quickstart

The SDK also exposes a parallel async API for agent frameworks, servers, and other event-loop-native callers:

```python
import asyncio

from bf_sdk import AsyncBFClient, RunnerConfig


async def main() -> None:
    cfg = (
        RunnerConfig("My async sandbox")
        .with_base_image("ubuntu:22.04")
        .with_commands(["echo async-ready"])
        .with_tier("m")
        .with_ttl(3600)
        .with_auto_pause(True)
    )

    async with AsyncBFClient(base_url="http://localhost:8080", token="my-token") as bf:
        workload = await bf.workloads.onboard(cfg)
        runner = await bf.workloads.start(workload)
        async with runner:
            result = await runner.exec_collect("sh", "-lc", "printf hello")
            print(result.stdout, result.exit_code)

            await runner.write_text("/workspace/hello.txt", "hello")
            print(await runner.read_text("/workspace/hello.txt"))

            async for event in runner.exec("pytest", "-q"):
                if event.type == "stdout":
                    print(event.data, end="")

            async with runner.shell(cols=120, rows=40) as sh:
                await sh.send("ls -la\n")
                print((await sh.recv_stdout()).decode())

            await runner.pause()
            await runner.resume()


asyncio.run(main())
```

## Low-level API

For full control, use the lower-level resource APIs directly. `allocate()` now retries transient capacity failures internally using a stable `request_id`, `allocate_ready()` gives you a single "usable runner" step, and the SDK can resolve workload names for you instead of forcing `leaf_workload_key` through your app:

```python
from bf_sdk import BFClient

with BFClient(base_url="http://localhost:8080", token="my-token") as client:
    # Allocate and wait for a usable runner
    with client.runners.allocate_ready("my-workload-key") as runner:
        # Execute a command (streaming ndjson)
        for event in runner.exec("echo", "hello"):
            if event.type == "stdout":
                print(event.data, end="")
            elif event.type == "exit":
                print(f"Exit code: {event.code}")

        # Typed file results still support dict-style access
        write_result = runner.write_text("/workspace/out.txt", "hello")
        print(write_result.bytes_written)

    # Or keep low-level allocation + explicit waiting if you want
    allocation = client.workloads.allocate("My dev sandbox", startup_timeout=45.0)
    client.runners.wait_ready(allocation.runner_id, timeout=45.0)
```

The async client mirrors the same concepts and models:

```python
from bf_sdk import AsyncBFClient


async def run() -> None:
    async with AsyncBFClient(base_url="http://localhost:8080", token="my-token") as client:
        runner = await client.runners.allocate_ready("my-workload-key")
        async with runner:
            async for event in runner.exec("echo", "hello"):
                if event.type == "stdout":
                    print(event.data, end="")
```

## Configuration

| Parameter | Env var | Default |
|---|---|---|
| `base_url` | `BF_BASE_URL` | `http://localhost:8080` |
| `token` | `BF_TOKEN` | `None` |
| `request_timeout` | `BF_REQUEST_TIMEOUT` | `30.0` |
| `startup_timeout` | `BF_STARTUP_TIMEOUT` | `45.0` |
| `operation_timeout` | `BF_OPERATION_TIMEOUT` | `120.0` |

`BFClient(timeout=...)` is still supported as a backwards-compatible alias for `request_timeout`.

## Key Concepts

| SDK concept | Server primitive | Description |
|---|---|---|
| `RunnerConfig` | LayeredConfig | Declarative runner shape (layers, tier, TTL, etc.) |
| `workloads.onboard()` | LayeredConfig create + build | Register a workload from Python or YAML |
| `runner_configs.build()` | `/layered-configs/{id}/build` | Advanced build control for an already-registered config |
| `runners.from_config()` | `/runners/allocate` | Spawn a runner from a config |
| `RunnerSession` | Runner ID + host cache | High-level handle with exec/shell/pause/resume |

## Host Reconnection

The SDK caches host addresses from `allocate()` and `connect()` responses. If a host agent returns 503 during `exec()`, the SDK automatically calls `connect()` to get a new host and retries (only if no output has been received yet). Safe read-only host operations also refresh the cached host and retry once, and PTY attach will reconnect once with a refreshed host before surfacing an error.

## Retry And Timeout Model

- `allocate()` retries transient capacity and control-plane availability failures until `startup_timeout` expires.
- `workloads.onboard_yaml()` accepts either YAML text or a YAML file path. If the YAML does not include `display_name`, pass `name=...`.
- `workloads.start()` is the preferred high-level path for named workloads.
- `from_config()` waits for a ready runner by default; pass `wait_ready=False` if you want lower-level control.
- `from_config()` and `allocate()` accept a workload display name, config response, config id, or raw workload key. Most callers should stick to a display name and let the SDK resolve the control-plane key.
- `request_timeout` covers one HTTP request, `startup_timeout` covers "get me a usable runner", and `operation_timeout` covers host-side file/PTY/stream operations.
- Successful responses include a `request_id` so you can correlate SDK behavior with control-plane logs.

Layer commands use the control-plane `SnapshotCommand` shape under the hood:
`{"type": "shell", "args": ["bash", "-lc", "echo hi"]}`. The shorthand
`RunnerConfig.with_commands(["echo hi"])` is normalized to that wire format for you.
