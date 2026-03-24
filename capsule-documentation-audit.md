# Capsule Repository - Documentation Audit Report

**Repository:** `rahul-roy-glean/capsule`
**Date:** 2026-03-24

---

## 1. CRITICAL: Documentation vs. Code Inconsistencies

### 1.1 Missing `onboard.yaml` in Root Directory
- **Files:** `README.md:50-53`, `docs/setup.md:40-43`
- **Problem:** Both instruct users to run `cp onboard.yaml my-config.yaml`, but no `onboard.yaml` exists in the repo root. The only copies exist under `examples/*/onboard.yaml`.
- **Impact:** Users following the quickstart will hit a "file not found" error immediately.

### 1.2 Missing `images/microvm/` Directory
- **Files:** `docs/DEV_SETUP.md:233`, `Makefile:167`
- **Problem:** DEV_SETUP.md lists `images/microvm/` as "guest rootfs build assets" and the Makefile has a `rootfs` target that runs `cd images/microvm && ./build-rootfs.sh`. This directory does not exist.
- **Reality:** Rootfs building is done via `dev/build-dev-rootfs.sh` and `dev/build-agent-rootfs.sh`.
- **Impact:** `make rootfs` will fail. Documentation misleads developers about project structure.

### 1.3 Undocumented API Endpoints
Several control plane endpoints registered in `cmd/capsule-control-plane/main.go` are not mentioned in user-facing docs:

| Endpoint | Line | Documented? |
|---|---|---|
| `/api/v1/runners/quarantine` | main.go:369 | No |
| `/api/v1/runners/unquarantine` | main.go:370 | No |
| `/api/v1/canary/report` | main.go:381 | No |

---

## 2. HIGH: Undocumented CLI Flags, Env Vars & Configuration

### 2.1 Autoscaler/Downscaler - 9 Env Vars, Zero Documentation
**File:** `cmd/capsule-control-plane/downscaler.go:32-101`

Completely undocumented environment variables:
- `DOWNSCALER_ENABLED`, `DOWNSCALER_INTERVAL`, `DOWNSCALER_MAX_DELETES`, `DOWNSCALER_MAX_DRAINS`, `DOWNSCALER_HEARTBEAT_STALE`
- `AUTOSCALER_SCALE_UP_THRESHOLD`, `AUTOSCALER_SCALE_DOWN_THRESHOLD`, `AUTOSCALER_COOLDOWN`, `AUTOSCALER_BOOT_COOLDOWN`

### 2.2 Builder VM Configuration - 4 Env Vars
**File:** `cmd/capsule-control-plane/main.go:287-296`
- `BUILDER_NETWORK`, `BUILDER_SUBNET`, `BUILDER_IMAGE`, `BUILDER_SERVICE_ACCOUNT`

### 2.3 MCP Server Feature - 2 Flags
**File:** `cmd/capsule-control-plane/main.go:57-58`
- `--mcp-port`, `--mcp-auth-token` - Entire MCP server feature is undocumented.

### 2.4 Snapshot Builder - 27+ CLI Flags
**File:** `cmd/snapshot-builder/main.go:33-72`
- Includes required flags like `--snapshot-commands`, `--gcs-bucket` - none documented.

### 2.5 Capsule Manager - 22+ CLI Flags
**File:** `cmd/capsule-manager/main.go:43-68`
- The critical `--control-plane` address flag is required but undocumented.
- `--quarantine-dir` flag references an undocumented quarantine feature.

### 2.6 OpenTelemetry - 6 Env Vars
**File:** `pkg/otel/config.go:18-44`
- `INSTANCE_ID`, `GCE_INSTANCE_ID`, `ZONE`, `GCE_ZONE`, `OTEL_EXPORTER_OTLP_ENDPOINT`

### 2.7 Advanced Onboard Config Fields
Undocumented fields accepted in `onboard.yaml`:
- `chunk_cache_size_gb`, `mem_cache_size_gb`, `auto_pause`, `ttl`, `tier`, `auto_rollout`, `session_max_age_seconds`, `rootfs_size_gb`, `network_policy_preset`

### 2.8 Helm Chart Values
**File:** `deploy/helm/capsule/values.yaml`
- No user-facing documentation of Helm values.

### 2.9 Benchmark Tools
**Binaries:** `bench-allocate`, `bench-session` - 11+ CLI flags, completely undocumented.

---

## 3. HIGH: Missing Go Doc Comments on Exported Symbols

### 3.1 Packages Without Package-Level Documentation

| Package | Status |
|---|---|
| `pkg/firecracker/` | Missing `// Package` comment |
| `pkg/github/` | Missing |
| `pkg/telemetry/` | Missing |
| `pkg/tiers/` | Missing |

### 3.2 Undocumented Exported Types

