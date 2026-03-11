.PHONY: all build test clean proto docker-build docker-push docker-push-control-plane docker-push-snapshot-builder
.PHONY: terraform-infra-init terraform-infra-plan terraform-infra-apply terraform-infra-destroy
.PHONY: terraform-app-init terraform-app-plan terraform-app-apply terraform-app-destroy
.PHONY: packer-init packer-validate packer-build firecracker-manager-linux release-host-image mig-rolling-update
.PHONY: onboard onboard-validate onboard-plan bin-onboard
.PHONY: firecracker-manager control-plane snapshot-builder thaw-agent
.PHONY: test-unit test-race test-cover test-integration test-all check
.PHONY: sdk-python-lint sdk-python-test sdk-python-typecheck sdk-python-e2e
.PHONY: dev-build dev-snapshot dev-stack dev-test-snapshot-builder dev-test-pause-resume dev-test-multi-pause-dedup dev-stop
.PHONY: dev-test-gcs-pause-resume
.PHONY: dev-test-file-ops dev-test-pty dev-test-checkpoint
.PHONY: dev-test-auto-resume dev-test-template-tags dev-test-network-policy dev-test-auth-proxy
.PHONY: dev-agent-rootfs dev-agent-snapshot dev-test-agent-sessions dev-run-agent-e2e
.PHONY: dev-setup dev-provision
.PHONY: bench-allocate bench-session dev-bench-allocate dev-bench-session

# Variables
PROJECT_ID ?= your-project-id
REGION ?= us-central1
ENV ?= dev
ZONE ?= us-central1-a
CONFIG ?= onboard.yaml
REGISTRY ?= $(REGION)-docker.pkg.dev/$(PROJECT_ID)/firecracker
VERSION ?= $(shell git describe --tags --always --dirty)

# Go build settings
GO := go
GOFLAGS := -trimpath -ldflags "-X main.version=$(VERSION)"
GOOS_TARGET ?= linux
GOARCH_TARGET ?= amd64
export PATH := /usr/local/go/bin:$(PATH)

# Binaries
BINARIES := firecracker-manager control-plane snapshot-builder thaw-agent bin-onboard workload-key

all: build

# Build all binaries
build: $(BINARIES)

CROSS_BUILD = CGO_ENABLED=0 GOOS=$(GOOS_TARGET) GOARCH=$(GOARCH_TARGET)

firecracker-manager:
	$(CROSS_BUILD) $(GO) build $(GOFLAGS) -o bin/firecracker-manager ./cmd/firecracker-manager

control-plane:
	$(CROSS_BUILD) $(GO) build $(GOFLAGS) -o bin/control-plane ./cmd/control-plane

snapshot-builder:
	$(CROSS_BUILD) $(GO) build $(GOFLAGS) -o bin/snapshot-builder ./cmd/snapshot-builder

thaw-agent:
	$(CROSS_BUILD) $(GO) build $(GOFLAGS) -o bin/thaw-agent ./cmd/thaw-agent

bin-onboard:
	$(CROSS_BUILD) $(GO) build $(GOFLAGS) -o bin/onboard ./cmd/onboard

workload-key:
	$(GO) build $(GOFLAGS) -o bin/workload-key ./cmd/workload-key

bench-allocate:
	$(GO) build $(GOFLAGS) -o bin/bench-allocate ./cmd/bench-allocate

bench-session:
	$(GO) build $(GOFLAGS) -o bin/bench-session ./cmd/bench-session

onboard: bin-onboard
	./bin/onboard --config=$(CONFIG) $(if $(STEPS),--steps=$(STEPS))

onboard-validate: bin-onboard
	./bin/onboard --config=$(CONFIG) --dry-run

onboard-plan: bin-onboard
	./bin/onboard --config=$(CONFIG) --plan $(if $(STEPS),--steps=$(STEPS))

# Generate protobuf code
.PHONY: proto proto-buf proto-protoc
proto: proto-protoc

# Generate using buf (preferred)
proto-buf:
	@command -v buf >/dev/null 2>&1 || { echo "buf not found, install with: go install github.com/bufbuild/buf/cmd/buf@latest"; exit 1; }
	buf generate api/proto

# Generate using protoc (recommended)
proto-protoc:
	@mkdir -p api/proto/runner
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/proto/runner.proto
	@mv api/proto/runner.pb.go api/proto/runner/ 2>/dev/null || true
	@mv api/proto/runner_grpc.pb.go api/proto/runner/ 2>/dev/null || true
	@echo "Proto files generated in api/proto/runner/"

# Run tests
test:
	$(GO) test -v ./...

