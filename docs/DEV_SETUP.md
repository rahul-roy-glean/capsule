# Development Setup

This guide covers local development for Capsule. You can build and test most of
the repository on macOS or Linux, but running real Firecracker microVMs and KVM
integration tests requires Linux with nested virtualization support.

## Supported Local Workflows

- `macOS`: build binaries, run unit tests, work on the Python SDK, edit docs,
  and validate most control-plane logic
- `Linux with KVM`: everything above plus host-agent and microVM integration work

## Prerequisites

### Required for most work

| Tool | Version | Notes |
|---|---|---|
| Go | `>= 1.24` | Build and test the Go services |
| `protoc` | `>= 3.21` | Generate protobuf bindings |
| `golangci-lint` | latest | Run Go lint checks |

### Required for infra and deployment work

| Tool | Version | Notes |
|---|---|---|
| Terraform | `>= 1.0` | Provision GCP infrastructure |
| Packer | `>= 1.9` | Build the host image |
| gcloud CLI | latest | GCP auth and operational commands |
| kubectl | latest | Inspect the deployed control plane |
| Docker | latest | Build images and run local dependencies |

## Initial Setup

```bash
git clone https://github.com/rahul-roy-glean/capsule.git
cd capsule
make dev-setup
```

`make dev-setup` downloads Go modules and installs the protobuf tooling used by
the repo:

- `protoc-gen-go`
- `protoc-gen-go-grpc`
- `buf`

## Recommended Contribution Loop

For most code changes:

```bash
make build
make test-unit
make check
make lint
```

If you touch protobufs:

```bash
make proto
```

If you touch the Python SDK:

```bash
cd sdk/python
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev]"
python -m ruff check src/capsule_sdk/ tests/
python -m pyright src/capsule_sdk/
python -m pytest tests/ -v --ignore=tests/e2e_live.py --ignore=tests/e2e_live_async.py
```

## Python SDK Workflow

The Python SDK is a first-class public surface and should be treated like a
separate package within the repo.

### Local setup

```bash
cd sdk/python
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev]"
```

### Day-to-day checks

```bash
python -m ruff check src/capsule_sdk/ tests/
python -m pyright src/capsule_sdk/
python -m pytest tests/ -v --ignore=tests/e2e_live.py --ignore=tests/e2e_live_async.py
```

### Contract tests against a live control plane

```bash
CAPSULE_BASE_URL=http://localhost:8080 CAPSULE_TOKEN=test-token \
  python -m pytest tests/test_contract.py -v -m contract
```

### Optional local hooks

```bash
python3 -m pip install pre-commit
pre-commit install
pre-commit install --hook-type pre-push
```

These hooks run fast formatting, build, and SDK checks locally before CI.

## Building

```bash
# Build all binaries into bin/
make build

# Build specific binaries
make capsule-control-plane
make capsule-manager
make capsule-thaw-agent
make snapshot-builder
```

Useful platform-specific note:

- `capsule-thaw-agent` always cross-compiles for Linux because it runs inside the guest VM
- `capsule-manager` can be cross-compiled for Linux from macOS via `make capsule-manager-linux`

## Testing

```bash
# Fast unit tests, works on macOS and Linux
make test-unit

# Race detector
make test-race

# Coverage
make test-cover

# Integration tests, requires Linux with KVM
make test-integration

# Everything
make test-all
```

`make check` is the preferred pre-push gate for most Go changes.

## Protobuf Workflow

The service definition lives in `api/proto/runner.proto`.

After editing it:

```bash
make proto
```

Alternative:

```bash
make proto-buf
```

Generated files live under `api/proto/runner/`.

## Running Components Locally

### Control plane

The easiest local setup is PostgreSQL plus the control plane.

```bash
docker run -d --name capsule-postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=capsule \
  -p 5432:5432 \
  postgres:15

make run-control-plane
```

By default the control plane serves:

- gRPC on `:50051`
- HTTP on `:8080`

### Host agent

The host agent requires root privileges for TAP devices, routing, and other
network setup. It can build on macOS, but it cannot actually create Firecracker
microVMs there.

```bash
sudo make run-host-agent
```

Common flags to override for local work:

- `--snapshot-bucket`
- `--snapshot-cache`
- `--socket-dir`
- `--log-dir`

### Guest agent

`capsule-thaw-agent` runs inside the microVM. It is not useful as a standalone
host process, but it is easy to build and unit-test in isolation:

```bash
make capsule-thaw-agent
make test-unit
```

## Project Layout

```text
cmd/                     main packages, one per binary
pkg/                     shared Go packages
api/proto/               gRPC definitions and generated code
deploy/
  terraform/             GCP infrastructure
  kubernetes/            standalone Kubernetes manifests
  helm/                  Helm chart(s)
  packer/                host-image build
  docker/                container Dockerfiles
images/microvm/          guest rootfs build assets
sdk/python/              Python SDK
examples/                deployable workload configs
docs/                    user and operator documentation
```

## Runtime Notes For Contributors

### Snapshot lifecycle

At a high level:

1. `snapshot-builder` boots a Firecracker VM from a Docker `base_image`
2. `capsule-thaw-agent` runs the warmup commands inside the guest
3. the VM is paused and its memory plus disk state are captured
4. the snapshot is chunked and uploaded to GCS
5. `capsule-manager` on each host restores or resumes a VM from that state

### MMDS

Firecracker MMDS is how the host passes runtime configuration into the guest.
Before boot or resume, the host agent writes runner metadata into MMDS. The
guest agent reads that data to configure networking, runtime options, and the
user `start_command`.

### Runner pooling

When pooling is enabled, completed runners can be paused instead of destroyed.
The next compatible allocation can reuse that paused VM and avoid a full
snapshot restore.

### Git cache pattern

Several examples use a read-only ext4 drive containing bare git mirrors. The
guest mounts that drive and uses `git clone --reference` to avoid refetching
objects that are already present locally.

## Useful Make Targets

| Target | Purpose |
|---|---|
| `make build` | Build all binaries |
| `make test-unit` | Run unit tests |
| `make test-race` | Run race-enabled tests |
| `make check` | Build plus unit tests |
| `make lint` | Run Go lint checks |
| `make proto` | Regenerate protobufs |
| `make clean` | Remove build artifacts |
