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

```python
from bf_sdk import BFClient

with BFClient(base_url="http://localhost:8080", api_key="my-key") as client:
    # Allocate a runner
    runner = client.runners.allocate("my-workload-key")
    print(f"Runner {runner.runner_id} on {runner.host_address}")

    # Wait for it to be ready
    client.runners.wait_ready(runner.runner_id)

    # Execute a command (streaming)
    for event in client.runners.exec(runner.runner_id, ["echo", "hello"]):
        if event.type == "stdout":
            print(event.data, end="")
        elif event.type == "exit":
            print(f"Exit code: {event.code}")

    # Open a PTY shell
    with client.runners.shell(runner.runner_id) as shell:
        shell.send("ls -la\n")
        data = shell.recv_stdout(timeout=5.0)
        print(data.decode())

    # Pause and resume
    pause = client.runners.pause(runner.runner_id)
    print(f"Paused session {pause.session_id}")

    conn = client.runners.connect(runner.runner_id)
    print(f"Reconnected: {conn.status}")

    # Release
    client.runners.release(runner.runner_id)
```

## Snapshot Config Management

```python
from bf_sdk import BFClient

with BFClient(base_url="http://localhost:8080") as client:
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

## Host Reconnection

The SDK caches host addresses from `allocate()` and `connect()` responses. If a host agent returns 503 during `exec()`, the SDK automatically calls `connect()` to get a new host and retries (only if no output has been received yet).
