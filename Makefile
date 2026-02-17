.PHONY: all build test clean proto docker-build docker-push terraform-init terraform-plan terraform-apply
.PHONY: packer-init packer-validate packer-build firecracker-manager-linux release-host-image mig-rolling-update
.PHONY: onboard onboard-validate bin-onboard
.PHONY: firecracker-manager control-plane snapshot-builder thaw-agent
.PHONY: git-cache-builder git-cache-freshness data-snapshot-builder snapshot-converter
.PHONY: test-unit test-race test-cover test-integration test-all check

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
GOFLAGS := -ldflags "-X main.version=$(VERSION)"

# Binaries
BINARIES := firecracker-manager control-plane snapshot-builder thaw-agent git-cache-builder git-cache-freshness data-snapshot-builder snapshot-converter bin-onboard

all: build

# Build all binaries
build: $(BINARIES)

LINUX_BUILD = CGO_ENABLED=0 GOOS=linux GOARCH=amd64

firecracker-manager:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/firecracker-manager ./cmd/firecracker-manager

control-plane:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/control-plane ./cmd/control-plane

snapshot-builder:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/snapshot-builder ./cmd/snapshot-builder

thaw-agent:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/thaw-agent ./cmd/thaw-agent

git-cache-builder:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/git-cache-builder ./cmd/git-cache-builder

git-cache-freshness:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/git-cache-freshness ./cmd/git-cache-freshness

data-snapshot-builder:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/data-snapshot-builder ./cmd/data-snapshot-builder

snapshot-converter:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/snapshot-converter ./cmd/snapshot-converter

bin-onboard:
	$(LINUX_BUILD) $(GO) build $(GOFLAGS) -o bin/onboard ./cmd/onboard

onboard: bin-onboard
	./bin/onboard --config=$(CONFIG) $(if $(STEPS),--steps=$(STEPS))

onboard-validate: bin-onboard
	./bin/onboard --config=$(CONFIG) --dry-run

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
	docker build --platform linux/amd64 -t $(REGISTRY)/firecracker-control-plane:$(VERSION) -f deploy/docker/Dockerfile.control-plane .
	docker tag $(REGISTRY)/firecracker-control-plane:$(VERSION) $(REGISTRY)/firecracker-control-plane:latest

docker-build-snapshot-builder:
	docker build --platform linux/amd64 -t $(REGISTRY)/firecracker-snapshot-builder:$(VERSION) -f deploy/docker/Dockerfile.snapshot-builder .
	docker tag $(REGISTRY)/firecracker-snapshot-builder:$(VERSION) $(REGISTRY)/firecracker-snapshot-builder:latest

docker-push:
	docker push $(REGISTRY)/firecracker-control-plane:$(VERSION)
	docker push $(REGISTRY)/firecracker-control-plane:latest
	docker push $(REGISTRY)/firecracker-snapshot-builder:$(VERSION)
	docker push $(REGISTRY)/firecracker-snapshot-builder:latest

# Build microVM rootfs
rootfs:
	cd images/microvm && ./build-rootfs.sh

# Terraform
terraform-init:
	cd deploy/terraform && terraform init

terraform-plan:
	cd deploy/terraform && terraform plan -var="project_id=$(PROJECT_ID)" -var="db_password=$(DB_PASSWORD)"

terraform-apply:
	cd deploy/terraform && terraform apply -var="project_id=$(PROJECT_ID)" -var="db_password=$(DB_PASSWORD)"

terraform-destroy:
	cd deploy/terraform && terraform destroy -var="project_id=$(PROJECT_ID)" -var="db_password=$(DB_PASSWORD)"

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

data-snapshot-build: data-snapshot-builder
	@echo "Building data snapshot (run on a GCE VM)..."
	@echo "This creates a disk, populates it with snapshot+git-cache, and creates a GCP disk snapshot."
	./bin/data-snapshot-builder \
		--project=$(PROJECT_ID) \
		--zone=$(ZONE) \
		--snapshot-gcs=gs://$(PROJECT_ID)-firecracker-snapshots/current/ \
		--repos="$(GIT_CACHE_REPOS)" \
		--metadata-bucket=$(PROJECT_ID)-firecracker-snapshots

data-snapshot-check: git-cache-freshness
	@echo "Checking data snapshot freshness..."
	./bin/git-cache-freshness \
		--gcs-bucket=$(PROJECT_ID)-firecracker-snapshots \
		--gcp-project=$(PROJECT_ID) \
		--max-age-hours=24 \
		--max-commit-drift=50

# Legacy git-cache targets (GCS-based approach - slower host boot)
.PHONY: git-cache-build git-cache-check

git-cache-build: git-cache-builder
	@echo "Building git-cache image to GCS (legacy mode)..."
	./bin/git-cache-builder \
		--repos="$(GIT_CACHE_REPOS)" \
		--gcs-bucket=$(PROJECT_ID)-firecracker-snapshots \
		--output-dir=/tmp/git-cache-build

git-cache-check: git-cache-freshness
	@echo "Checking git-cache freshness..."
	./bin/git-cache-freshness \
		--gcs-bucket=$(PROJECT_ID)-firecracker-snapshots \
		--max-age-hours=24 \
		--max-commit-drift=50

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
	@echo "  terraform-init         - Initialize Terraform"
	@echo "  terraform-plan         - Plan Terraform changes"
	@echo "  terraform-apply        - Apply Terraform changes"
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
	@echo "Example workflow (disk snapshots):"
	@echo "  1. make packer-build PROJECT_ID=my-project"
	@echo "  2. make snapshot-builder && ./bin/snapshot-builder --repo-url=... --gcs-bucket=..."
	@echo "  3. make data-snapshot-build PROJECT_ID=my-project GIT_CACHE_REPOS=github.com/org/repo:name"
	@echo "  4. terraform apply -var='use_data_snapshot=true' -var='data_snapshot_name=runner-data-YYYYMMDD-HHMMSS'"
	@echo "  5. make mig-rolling-update PROJECT_ID=my-project"