# Run tests with coverage
test-coverage:
	$(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

# Lint
lint:
	golangci-lint run ./...

# Clean build artifacts
clean:
	rm -rf bin/
	rm -f coverage.out coverage.html

# Docker builds
docker-build: docker-build-control-plane docker-build-snapshot-builder

docker-build-control-plane:
	docker buildx build --platform linux/amd64 --load \
		-t $(REGISTRY)/firecracker-control-plane:$(VERSION) \
		-t $(REGISTRY)/firecracker-control-plane:latest \
		-f deploy/docker/Dockerfile.control-plane .

docker-build-snapshot-builder:
	docker buildx build --platform linux/amd64 --load \
		-t $(REGISTRY)/firecracker-snapshot-builder:$(VERSION) \
		-t $(REGISTRY)/firecracker-snapshot-builder:latest \
		-f deploy/docker/Dockerfile.snapshot-builder .

docker-push: docker-push-control-plane docker-push-snapshot-builder

docker-push-control-plane:
	docker push $(REGISTRY)/firecracker-control-plane:$(VERSION)
	docker push $(REGISTRY)/firecracker-control-plane:latest

docker-push-snapshot-builder:
	docker push $(REGISTRY)/firecracker-snapshot-builder:$(VERSION)
	docker push $(REGISTRY)/firecracker-snapshot-builder:latest

# Build microVM rootfs
rootfs:
	cd images/microvm && ./build-rootfs.sh

# Terraform - Infrastructure (Stage 1)
terraform-infra-init:
	cd deploy/terraform/infra && terraform init

terraform-infra-plan:
	cd deploy/terraform/infra && terraform plan

terraform-infra-apply:
	cd deploy/terraform/infra && terraform apply

terraform-infra-destroy:
	cd deploy/terraform/infra && terraform destroy

# Terraform - Application (Stage 2)
terraform-app-init:
	cd deploy/terraform/app && terraform init

terraform-app-plan:
	cd deploy/terraform/app && terraform plan

terraform-app-apply:
	cd deploy/terraform/app && terraform apply

terraform-app-destroy:
	cd deploy/terraform/app && terraform destroy

# Packer
packer-init:
	cd deploy/packer && packer init .

packer-validate: firecracker-manager-linux
	cd deploy/packer && packer validate \
		-var="project_id=$(PROJECT_ID)" \
		-var="firecracker_manager_binary=../../bin/firecracker-manager" \
		-var="network=fc-runner-$(ENV)-vpc" \
		-var="subnetwork=fc-runner-$(ENV)-hosts" \
		host-image.pkr.hcl

packer-build: firecracker-manager-linux packer-init
	cd deploy/packer && packer build \
		-var="project_id=$(PROJECT_ID)" \
		-var="firecracker_manager_binary=../../bin/firecracker-manager" \
		-var="network=fc-runner-$(ENV)-vpc" \
		-var="subnetwork=fc-runner-$(ENV)-hosts" \
		host-image.pkr.hcl

# Cross-compile firecracker-manager for Linux (for Packer builds from macOS)
firecracker-manager-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o bin/firecracker-manager ./cmd/firecracker-manager
	@echo "Built bin/firecracker-manager (linux/amd64)"

# Full release: build binary, build image, update MIG
.PHONY: release-host-image
release-host-image: packer-build
	@echo ""
	@echo "=== Host image built successfully ==="
	@echo "Image family: firecracker-host"
	@echo ""
	@echo "To roll out to the MIG, run:"
	@echo "  make mig-rolling-update"

# Rolling update the MIG to use the latest image
.PHONY: mig-rolling-update
mig-rolling-update:
	@echo "Starting rolling update of host MIG..."
	$(eval TEMPLATE := $(shell gcloud compute instance-templates list \
		--project=$(PROJECT_ID) \
		--filter="name~'^fc-runner-$(ENV)-host-'" \
		--format="value(name)" \
		--sort-by=~creationTimestamp \
		--limit=1))
	@echo "Using template: $(TEMPLATE)"
	gcloud compute instance-groups managed rolling-action start-update \
		fc-runner-$(ENV)-hosts \
		--version=template=$(TEMPLATE) \
		--region=$(REGION) \
		--project=$(PROJECT_ID) \
		--max-surge=3 \
		--max-unavailable=0
	@echo ""
	@echo "Rolling update initiated. Monitor with:"
	@echo "  gcloud compute instance-groups managed list-instances fc-runner-$(ENV)-hosts --region=$(REGION)"

# Kubernetes
k8s-deploy:
	kubectl apply -f deploy/kubernetes/

k8s-delete:
	kubectl delete -f deploy/kubernetes/

# Development helpers
dev-setup:
	$(GO) mod download
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	$(GO) install github.com/bufbuild/buf/cmd/buf@latest

# Run locally (for development)
run-control-plane:
	$(GO) run ./cmd/control-plane \
		--db-host=localhost \
		--db-port=5432 \
		--db-user=postgres \
		--db-password=postgres \
		--db-name=firecracker_runner \
		--gcs-bucket=$(PROJECT_ID)-firecracker-snapshots

run-host-agent:
	sudo $(GO) run ./cmd/firecracker-manager \
		--snapshot-bucket=$(PROJECT_ID)-firecracker-snapshots \
		--snapshot-cache=/tmp/snapshots \
		--socket-dir=/tmp/firecracker \
		--log-dir=/tmp/firecracker-logs

# Data snapshot targets (disk snapshot approach - fast host boot)
.PHONY: data-snapshot-build data-snapshot-check

# Unit tests (works on macOS + Linux, no infra needed)
test-unit:
	$(GO) test -v -count=1 ./pkg/... ./cmd/...

# Unit tests with race detector
test-race:
	$(GO) test -v -race -count=1 ./pkg/... ./cmd/...

# Unit tests with coverage
test-cover:
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./pkg/... ./cmd/...
	$(GO) tool cover -func=coverage.out
	@echo "HTML report: go tool cover -html=coverage.out"

# Integration tests (Linux + KVM only)
test-integration:
	$(GO) test -v -tags=integration -count=1 -timeout=10m ./pkg/... ./cmd/...

# All tests
test-all:
	$(GO) test -v -tags=integration -count=1 -timeout=10m ./pkg/... ./cmd/...

# Pre-commit check (compile + unit tests)
check: build test-unit
	@echo "All checks passed"

# Help
help:
	@echo "Firecracker Bazel Runner Platform"
	@echo ""
	@echo "Build Targets:"
	@echo "  build                  - Build all binaries"
	@echo "  firecracker-manager    - Build firecracker-manager (native)"
	@echo "  firecracker-manager-linux - Build firecracker-manager (linux/amd64)"
	@echo "  test                   - Run all tests"
	@echo "  test-unit              - Unit tests (macOS + Linux)"
	@echo "  test-race              - Unit tests with race detector"
	@echo "  test-cover             - Unit tests with coverage report"
	@echo "  test-integration       - Integration tests (Linux + KVM)"
	@echo "  test-all               - All tests including integration"
	@echo "  check                  - Build + unit tests (pre-commit)"
	@echo "  lint                   - Run linter"
	@echo "  clean                  - Clean build artifacts"
	@echo ""
	@echo "Infrastructure:"
	@echo "  terraform-infra-init   - Initialize Terraform (infra stage)"
	@echo "  terraform-infra-plan   - Plan Terraform changes (infra stage)"
	@echo "  terraform-infra-apply  - Apply Terraform changes (infra stage)"
	@echo "  terraform-app-init     - Initialize Terraform (app stage)"
	@echo "  terraform-app-plan     - Plan Terraform changes (app stage)"
	@echo "  terraform-app-apply    - Apply Terraform changes (app stage)"
	@echo "  packer-build           - Build GCE host image"
	@echo "  release-host-image     - Build binary + Packer image"
	@echo "  mig-rolling-update     - Rolling update hosts to latest image"
	@echo ""
	@echo "Data Snapshot (RECOMMENDED - fast ~30s host boot):"
	@echo "  data-snapshot-build    - Build GCP disk snapshot with all artifacts"
	@echo "  data-snapshot-check    - Check if snapshot needs rebuild"
	@echo ""
	@echo "Legacy GCS Mode (slower ~5-15min host boot):"
	@echo "  git-cache-build        - Build git-cache.img to GCS"
	@echo "  git-cache-check        - Check git-cache freshness"
	@echo ""
	@echo "Variables:"
	@echo "  PROJECT_ID         - GCP project ID (required)"
	@echo "  REGION             - GCP region (default: us-central1)"
	@echo "  ZONE               - GCP zone (default: us-central1-a)"
	@echo "  ENV                - Environment name (default: dev)"
	@echo "  GIT_CACHE_REPOS    - Repos for git-cache (e.g., github.com/org/repo:name)"
	@echo ""
	@echo "Local Development (Linux with KVM):"
	@echo "  dev-provision        - Install prerequisites (run once, needs sudo)"
	@echo "  dev-build            - Build binaries + minimal rootfs"
	@echo "  dev-snapshot         - Build full snapshot for restore testing"
	@echo "  dev-test-snapshot-builder - Run snapshot-builder smoke tests (legacy + base-image)"
	@echo "  dev-stack            - Start control-plane + firecracker-manager"
	@echo "  dev-test-file-ops    - Run E2E file operations test (WS2)"
	@echo "  dev-test-pty         - Run E2E PTY terminal test (WS3)"
	@echo "  dev-test-template-tags - Run E2E template tags test (WS6)"
	@echo "  dev-test-network-policy - Run E2E network policy test"
	@echo "  dev-test-auth-proxy  - Run E2E auth proxy test (delegated provider)"
	@echo "  dev-test-checkpoint  - Run E2E checkpoint test (WS4, needs GCS)"
	@echo "  dev-test-auto-resume - Run E2E auto-resume test (WS5, needs GCS)"
	@echo "  dev-agent-snapshot   - Provision the AI agent snapshot"
	@echo "  dev-run-agent-e2e    - Convenience wrapper: provision snapshot + run agent tests"
	@echo "  dev-stop             - Stop the stack"
	@echo ""
	@echo "Example workflow (disk snapshots):"
	@echo "  1. make packer-build PROJECT_ID=my-project"
	@echo "  2. make snapshot-builder && ./bin/snapshot-builder --repo-url=... --gcs-bucket=..."
	@echo "  3. make data-snapshot-build PROJECT_ID=my-project GIT_CACHE_REPOS=github.com/org/repo:name"
	@echo "  4. terraform apply -var='use_data_snapshot=true' -var='data_snapshot_name=runner-data-YYYYMMDD-HHMMSS'"
	@echo "  5. make mig-rolling-update PROJECT_ID=my-project"

# === Local Development ===
# Requires a Linux host with KVM. Run on bare-metal or a GCE VM with nested virt.
# Workflow: make dev-provision → make dev-build → make dev-test-snapshot-builder → make dev-snapshot → make dev-stack → make dev-test-pause-resume
# --- Linux dev targets (run directly on a Linux host with KVM) ---

# Install prerequisites on a fresh Linux host (Ubuntu/Debian)
dev-provision:
	sudo bash dev/setup-linux.sh

# Build binaries + dev rootfs
dev-build:
	make build
	bash dev/build-dev-rootfs.sh

# Build a full snapshot
dev-snapshot:
	bash dev/build-snapshot.sh

# Start the full stack (control-plane + firecracker-manager)
dev-stack:
	bash dev/run-stack.sh

# Stop the stack
dev-stop:
	bash dev/stop-stack.sh

# Run snapshot-builder smoke tests
dev-test-snapshot-builder:
	bash dev/test-snapshot-builder.sh

# Benchmark: cold allocate → exec → release latency
# Usage: WORKLOAD_KEY=<key> make dev-bench-allocate
dev-bench-allocate: bench-allocate
	./bin/bench-allocate \
	  --cp http://localhost:8080 \
	  --mgr http://localhost:9080 \
	  --workload-key "$(WORKLOAD_KEY)" \
	  --iterations $(or $(ITERATIONS),50) \
	  --warmup $(or $(WARMUP),5)

# Benchmark: session pause + resume latency
# Usage: WORKLOAD_KEY=<key> make dev-bench-session
dev-bench-session: bench-session
	./bin/bench-session \
	  --cp http://localhost:8080 \
	  --mgr http://localhost:9080 \
	  --workload-key "$(WORKLOAD_KEY)" \
	  --iterations $(or $(ITERATIONS),50) \
	  --warmup $(or $(WARMUP),5)

# Run E2E pause/resume test
dev-test-pause-resume:
	bash dev/test-pause-resume.sh

# Run E2E multi-pause chunk dedup test
dev-test-multi-pause-dedup:
	bash dev/test-multi-pause-dedup.sh

# Run E2E GCS pause/resume test
dev-test-gcs-pause-resume:
	bash dev/test-gcs-pause-resume.sh

# Run E2E file operations test (WS2)
dev-test-file-ops:
	bash dev/test-file-ops.sh

# Run E2E PTY terminal test (WS3)
dev-test-pty:
	bash dev/test-pty.sh

# Run E2E non-destructive checkpoint test (WS4)
dev-test-checkpoint:
	bash dev/test-checkpoint.sh

# Run E2E auto-resume test (WS5)
dev-test-auto-resume:
	bash dev/test-auto-resume.sh

# Run E2E template tags test (WS6)
dev-test-template-tags:
	bash dev/test-template-tags.sh

dev-test-network-policy:
	bash dev/test-network-policy.sh

# Run E2E auth proxy test
dev-test-auth-proxy:
	bash dev/test-auth-proxy.sh

# AI Agent Sandbox E2E tests
dev-agent-rootfs:
	bash dev/build-agent-rootfs.sh

dev-agent-snapshot:
	bash dev/provision-agent-snapshot.sh

dev-test-agent-sessions:
	bash dev/test-agent-sessions.sh

dev-run-agent-e2e:
	bash dev/run-agent-e2e.sh

# ── Python SDK ──────────────────────────────────────────────────────────────
sdk-python-lint:
	cd sdk/python && python -m ruff check src/bf_sdk/ tests/

sdk-python-test:
	cd sdk/python && python -m pytest tests/ -v

sdk-python-typecheck:
	cd sdk/python && python -m pyright src/bf_sdk/

sdk-python-e2e:
	cd sdk/python && python -m pytest tests/e2e_live.py -v