| File | Line | Symbol |
|---|---|---|
| `pkg/runner/manager.go` | 110 | `QuarantineOptions` struct |
| `pkg/runner/manager.go` | 116 | `UnquarantineOptions` struct |
| `pkg/otel/instruments.go` | 10-13 | `HistogramName`, `GaugeName`, `UpDownCounterName`, `Float64GaugeName` |

### 3.3 Undocumented Exported Functions/Methods

**Auth proxy providers** - all 5 providers (`bearer_token.go`, `delegated.go`, `gcp_metadata.go`, `github_app.go`, `github_dual_token.go`) have undocumented `Name()`, `Matches()`, `InjectCredentials()`, `Start()`, `Stop()` methods.

**Network stubs** - `NewDNSProxy`, `NewNetNSNetwork`, `EnsureNetNSDir`, `NewPolicyEnforcer` + ~24 stub methods across `dns_proxy_stub.go`, `netns_stub.go`, `policy_enforcer_stub.go`.

**Runner manager** - `IsDraining()` (line 415), `SetDraining()` (line 421), `QuarantineRunner()` (line 576), `UnquarantineRunner()` (line 682), `GetChunkStore()` (line 1371).

---

## 4. HIGH: Missing Architecture & Design Documentation

The existing `docs/architecture.md` provides a high-level overview, but the following complex subsystems have no design documentation:

| Component | Key File(s) | LOC | Risk |
|---|---|---|---|
| **UFFD Handler** | `pkg/uffd/handler.go`, `layered_handler.go` | 1,370 | CRITICAL |
| **Session Pause/Resume** | `pkg/runner/session.go` | 1,475 | HIGH |
| **Chunked Snapshot System** | `pkg/snapshot/chunked.go` | 1,407 | HIGH |
| **Network Namespaces** | `pkg/network/netns.go` | 2,700+ | HIGH |
| **Network Policy Enforcement** | `pkg/network/policy*.go` | 28,700+ | HIGH |
| **Auth Proxy & SSL Bumping** | `pkg/authproxy/proxy.go` | 19,570 | HIGH |
| **Runner Manager Lifecycle** | `pkg/runner/manager.go` | 1,417 | HIGH |
| **Layer Builder Pipeline** | `cmd/.../layer_builder.go` | 1,607 | HIGH |
| **Scheduler/Allocation** | `cmd/.../scheduler.go` | 1,144 | MEDIUM-HIGH |
| **Snapshot Manager** | `cmd/.../snapshots.go` | 1,048 | HIGH |
| **Chunked Restore** | `pkg/runner/chunked.go` | 1,525 | HIGH |
| **Session Uploader** | `pkg/snapshot/session_uploader.go` | 693 | MEDIUM-HIGH |
| **Downscaler/Autoscaler** | `cmd/.../downscaler.go` | 550 | MEDIUM |
| **MCP Integration** | `cmd/.../mcp.go` | 816 | MEDIUM |
| **Deployment Architecture** | `deploy/` | - | HIGH |

### Missing design docs needed:
- `docs/design-uffd-lazy-load.md` - Userfaultfd integration, address translation
- `docs/design-session-lifecycle.md` - Pause/resume state machine, cross-host resume
- `docs/design-snapshot-chunking.md` - Chunk addressing, prefetching, caching
- `docs/design-network-isolation.md` - Namespace design, veth routing, security model
- `docs/design-auth-proxy.md` - Credential injection, SSL bumping, providers
- `docs/design-build-pipeline.md` - Layer builder, dependency graph, convergence
- `docs/design-scheduler.md` - Allocation algorithm, cache affinity
- `docs/deployment-architecture.md` - Infrastructure topology, Helm/Terraform guidance

---

## 5. Verified as Correct

The following were confirmed consistent between docs and code:
- All documented API endpoints (15+) match code registrations
- Control plane default ports (gRPC `:50051`, HTTP `:8080`)
- Database table names (5 tables)
- Python SDK methods (`.with_base_image()`, `.with_ttl()`, etc.)
- Build query parameters (`?force=true`, `?clean=true`)
- Go version requirement (`>= 1.24`)
- Make targets (`check`, `test-unit`, `lint`, `build`, `proto`)
- GCS path structures (`v1/build-artifacts/`, `v1/chunks/`)

---

## Summary

| Category | Count |
|---|---|
| Critical doc/code inconsistencies | 3 |
| Undocumented env vars | 19+ |
| Undocumented CLI flags | 60+ |
| Undocumented config fields | 9+ |
| Missing package-level Go docs | 4 packages |
| Undocumented exported symbols | 70+ |
| Missing design/architecture docs | 15 subsystems |

### Top 5 Priorities to Fix
1. Fix the broken quickstart (`onboard.yaml` missing from root)
2. Fix the `images/microvm` reference (dead Makefile target + wrong DEV_SETUP.md)
3. Document autoscaler/downscaler env vars (production-critical)
4. Document capsule-manager and snapshot-builder CLI flags (required for deployment)
5. Write design docs for UFFD handler, session lifecycle, and chunked snapshots (critical for maintainability)
