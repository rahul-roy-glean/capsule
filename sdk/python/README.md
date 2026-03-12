# Capsule SDK

The Capsule SDK is the recommended client surface for registering workloads,
triggering builds, allocating runners, and interacting with running Capsule
sandboxes from Python.

## Requirements

- Python `>= 3.10`
- access to a running Capsule control plane
- an API token if your deployment requires authenticated requests

## Installation

```bash
pip install capsule-sdk
```

For local development:

```bash
cd sdk/python
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev]"
```

## Configuration

The SDK can be configured directly in code or through environment variables.

| Parameter | Env var | Default |
|---|---|---|
| `base_url` | `CAPSULE_BASE_URL` | `http://localhost:8080` |
| `token` | `CAPSULE_TOKEN` | `None` |
| `request_timeout` | `CAPSULE_REQUEST_TIMEOUT` | `30.0` |
| `startup_timeout` | `CAPSULE_STARTUP_TIMEOUT` | `45.0` |
| `operation_timeout` | `CAPSULE_OPERATION_TIMEOUT` | `120.0` |

Example:

```bash
export CAPSULE_BASE_URL="http://localhost:8080"
export CAPSULE_TOKEN="my-token"
```

## Quickstart

The fastest way to get started is the high-level `workloads` API.

```python
from capsule_sdk import CapsuleClient, RunnerConfig

cfg = (
    RunnerConfig("My dev sandbox")
    .with_base_image("ubuntu:22.04")
    .with_commands(["apt-get update", "apt-get install -y python3"])
    .with_tier("m")
    .with_ttl(3600)
    .with_auto_pause(True)
    .with_auto_rollout(True)
)

with CapsuleClient(base_url="http://localhost:8080", token="my-token") as client:
    workload = client.workloads.onboard(cfg)

    with client.workloads.start(workload) as runner:
        output, code = runner.exec_collect("python3", "-c", "print('hello')")
        print(output, code)

        runner.write_text("/workspace/hello.txt", "hello")
        print(runner.read_text("/workspace/hello.txt"))
```

## Onboard From YAML

You can also onboard directly from an `onboard.yaml`-style file:

```python
from capsule_sdk import CapsuleClient

with CapsuleClient(base_url="http://localhost:8080", token="my-token") as client:
    workload = client.workloads.onboard_yaml(
        "examples/afs/onboard.yaml",
        name="afs-sandbox",
    )

    with client.workloads.start("afs-sandbox") as runner:
        print(runner.read_text("/etc/hostname"))
```

The AFS example is an example workload name, not a special SDK mode. See
`examples/afs/` for the underlying config shape.

## Async Quickstart

Use the async client in event-loop-native applications:

```python
import asyncio

from capsule_sdk import AsyncCapsuleClient, RunnerConfig


async def main() -> None:
    cfg = (
        RunnerConfig("My async sandbox")
        .with_base_image("ubuntu:22.04")
        .with_commands(["echo async-ready"])
        .with_tier("m")
        .with_ttl(3600)
        .with_auto_pause(True)
    )

    async with AsyncCapsuleClient(base_url="http://localhost:8080", token="my-token") as client:
        workload = await client.workloads.onboard(cfg)
        runner = await client.workloads.start(workload)

        async with runner:
            result = await runner.exec_collect("sh", "-lc", "printf hello")
            print(result.stdout, result.exit_code)


asyncio.run(main())
```

## Low-Level APIs

For finer control, work directly with the resource clients:

```python
from capsule_sdk import CapsuleClient

with CapsuleClient(base_url="http://localhost:8080", token="my-token") as client:
    with client.runners.allocate_ready("my-workload-key") as runner:
        for event in runner.exec("echo", "hello"):
            if event.type == "stdout":
                print(event.data, end="")
```

Key low-level surfaces:

- `client.runners`
- `client.workloads`
- `client.snapshots`
- `client.runner_configs`

## Key Concepts

| SDK concept | Server primitive | Description |
|---|---|---|
| `RunnerConfig` | `LayeredConfig` | Declarative workload shape |
| `workloads.onboard()` | create + build | Register a workload from Python or YAML |
| `workloads.start()` | allocate + wait | Start a ready runner by workload name |
| `runners.allocate_ready()` | `/runners/allocate` | Allocate and wait for a usable runner |
| `RunnerSession` | runner handle | High-level exec, file, shell, pause, and resume API |

## Retry And Timeout Behavior

- `request_timeout` applies to a single HTTP request
- `startup_timeout` covers "get me a usable runner"
- `operation_timeout` applies to host-side file, PTY, and stream operations
- `allocate()` retries transient control-plane and capacity errors until `startup_timeout`
- `workloads.start()` is the preferred high-level path for named workloads
- `from_config()` waits for runner readiness by default; use `wait_ready=False` for lower-level control

## Host Reconnection

The SDK caches host addresses returned by `allocate()` and `connect()`. If a
host proxy becomes unavailable during a safe retryable operation, the SDK will
refresh the host via `connect()` and retry once when possible.

## Live End-To-End Test

The repository includes an explicit live SDK E2E at `sdk/python/tests/e2e_live.py`.
It exercises config registration, build enqueue, allocation, exec, file ops,
PTY, pause/resume, release, and config cleanup against a real control plane.

Run it with:

```bash
make sdk-python-e2e
```

If you are not using the default address:

```bash
CAPSULE_BASE_URL="http://localhost:8080" make sdk-python-e2e
```

## Development Checks

```bash
python -m ruff check src/capsule_sdk/ tests/
python -m pyright src/capsule_sdk/
python -m pytest tests/ -v --ignore=tests/e2e_live.py --ignore=tests/e2e_live_async.py
```

For contract tests against a live control plane:

```bash
CAPSULE_BASE_URL=http://localhost:8080 CAPSULE_TOKEN=test-token \
  python -m pytest tests/test_contract.py -v -m contract
```
