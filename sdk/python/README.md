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

The fastest way to get started is the declarative **RunnerConfig** API — declare your runner shape once, build it, then spawn runners:

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

with BFClient(base_url="http://localhost:8080", api_key="my-key") as bf:
    # 2. Register the config and build
    result = bf.runner_configs.apply(cfg)
    bf.runner_configs.build(result.config_id)

    # 3. Spawn a runner from the config (auto-released on exit)
    with bf.runners.from_config(result.leaf_workload_key) as r:
        # Run a command and collect output
        output, code = r.exec_collect("python", "-c", "print('hello')")
        print(output, code)

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

## Low-level API

For full control, use the resource APIs directly:

```python
from bf_sdk import BFClient

with BFClient(base_url="http://localhost:8080", api_key="my-key") as client:
    # Allocate a runner
    runner = client.runners.allocate("my-workload-key")
    client.runners.wait_ready(runner.runner_id)

    # Execute a command (streaming ndjson)
    for event in client.runners.exec(runner.runner_id, ["echo", "hello"]):
        if event.type == "stdout":
            print(event.data, end="")
        elif event.type == "exit":
            print(f"Exit code: {event.code}")

    # PTY shell
    with client.runners.shell(runner.runner_id) as shell:
        shell.send("ls -la\n")
        data = shell.recv_stdout(timeout=5.0)
        print(data.decode())

    # Pause and resume
    pause = client.runners.pause(runner.runner_id)
    conn = client.runners.connect(runner.runner_id)

    # Release
    client.runners.release(runner.runner_id)
```

## Layered Config Management

```python
from bf_sdk import BFClient

with BFClient() as client:
    # List configs
    configs = client.layered_configs.list()

    # Create a layered config
    result = client.layered_configs.create({
        "display_name": "My Workload",
        "base_image": "ubuntu:22.04",
        "layers": [
            {"name": "deps", "init_commands": [{"type": "shell", "args": ["bash", "-lc", "apt-get install -y python3"]}]},
            {"name": "app", "init_commands": [{"type": "shell", "args": ["bash", "-lc", "pip install ."]}]},
        ],
        "config": {"tier": "m", "auto_rollout": True},
    })

    # Trigger a build
    build = client.layered_configs.build(result.config_id)

    # Get config details with layer statuses
    detail = client.layered_configs.get(result.config_id)
    for layer in detail.layers or []:
        print(f"{layer.name}: {layer.status}")

    # Refresh a specific layer
    client.layered_configs.refresh_layer(result.config_id, "deps")
```

## Configuration

| Parameter | Env var | Default |
|---|---|---|
| `base_url` | `BF_BASE_URL` | `http://localhost:8080` |
| `api_key` | `BF_API_KEY` | `None` |
| `timeout` | - | `30.0` |

## Key Concepts

| SDK concept | Server primitive | Description |
|---|---|---|
| `RunnerConfig` | LayeredConfig | Declarative runner shape (layers, tier, TTL, etc.) |
| `runner_configs.build()` | `/layered-configs/{id}/build` | Build all layers in a config |
| `layered_configs.refresh_layer()` | `/layered-configs/{id}/layers/{name}/refresh` | Refresh a specific layer |
| `runners.from_config()` | `/runners/allocate` | Spawn a runner from a config |
| `RunnerSession` | Runner ID + host cache | High-level handle with exec/shell/pause/resume |

## Host Reconnection

The SDK caches host addresses from `allocate()` and `connect()` responses. If a host agent returns 503 during `exec()`, the SDK automatically calls `connect()` to get a new host and retries (only if no output has been received yet).

Layer commands use the control-plane `SnapshotCommand` shape under the hood:
`{"type": "shell", "args": ["bash", "-lc", "echo hi"]}`. The shorthand
`RunnerConfig.with_commands(["echo hi"])` is normalized to that wire format for you.
