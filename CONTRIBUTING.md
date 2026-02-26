# Contributing to bazel-firecracker

Thank you for your interest in contributing to bazel-firecracker, a sub-second VM snapshot/restore platform built on Firecracker for GitHub Actions.

## Table of Contents

- [Development Environment Setup](#development-environment-setup)
- [Code Style](#code-style)
- [Linux-Only Code Pattern](#linux-only-code-pattern)
- [Running Tests](#running-tests)
- [Pull Request Process](#pull-request-process)
- [Commit Message Conventions](#commit-message-conventions)
- [Reporting Issues](#reporting-issues)

---

## Development Environment Setup

See [docs/DEV_SETUP.md](docs/DEV_SETUP.md) for full instructions. The short version:

**Prerequisites:**
- Go >= 1.24
- Docker (for rootfs builds)
- On Linux: KVM access for integration tests (`ls /dev/kvm`)
- On macOS: [Lima](https://github.com/lima-vm/lima) for a local Linux VM with KVM

**Quick start:**

```bash
# Clone
git clone https://github.com/your-org/bazel-firecracker.git
cd bazel-firecracker

# Download dependencies
go mod download

# Build all binaries (cross-compiled for linux/amd64)
make build

# Run unit tests (works on macOS and Linux)
make test-unit
```

**macOS + Lima (for integration tests):**

```bash
make dev-up        # Start Lima VM (one-time, ~3 min)
make dev-build     # Build binaries + dev rootfs inside Lima VM
make dev-snapshot  # Build full snapshot inside Lima VM
make dev-stack     # Start control-plane + firecracker-manager
make dev-test-exec # Run E2E exec test
make dev-stop      # Stop the stack
```

**Linux / GCE (for integration tests):**

```bash
make dev-provision    # Install prerequisites (needs sudo, run once)
make dev-build-local  # Build binaries + dev rootfs
make dev-stack-local  # Start control-plane + firecracker-manager
make dev-test-exec-local # Run E2E exec test
make dev-stop-local   # Stop the stack
```

---

## Code Style

This project uses standard Go tooling for code style:

- **gofmt**: All Go code must be formatted with `gofmt`. Run `gofmt -w .` before committing.
- **golangci-lint**: Linting is enforced in CI. Run `golangci-lint run ./...` locally to check. The pinned version is in `.pre-commit-config.yaml`.
- **go vet**: Static analysis via `go vet ./...`.

**Pre-commit hooks** (optional but recommended):

```bash
pip install pre-commit
pre-commit install
```

This runs `gofmt`, `go vet`, `go mod tidy`, and `go build` on every commit.

**General conventions:**
- Keep functions focused and small.
- Export only what is needed by other packages.
- Write tests for all new functionality.
- Add comments to all exported types and functions.
- Prefer explicit error handling over panics.

---

## Linux-Only Code Pattern

Much of this project (UFFD handlers, Firecracker integration, FUSE mounts) only compiles on Linux. The convention is:

1. The main implementation lives in a file with a `//go:build linux` build tag.
2. A companion `_stub.go` file provides stub implementations for non-Linux platforms.

**Example:**

`pkg/uffd/handler.go`:
```go
//go:build linux

package uffd

// ... Linux-specific implementation
```

`pkg/uffd/handler_stub.go`:
```go
//go:build !linux

package uffd

// ... stub that returns errors or panics with a clear message
```

When adding Linux-only code, always provide the stub counterpart so that `go build ./...` succeeds on macOS and Windows.

---

## Running Tests

```bash
# Unit tests (works on macOS + Linux, no infra needed)
make test-unit

# Unit tests with race detector
make test-race

# Unit tests with coverage report
make test-cover

# Integration tests (Linux + KVM required)
make test-integration

# All tests (unit + integration)
make test-all

# Pre-commit check: build + unit tests
make check
```

Integration tests require a Linux host with `/dev/kvm`. They are tagged with `//go:build integration` and are skipped in normal `go test` runs.

---

## Pull Request Process

1. **Fork** the repository and create a branch from `main`:
   ```bash
   git checkout -b feat/your-feature-name
   ```

2. **Make your changes** following the code style guidelines above.

3. **Add or update tests** for the changes you make.

4. **Ensure all checks pass:**
   ```bash
   make check   # build + unit tests
   make lint    # golangci-lint
   ```

5. **Push** your branch and open a **Pull Request** against `main`.

6. Fill out the pull request description with:
   - What the change does and why
   - How you tested it
   - Any known limitations or follow-up work

7. **Request a review.** At least one maintainer must approve before merging.

8. PRs are merged via **squash merge** to keep the history clean.

---

## Commit Message Conventions

Follow the [Conventional Commits](https://www.conventionalcommits.org/) format:

```
<type>(<scope>): <short summary>

[optional body]

[optional footer]
```

**Types:**
- `feat`: A new feature
- `fix`: A bug fix
- `refactor`: Code change that neither fixes a bug nor adds a feature
- `test`: Adding or updating tests
- `docs`: Documentation changes only
- `ci`: CI/CD configuration changes
- `chore`: Build process, dependency updates, or other maintenance

**Examples:**
```
feat(runner): add pause/resume support for chunked snapshots
fix(uffd): handle SIGBUS correctly on memory fault
docs(setup): update Terraform variable names
ci: pin golangci-lint to v1.64.8
```

Keep the summary line under 72 characters. Use the body for motivation and context.

---

## Reporting Issues

- **Bugs**: Open a [GitHub Issue](../../issues/new) with a clear title, steps to reproduce, expected vs. actual behavior, and relevant logs.
- **Feature requests**: Open a GitHub Issue describing the use case and proposed solution.
- **Security vulnerabilities**: See [SECURITY.md](SECURITY.md). Do **not** open a public issue.

Before opening an issue, please search existing issues to avoid duplicates.
