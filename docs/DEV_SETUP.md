# Development Setup

Local development guide for the Firecracker-based sandbox platform in this repository. You can build and test most components on macOS (Apple Silicon or Intel). Integration tests and actually running microVMs require Linux with KVM.

## Prerequisites

### Required

| Tool | Version | Install |
|------|---------|---------|
| Go | >= 1.24 | `brew install go` or [go.dev/dl](https://go.dev/dl/) |
| protoc | >= 3.21 | `brew install protobuf` |
| golangci-lint | latest | `brew install golangci-lint` |

### For infrastructure work

| Tool | Version | Install |
|------|---------|---------|
| Terraform | >= 1.0 | `brew install terraform` |
| Packer | >= 1.9 | `brew install packer` |
| gcloud CLI | latest | [cloud.google.com/sdk](https://cloud.google.com/sdk/docs/install) |
| kubectl | latest | `brew install kubectl` |
| Docker | latest | [docker.com](https://www.docker.com/products/docker-desktop/) |

## Initial Setup

```bash
# Clone the repository
git clone https://github.com/rahul-roy-glean/bazel-firecracker.git
cd bazel-firecracker

# Download Go modules and install protobuf codegen tools
make dev-setup
```

This runs `go mod download` and installs:
- `protoc-gen-go` (protobuf Go codegen)
- `protoc-gen-go-grpc` (gRPC Go codegen)
- `buf` (protobuf linting/generation)

## Python SDK

The Python SDK is a tier-1 surface and has its own local workflow:

```bash
cd sdk/python
python3 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev]"

python -m ruff check src/bf_sdk/ tests/
python -m pyright src/bf_sdk/
python -m pytest tests/ -v --ignore=tests/e2e_live.py
```

For the live control-plane-backed contract tests, export `BF_BASE_URL` and `BF_TOKEN`:

```bash
BF_BASE_URL=http://localhost:8080 BF_TOKEN=test-token \
  python -m pytest tests/test_contract.py -v -m contract
```

## Building

```bash
# Build all binaries (output to bin/)
make build

# Build a specific binary
make firecracker-manager
make control-plane
make thaw-agent
make snapshot-builder

# Cross-compile firecracker-manager for Linux (needed for Packer image builds from macOS)
make firecracker-manager-linux
```

The `thaw-agent` always cross-compiles for Linux (`CGO_ENABLED=0 GOOS=linux`) since it runs inside microVMs.

## Testing

```bash
# Unit tests -- works on macOS and Linux, no infrastructure needed
make test-unit

# Unit tests with race detector
make test-race

# Unit tests with coverage report
make test-cover

# Integration tests -- requires Linux with KVM
make test-integration

# All tests
make test-all

# Pre-commit check (build + unit tests)
make check
```

Test files live alongside the code they test:

```
cmd/firecracker-manager/server_test.go
cmd/firecracker-manager/handlers_test.go
cmd/thaw-agent/helpers_test.go
cmd/thaw-agent/mmds_test.go
pkg/runner/manager_test.go
pkg/runner/pool_test.go
pkg/snapshot/lru_cache_test.go
pkg/snapshot/eager_fetch_test.go
```

## Protobuf

The gRPC service definition is at `api/proto/runner.proto`. After modifying it:

```bash
# Generate Go code (uses protoc)
make proto

# Alternative: generate using buf
make proto-buf
```

Generated files go to `api/proto/runner/`.

## Linting

```bash
make lint
```

Uses `golangci-lint`. Configure in `.golangci.yml` if needed.

## Running Locally

### Control Plane

Requires a PostgreSQL instance. Easiest locally with Docker:

```bash
# Start local PostgreSQL
docker run -d --name fc-postgres \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=firecracker_runner \
  -p 5432:5432 \
  postgres:15

# Run the control plane
make run-control-plane
```

This connects to `localhost:5432` with default credentials. The control plane serves gRPC on `:50051` and HTTP on `:8080`.

### Firecracker Manager

The host agent requires root for networking (TAP devices, iptables). On macOS it will build and start but cannot actually create microVMs (no KVM).

```bash
sudo make run-host-agent
```

Flags you may want to override for local development:
- `--snapshot-bucket` -- GCS bucket with snapshots
- `--snapshot-cache` -- local directory for cached snapshot files
- `--socket-dir` -- directory for Firecracker sockets
- `--log-dir` -- log output directory

### Thaw Agent

The thaw agent runs inside microVMs and reads configuration from Firecracker's MMDS (Metadata Data Store). It cannot be meaningfully run outside a microVM, but you can build and test it:

```bash
make thaw-agent
make test-unit  # runs thaw-agent unit tests
```

## Project Layout

```
cmd/                     One directory per binary (main packages)
pkg/                     Shared library packages
api/proto/               gRPC definitions + generated code
deploy/
  terraform/             GCP infrastructure
  kubernetes/            K8s manifests for control plane
  helm/                  Helm charts (alternative)
  packer/                GCE host VM image
  docker/                Dockerfiles
images/microvm/          MicroVM rootfs build scripts
docs/                    Documentation
```

## Key Concepts

### Snapshot Lifecycle

1. `snapshot-builder` boots a Firecracker VM from a Docker `base_image`
2. The `thaw-agent` runs the layered warmup commands inside the guest
3. The VM is paused and its memory + disk state are saved as a snapshot
4. The snapshot is uploaded to GCS with workload-keyed metadata
5. `firecracker-manager` on each host syncs the manifests or chunks it needs
6. On allocation, the manager restores or resumes a microVM in ~100-500ms
7. The `thaw-agent` wakes up in restore mode, configures networking, and launches the configured `start_command`

### MMDS

Firecracker's Metadata Data Store (MMDS) passes configuration to the guest VM via a virtual network device at `169.254.169.254` (when IP is this, instead of routing this the request through TAP, firecracker has a user mode TCP impl to handle these. TAP, terminal access point, for Guest OS is like a ethernet adaptor, but for host OS, its just a file description where firecracker is writing to and reading from). The manager writes runner config (network, job details, git-cache paths) into MMDS before starting or restoring a VM. The thaw agent reads it on boot.

### Runner Pooling

When enabled, completed runners are paused instead of destroyed. The next job with a matching pool key reuses the paused VM, skipping the snapshot restore entirely.

### Git Cache

An ext4 image containing bare git mirrors of the target repository. Attached as a read-only block device to each microVM. The thaw agent uses it as a `--reference` for `git clone`, avoiding network fetches for most objects. Enables fast cloning of private repos without network auth tokens in the snapshot. When git tries to fetch any object, it first look at the referenced local path, and if object is present at the referenced path it reduces network i/o.

## Useful Make Targets

Run `make help` for the full list. Most commonly used during development:

| Target | What it does |
|--------|--------------|
| `make build` | Build all binaries to `bin/` |
| `make test-unit` | Run unit tests |
| `make test-race` | Unit tests with race detector |
| `make check` | Build + unit tests (pre-commit gate) |
| `make lint` | Run golangci-lint |
| `make proto` | Regenerate protobuf code |
| `make clean` | Remove `bin/` and coverage files |
