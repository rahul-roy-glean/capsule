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

## Quickstart

The fastest way to get started is the declarative **RunnerConfig** API — declare your runner shape once, build and tag it, then spawn runners from the tag:

```python
from bf_sdk import BFClient, RunnerConfig

# 1. Define your runner config
cfg = (
    RunnerConfig("my-workload")
    .with_display_name("My dev sandbox")
    .with_commands(["apt-get install -y python3", "pip install -e .[dev]"])
    .with_tier("small")
    .with_runner_ttl(3600)
    .with_auto_pause(True)
)

with BFClient(base_url="http://localhost:8080", api_key="my-key") as bf:
    # 2. Register the config, build a snapshot, and tag it
    bf.runner_configs.apply(cfg)
    bf.runner_configs.build(cfg, tag="dev")
    bf.runner_configs.promote(cfg, tag="dev", to="stable")

    # 3. Spawn a runner from the tag (auto-released on exit)
    with bf.runners.from_config("my-workload", tag="stable") as r:
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

## Snapshot Config Management

```python
from bf_sdk import BFClient

with BFClient() as client:
    # List configs
    configs = client.snapshot_configs.list()

    # Create a config
    config = client.snapshot_configs.create(
        display_name="My Workload",
        commands=[{"command": "apt-get install -y python3"}],
        ci_system="none",
    )

    # Trigger a build
    build = client.snapshot_configs.trigger_build(config.workload_key)

    # Tag and promote
    client.snapshot_configs.create_tag(
        config.workload_key, tag="stable", version=build.version
    )
    client.snapshot_configs.promote(config.workload_key, tag="stable")
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
| `RunnerConfig` | SnapshotConfig | Declarative runner shape (commands, tier, TTL, etc.) |
| `runner_configs.build()` | `/snapshot-configs/{wk}/build` | Build a snapshot image from a config |
| `runner_configs.promote()` | Tag create | Copy a tag's version to a target tag (e.g. dev -> stable) |
| `runners.from_config()` | `/runners/allocate` | Spawn a runner from a tagged config |
| `RunnerSession` | Runner ID + host cache | High-level handle with exec/shell/pause/resume |

## Host Reconnection

The SDK caches host addresses from `allocate()` and `connect()` responses. If a host agent returns 503 during `exec()`, the SDK automatically calls `connect()` to get a new host and retries (only if no output has been received yet).
