## Capsule TypeScript SDK

Production-oriented TypeScript and JavaScript SDK for Capsule.

This SDK mirrors the current Capsule control-plane and manager APIs used by the
Python SDK, with a workload-first surface for onboarding workloads, allocating
runners, executing commands, interacting with files, and opening PTY sessions.

### Install

```bash
npm install capsule-sdk
```

Node.js 20+ is required.

### Configuration

The client can be configured explicitly:

```ts
import { CapsuleClient } from "capsule-sdk";

const client = new CapsuleClient({
  baseUrl: "http://localhost:8080",
  token: "test-token",
});
```

Or through environment variables:

- `CAPSULE_BASE_URL`
- `CAPSULE_TOKEN`
- `CAPSULE_REQUEST_TIMEOUT`
- `CAPSULE_STARTUP_TIMEOUT`
- `CAPSULE_OPERATION_TIMEOUT`

Timeout values are in milliseconds.

### Quickstart

```ts
import { CapsuleClient, RunnerConfig } from "capsule-sdk";

const client = new CapsuleClient({
  baseUrl: "http://localhost:8080",
  token: "test-token",
});

const config = new RunnerConfig("hello-service")
  .withBaseImage("ubuntu:22.04")
  .withCommands([
    "apt-get update",
    "apt-get install -y python3",
  ])
  .withTier("m")
  .withAutoPause(true)
  .withTtl(300);

const workload = await client.workloads.onboard(config, { build: false });
const runner = await client.workloads.start(workload, { waitReady: false });

try {
  await runner.waitReady();
  const result = await runner.execCollect("python3", "-c", "print('hello')");
  console.log(result.stdout, result.exitCode);
} finally {
  await runner.release();
  await client.close();
}
```

### Primary surfaces

- `client.workloads`
  - onboard workloads from `RunnerConfig`, JSON, YAML text, or YAML file
  - list, get, build, delete workloads
  - start or allocate runners from workload references
- `client.runners`
  - allocate, poll, release, pause, quarantine
  - file APIs
  - command execution streaming
  - PTY shell sessions
- `client.runnerConfigs`
  - apply and build layered workload configs
- `client.snapshots`
  - list snapshots

### Workload references

The SDK accepts multiple workload reference shapes where practical:

- display name string
- control-plane workload key string
- `WorkloadSummary`
- `CreateConfigResponse`
- `StoredLayeredConfig`
- `LayeredConfigDetail`
- `RunnerConfig`

### Files

```ts
const runner = await client.runners.fromConfig("my-workload");

await runner.writeFile("/workspace/hello.txt", "hello");
const text = await runner.readText("/workspace/hello.txt");
const files = await runner.listFiles("/workspace");
const blob = await runner.download("/workspace/hello.txt");
await runner.upload("/workspace/data.bin", blob);
```

### Exec

```ts
for await (const event of runner.exec("bash", "-lc", "echo hi && echo bye >&2")) {
  if (event.type === "stdout") {
    process.stdout.write(event.data ?? "");
  }
  if (event.type === "stderr") {
    process.stderr.write(event.data ?? "");
  }
}
```

Collect output:

```ts
const result = await runner.execCollect("bash", "-lc", "echo hi");
console.log(result.stdout);
console.log(result.exitCode);
```

### PTY shell

```ts
const shell = runner.shell({ cols: 120, rows: 40 });
await shell.connect();
try {
  await shell.send("echo hello\n");
  const frame = await shell.recv();
  if (frame.type === 0x01) {
    console.log(frame.payload.toString("utf8"));
  }
} finally {
  await shell.close();
}
```

### Errors

The SDK raises typed errors for common failure modes:

- `CapsuleAuthError`
- `CapsuleNotFound`
- `CapsuleConflict`
- `CapsuleRateLimited`
- `CapsuleServiceUnavailable`
- `CapsuleConnectionError`
- `CapsuleRequestTimeoutError`
- `CapsuleOperationTimeoutError`
- `CapsuleAllocationTimeoutError`
- `CapsuleRunnerUnavailableError`

### Development

```bash
npm install
npm run typecheck
npm run build
npm test
npm run test:contract
```

### Contract tests

The contract test suite runs against a live control-plane instance and uses:

- `CAPSULE_BASE_URL`
- `CAPSULE_TOKEN`

Example:

```bash
CAPSULE_BASE_URL=http://localhost:8080 CAPSULE_TOKEN=test-token npm run test:contract
```
