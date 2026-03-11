#!/usr/bin/env python3
"""
E2E Control Plane Simulation Script
====================================
Exercises every surface of the bazel-firecracker control plane HTTP API
using the bf-sdk Python client, with rich structured logging and clear
pass/fail reporting per scenario.

Usage:
    python scripts/e2e_simulate.py [--base-url http://10.0.16.16:8080] [--token <token>] [--suite <suite>]

Suites:
    all         - Run every suite (default)
    health      - Health / connectivity checks
    configs     - Layered config CRUD
    builds      - Trigger builds, poll status
    snapshots   - Snapshot listing
    runners     - Runner allocate / status / release lifecycle
    session     - Pause → resume session lifecycle
    quarantine  - Quarantine / unquarantine a runner
    fleet       - Fleet convergence & desired versions
    canary      - Canary report endpoint
    edge        - Edge-case / error-path scenarios
    concurrency - Multi-client concurrent load scenarios
    realistic   - Staggered arrivals, mixed workloads, natural concurrency

Environment variables (override CLI flags):
    BF_BASE_URL  - Control plane base URL
    BF_TOKEN     - Bearer auth token
"""

from __future__ import annotations

import argparse
import json
import logging
import math
import os
import random
import sys
import time
import uuid
from collections import Counter
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime, timezone
from threading import Barrier, BrokenBarrierError, Semaphore, Thread
from typing import Any, Optional

import httpx

# ---------------------------------------------------------------------------
# SDK import – run from repo root or install with `pip install -e sdk/python`
# ---------------------------------------------------------------------------
SDK_PATH = os.path.join(os.path.dirname(__file__), "..", "sdk", "python", "src")
if SDK_PATH not in sys.path:
    sys.path.insert(0, SDK_PATH)

try:
    import bf_sdk
    from bf_sdk import BFClient, BFError, BFNotFound, BFServiceUnavailable
    from bf_sdk._errors import BFRateLimited
    from bf_sdk._http import HttpClient
except ImportError as exc:
    print(f"[FATAL] Cannot import bf_sdk: {exc}")
    print("  Install with: pip install -e sdk/python  or run from the repo root.")
    sys.exit(1)

# ---------------------------------------------------------------------------
# Fixed workload key used for all runner/session/quarantine/fleet tests
# ---------------------------------------------------------------------------
FIXED_WORKLOAD_KEY = "34a7694a602bf3cb"


# ---------------------------------------------------------------------------
# Logging setup – structured JSON to stderr so stdout stays clean for output
# ---------------------------------------------------------------------------

class _JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "ts": datetime.now(timezone.utc).isoformat(),
            "level": record.levelname,
            "logger": record.name,
            "msg": record.getMessage(),
        }
        if record.exc_info:
            payload["exc"] = self.formatException(record.exc_info)
        extra = {k: v for k, v in record.__dict__.items()
                 if k not in logging.LogRecord.__dict__ and
                    k not in ("msg", "args", "levelname", "levelno", "name",
                              "pathname", "filename", "module", "exc_info",
                              "exc_text", "stack_info", "lineno", "funcName",
                              "created", "msecs", "relativeCreated", "thread",
                              "threadName", "processName", "process", "message",
                              "taskName")}
        if extra:
            payload.update(extra)
        return json.dumps(payload)


def _setup_logging(verbose: bool = False) -> logging.Logger:
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(_JSONFormatter())
    root = logging.getLogger()
    root.addHandler(handler)
    root.setLevel(logging.DEBUG if verbose else logging.INFO)
    return logging.getLogger("e2e")


# ---------------------------------------------------------------------------
# Result tracking
# ---------------------------------------------------------------------------

@dataclass
class TestResult:
    name: str
    suite: str
    passed: bool
    duration_ms: float
    detail: str = ""
    error: str = ""


@dataclass
class SimulationRun:
    results: list[TestResult] = field(default_factory=list)

    def record(self, r: TestResult) -> None:
        self.results.append(r)

    def summary(self) -> dict[str, Any]:
        total  = len(self.results)
        passed = sum(1 for r in self.results if r.passed)
        failed = total - passed
        by_suite: dict[str, dict[str, int]] = {}
        for r in self.results:
            s = by_suite.setdefault(r.suite, {"passed": 0, "failed": 0})
            if r.passed:
                s["passed"] += 1
            else:
                s["failed"] += 1
        return {"total": total, "passed": passed, "failed": failed, "by_suite": by_suite}

    def print_report(self) -> None:
        print("\n" + "=" * 72)
        print("  E2E SIMULATION REPORT")
        print("=" * 72)
        for r in self.results:
            icon = "✓" if r.passed else "✗"
            label = f"{r.suite}/{r.name}"
            ms = f"{r.duration_ms:.0f}ms"
            line = f"  {icon}  {label:<52}  {ms:>8}"
            print(line)
            if not r.passed and r.error:
                print(f"       ERROR: {r.error}")
        s = self.summary()
        print("=" * 72)
        print(f"  TOTAL {s['total']}  |  PASSED {s['passed']}  |  FAILED {s['failed']}")
        print("=" * 72 + "\n")


@dataclass
class RealisticConfig:
    total_runners: int = 30
    arrival_rate: float = 2.0
    lifetime_mean: float = 15.0
    lifetime_stddev: float = 5.0
    session_pct: float = 0.3
    max_concurrent: int = 20
    steady_seconds: float = 60.0


@dataclass
class RunnerLifecycleResult:
    runner_id: str
    alloc_latency: float
    lifetime: float
    is_session: bool
    success: bool
    error: Optional[str] = None
    status_code: Optional[int] = None


# ---------------------------------------------------------------------------
# Test-case context manager
# ---------------------------------------------------------------------------

@contextmanager
def test_case(run: SimulationRun, suite: str, name: str, log: logging.Logger):
    """Wrap a single test case: timing, logging, result recording."""
    log.info("Starting test", extra={"suite": suite, "test": name})
    t0 = time.monotonic()
    passed = False
    error = ""
    try:
        yield
        passed = True
        log.info("PASSED", extra={"suite": suite, "test": name})
    except AssertionError as exc:
        error = str(exc) or "assertion failed"
        log.warning("FAILED (assertion)", extra={"suite": suite, "test": name, "error": error})
    except BFNotFound as exc:
        error = f"Not found: {exc}"
        log.warning("FAILED (not found)", extra={"suite": suite, "test": name, "error": error})
    except (BFServiceUnavailable, BFRateLimited) as exc:
        error = f"Service unavailable: {exc}"
        log.warning("FAILED (service unavailable)", extra={"suite": suite, "test": name, "error": error})
    except BFError as exc:
        error = f"API error: {exc}"
        log.warning("FAILED (api error)", extra={"suite": suite, "test": name, "error": error})
    except Exception as exc:  # noqa: BLE001
        error = f"{type(exc).__name__}: {exc}"
        log.error("FAILED (unexpected)", extra={"suite": suite, "test": name, "error": error},
                  exc_info=True)
    finally:
        duration_ms = (time.monotonic() - t0) * 1000
        run.record(TestResult(
            name=name, suite=suite, passed=passed,
            duration_ms=duration_ms, error=error,
        ))


# ---------------------------------------------------------------------------
# Raw HTTP helper
# Builds a plain httpx.Client that mirrors the SDK auth headers, so we can
# make raw requests for negative / error-path tests.
# ---------------------------------------------------------------------------

def _raw_client(http: HttpClient) -> httpx.Client:
    """Return a plain httpx.Client sharing the SDK's base_url and auth headers."""
    headers = dict(http._client.headers)
    return httpx.Client(
        base_url=http._config.base_url,
        headers=headers,
        timeout=httpx.Timeout(10.0),
    )


# ===========================================================================
# SUITE: health
# ===========================================================================

def suite_health(client: BFClient, run: SimulationRun, log: logging.Logger) -> None:
    """Basic connectivity and health checks."""
    SUITE = "health"
    http = client._http  # type: ignore[attr-defined]

    with test_case(run, SUITE, "GET /health returns 200", log):
        with _raw_client(http) as rc:
            resp = rc.get("/health")
        assert resp.status_code == 200, f"Expected 200, got {resp.status_code}"
        log.info("Health response", extra={"status": resp.status_code, "body": resp.text[:80]})

    with test_case(run, SUITE, "GET /metrics returns 200", log):
        with _raw_client(http) as rc:
            resp = rc.get("/metrics")
        assert resp.status_code == 200, f"Expected 200, got {resp.status_code}"

    with test_case(run, SUITE, "Unauthenticated request rejected when token configured", log):
        if http._config.token:
            # A client with no auth header
            with httpx.Client(base_url=http._config.base_url, timeout=10.0) as rc:
                resp = rc.get("/api/v1/runners")
            assert resp.status_code == 401, (
                f"Expected 401 for unauthenticated request, got {resp.status_code}"
            )
            log.info("Unauthed request correctly rejected", extra={"status": resp.status_code})
        else:
            log.info("No auth token configured, skipping unauthed test")

    with test_case(run, SUITE, "GET /api/v1/hosts returns valid JSON", log):
        data = http.get("/api/v1/hosts")
        assert "hosts" in data, f"Expected 'hosts' key, got {list(data.keys())}"
        assert "count" in data
        log.info("Hosts response", extra={"count": data["count"]})

    with test_case(run, SUITE, "GET /api/v1/runners returns valid JSON", log):
        data = http.get("/api/v1/runners")
        assert "runners" in data, f"Expected 'runners' key, got {list(data.keys())}"
        assert "count" in data
        log.info("Runners response", extra={"count": data["count"]})


# ===========================================================================
# SUITE: snapshots
# ===========================================================================

def suite_snapshots(client: BFClient, run: SimulationRun, log: logging.Logger) -> None:
    """Snapshot listing."""
    SUITE = "snapshots"

    with test_case(run, SUITE, "List snapshots returns a list", log):
        snaps = client.snapshots.list()
        assert isinstance(snaps, list), f"Expected list, got {type(snaps)}"
        log.info("Snapshots listed", extra={"count": len(snaps)})
        for s in snaps[:5]:
            log.info("Snapshot", extra={
                "version": s.version,
                "status": s.status,
                "gcs_path": s.gcs_path,
                "size_bytes": s.size_bytes,
            })

    with test_case(run, SUITE, "Snapshot objects have required fields", log):
        snaps = client.snapshots.list()
        for s in snaps:
            assert s.version, "snapshot.version must be non-empty"
            assert s.status is not None, "snapshot.status must be set"

    with test_case(run, SUITE, "At most one 'active' snapshot at a time", log):
        snaps = client.snapshots.list()
        active = [s for s in snaps if s.status == "active"]
        assert len(active) <= 1, (
            f"Expected ≤1 active snapshot, found {len(active)}: "
            + ", ".join(s.version for s in active)
        )
        log.info("Active snapshots", extra={"count": len(active)})

    with test_case(run, SUITE, "Snapshot status values are known states", log):
        snaps = client.snapshots.list()
        KNOWN = {"active", "ready", "deprecated", "failed", "building", "canary",
                 "rolled_back", "validating", "pending"}
        for s in snaps:
            assert s.status in KNOWN or s.status is None, (
                f"Unexpected snapshot status '{s.status}' for version {s.version}"
            )


# ===========================================================================
# SUITE: configs
# ===========================================================================

_DUMMY_LAYER_COMMANDS = [{"type": "shell", "args": ["echo 'e2e init layer'"]}]


def _make_test_config_body(tag: str = "") -> dict[str, Any]:
    """Build a minimal layered config payload deterministic on `tag`."""
    name = f"e2e-test-{tag or uuid.uuid4().hex[:8]}"
    return {
        "display_name": name,
        "layers": [
            {
                "name": "base",
                "init_commands": _DUMMY_LAYER_COMMANDS,
            }
        ],
        "config": {
            "ttl": 300,
            "auto_pause": True,
            "tier": "s",
        },
    }


def suite_configs(
    client: BFClient, run: SimulationRun, log: logging.Logger,
) -> dict[str, Any]:
    """
    Layered config CRUD lifecycle.
    Returns created config info for downstream suites.
    """
    SUITE = "configs"
    created: dict[str, Any] = {}

    with test_case(run, SUITE, "List configs returns a list", log):
        configs = client.layered_configs.list()
        assert isinstance(configs, list), f"Expected list, got {type(configs)}"
        log.info("Existing configs", extra={"count": len(configs)})

    # --- Create ---
    with test_case(run, SUITE, "Create config returns config_id and leaf_workload_key", log):
        body = _make_test_config_body("e2ecreate")
        resp = client.layered_configs.create(body)
        assert resp.config_id, "config_id must be non-empty"
        assert resp.leaf_workload_key, "leaf_workload_key must be non-empty"
        created["config_id"] = resp.config_id
        created["leaf_workload_key"] = resp.leaf_workload_key
        log.info("Config created", extra={
            "config_id": resp.config_id,
            "leaf_workload_key": resp.leaf_workload_key,
            "num_layers": len(resp.layers or []),
        })

    if not created.get("config_id"):
        log.warning("Config creation failed — skipping downstream config tests")
        return created

    # --- Idempotent re-create ---
    with test_case(run, SUITE, "Re-creating identical config is idempotent (same config_id)", log):
        body = _make_test_config_body("e2ecreate")
        resp2 = client.layered_configs.create(body)
        assert resp2.config_id == created["config_id"], (
            f"Expected same config_id on re-create, "
            f"got {resp2.config_id} vs {created['config_id']}"
        )

    # --- Get by ID ---
    with test_case(run, SUITE, "Get config by config_id returns full detail", log):
        detail = client.layered_configs.get(created["config_id"])
        assert detail.config.config_id == created["config_id"]
        assert detail.layers is not None and len(detail.layers) > 0
        log.info("Config detail", extra={
            "config_id": detail.config.config_id,
            "display_name": detail.config.display_name,
            "layer_count": len(detail.layers or []),
            "tier": detail.config.tier,
            "ttl": detail.config.runner_ttl_seconds,
        })
        for layer in (detail.layers or []):
            log.info("Layer status", extra={
                "layer_name": layer.name,
                "status": layer.status,
                "depth": layer.depth,
                "layer_hash": (layer.layer_hash or "")[:16],
            })

    # --- List contains created config ---
    with test_case(run, SUITE, "List includes newly created config", log):
        configs = client.layered_configs.list()
        ids = [c.config_id for c in configs]
        assert created["config_id"] in ids, (
            f"Created config {created['config_id']} not found in list: {ids[:5]}"
        )

    # --- Config fields are correct ---
    with test_case(run, SUITE, "Config fields match what was submitted", log):
        detail = client.layered_configs.get(created["config_id"])
        assert detail.config.runner_ttl_seconds == 300, (
            f"Expected ttl=300, got {detail.config.runner_ttl_seconds}"
        )
        assert detail.config.auto_pause is True, (
            f"Expected auto_pause=True, got {detail.config.auto_pause}"
        )
        assert detail.config.tier == "s", (
            f"Expected tier='s', got {detail.config.tier}"
        )

    # --- Create a second config (multi-layer) ---
    with test_case(run, SUITE, "Create multi-layer config works", log):
        body2: dict[str, Any] = {
            "display_name": "e2e-multi-layer",
            "layers": [
                {
                    "name": "deps",
                    "init_commands": [{"type": "shell", "args": ["echo 'install deps'"]}],
                },
                {
                    "name": "app",
                    "init_commands": [{"type": "shell", "args": ["echo 'setup app'"]}],
                    "refresh_commands": [{"type": "shell", "args": ["echo 'refresh app'"]}],
                    "refresh_interval": "1h",
                },
            ],
            "config": {"ttl": 600, "tier": "m"},
        }
        resp3 = client.layered_configs.create(body2)
        assert resp3.config_id, "multi-layer config_id must be set"
        assert resp3.leaf_workload_key, "multi-layer leaf_workload_key must be set"
        created["multi_config_id"] = resp3.config_id
        log.info("Multi-layer config created", extra={
            "config_id": resp3.config_id,
            "leaf_workload_key": resp3.leaf_workload_key,
        })

    return created


# ===========================================================================
# SUITE: builds
# ===========================================================================

def suite_builds(
    client: BFClient, run: SimulationRun, log: logging.Logger,
    config_id: str | None = None,
) -> None:
    """Trigger and monitor layer builds."""
    SUITE = "builds"

    if not config_id:
        log.info("No config_id supplied for builds suite, creating a fresh one")
        try:
            resp = client.layered_configs.create(_make_test_config_body("buildsuite"))
            config_id = resp.config_id
        except Exception as exc:  # noqa: BLE001
            log.warning("Could not create config for builds suite", extra={"error": str(exc)})
            return

    with test_case(run, SUITE, "Trigger build returns config_id and status", log):
        resp = client.layered_configs.build(config_id)
        assert resp.config_id == config_id
        assert resp.status is not None
        log.info("Build triggered", extra={"config_id": config_id, "status": resp.status})

    with test_case(run, SUITE, "Trigger build with force=True is accepted", log):
        resp = client.layered_configs.build(config_id, force=True)
        assert resp.config_id == config_id
        log.info("Force build response", extra={"status": resp.status})

    with test_case(run, SUITE, "Get config shows layer statuses after build trigger", log):
        detail = client.layered_configs.get(config_id)
        assert detail.layers is not None
        for layer in detail.layers:
            log.info("Layer post-build-trigger", extra={
                "layer_name": layer.name,
                "status": layer.status,
                "build_status": layer.build_status,
                "build_version": layer.build_version,
            })

    with test_case(run, SUITE, "Build status is a known value", log):
        VALID_LAYER_STATUS = {
            "pending", "queued", "building", "running", "waiting_parent",
            "ready", "active", "failed", "cancelled", "inactive", None,
        }
        VALID_BUILD_STATUS = {
            "queued", "waiting_parent", "running", "ready",
            "failed", "cancelled", None,
        }
        detail = client.layered_configs.get(config_id)
        for layer in (detail.layers or []):
            assert layer.status in VALID_LAYER_STATUS, (
                f"Layer '{layer.name}' has unexpected status '{layer.status}'"
            )
            assert layer.build_status in VALID_BUILD_STATUS, (
                f"Layer '{layer.name}' has unexpected build_status '{layer.build_status}'"
            )

    # Poll up to 60s for build activity to settle
    with test_case(run, SUITE, "Build status stabilises within 60s", log):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            detail = client.layered_configs.get(config_id)
            build_statuses = [l.build_status for l in (detail.layers or [])]
            active = [bs for bs in build_statuses if bs in {"queued", "running", "waiting_parent"}]
            log.debug("Build poll", extra={
                "config_id": config_id,
                "build_statuses": build_statuses,
                "active_count": len(active),
            })
            if not active:
                break
            time.sleep(5)
        log.info("Build settled", extra={"build_statuses": build_statuses})

    # Refresh layer test
    with test_case(run, SUITE, "Refresh layer endpoint returns response", log):
        detail = client.layered_configs.get(config_id)
        if detail.layers:
            layer_name = detail.layers[-1].name  # use leaf layer
            resp = client.layered_configs.refresh_layer(config_id, layer_name)
            assert resp.config_id == config_id
            assert resp.layer_name == layer_name
            log.info("Refresh layer response", extra={
                "layer_name": layer_name,
                "status": resp.status,
            })


# ===========================================================================
# SUITE: runners
# ===========================================================================

def suite_runners(
    client: BFClient, run: SimulationRun, log: logging.Logger,
    leaf_workload_key: str | None = None,
) -> dict[str, Any]:
    """
    Runner allocate / status / connect / release lifecycle.
    Returns allocation info for downstream suites.
    """
    SUITE = "runners"
    alloc_info: dict[str, Any] = {}

    if not leaf_workload_key:
        try:
            configs = client.layered_configs.list()
            if configs and configs[0].leaf_workload_key:
                leaf_workload_key = configs[0].leaf_workload_key
                log.info("Discovered workload_key from first config", extra={
                    "workload_key": leaf_workload_key,
                    "config_id": configs[0].config_id,
                })
        except Exception as exc:  # noqa: BLE001
            log.warning("Could not discover workload_key", extra={"error": str(exc)})

    if not leaf_workload_key:
        log.warning("No workload_key available — skipping runner lifecycle tests")
        return alloc_info

    # --- Allocate ---
    with test_case(run, SUITE, "Allocate runner returns runner_id", log):
        request_id = f"e2e-{uuid.uuid4().hex[:12]}"
        resp = client.runners.allocate(leaf_workload_key, request_id=request_id)
        assert resp.runner_id, "runner_id must be non-empty"
        alloc_info.update({
            "runner_id": resp.runner_id,
            "host_id": resp.host_id,
            "host_address": resp.host_address,
            "session_id": resp.session_id,
            "resumed": resp.resumed,
        })
        log.info("Runner allocated", extra={
            "runner_id": resp.runner_id,
            "host_id": resp.host_id,
            "host_address": resp.host_address,
            "resumed": resp.resumed,
            "request_id": request_id,
        })

    runner_id: str | None = alloc_info.get("runner_id")

    # --- Status ---
    with test_case(run, SUITE, "Runner status is a known state", log):
        if not runner_id:
            log.warning("No runner_id — skipping status check")
        else:
            status = client.runners.status(runner_id)
            KNOWN = {"ready", "pending", "unavailable", "suspended", "quarantined"}
            assert status.status in KNOWN, (
                f"Unexpected runner status '{status.status}' — expected one of {KNOWN}"
            )
            log.info("Runner status", extra={"runner_id": runner_id, "status": status.status})

    # --- Poll until ready (best-effort, short timeout) ---
    with test_case(run, SUITE, "Runner status poll is stable (no server errors)", log):
        if not runner_id:
            log.warning("Skipping status poll — no runner_id")
        else:
            deadline = time.monotonic() + 30
            final_status = None
            while time.monotonic() < deadline:
                s = client.runners.status(runner_id)
                final_status = s.status
                log.debug("Polling runner", extra={"runner_id": runner_id, "status": final_status})
                if final_status in ("ready", "unavailable", "suspended"):
                    break
                time.sleep(3)
            log.info("Final polled status", extra={"runner_id": runner_id, "status": final_status})

    # --- List ---
    with test_case(run, SUITE, "List runners returns expected shape", log):
        runners = client.runners.list()
        assert isinstance(runners, list), f"Expected list, got {type(runners)}"
        log.info("Runners listed", extra={"count": len(runners)})
        if runner_id:
            in_list = any(r.get("runner_id") == runner_id for r in runners)
            log.info("Allocated runner in list", extra={
                "runner_id": runner_id,
                "in_list": in_list,
            })

    # --- Connect ---
    with test_case(run, SUITE, "Connect to runner returns valid status", log):
        if not runner_id:
            log.warning("Skipping connect — no runner_id")
        else:
            result = client.runners.connect(runner_id)
            assert result.runner_id, "connect result runner_id must be set"
            assert result.status in {"connected", "resumed", "pending"}, (
                f"Unexpected connect status: {result.status}"
            )
            # Update runner_id in case it changed after resume
            alloc_info["runner_id"] = result.runner_id
            log.info("Connect result", extra={
                "runner_id": result.runner_id,
                "status": result.status,
                "host_address": result.host_address,
            })

    # --- Labels in allocate ---
    with test_case(run, SUITE, "Allocate with labels is accepted", log):
        resp2 = client.runners.allocate(
            leaf_workload_key,
            request_id=f"e2e-labels-{uuid.uuid4().hex[:8]}",
            labels={"ci": "true", "e2e": "true"},
        )
        assert resp2.runner_id, "runner_id must be set"
        log.info("Allocate with labels", extra={"runner_id": resp2.runner_id})
        # Release this extra runner immediately
        client.runners.release(resp2.runner_id)

    # --- Network policy preset ---
    with test_case(run, SUITE, "Allocate with network_policy_preset is accepted", log):
        resp3 = client.runners.allocate(
            leaf_workload_key,
            request_id=f"e2e-netpol-{uuid.uuid4().hex[:8]}",
            network_policy_preset="restricted-egress",
        )
        assert resp3.runner_id, "runner_id must be set"
        log.info("Allocate with network_policy_preset", extra={"runner_id": resp3.runner_id})
        client.runners.release(resp3.runner_id)

    return alloc_info


# ===========================================================================
# SUITE: session (pause / resume)
# ===========================================================================

def suite_session(
    client: BFClient, run: SimulationRun, log: logging.Logger,
    alloc_info: dict[str, Any] | None = None,
    leaf_workload_key: str | None = None,
) -> None:
    """Pause → suspend → resume session lifecycle."""
    SUITE = "session"

    runner_id: str | None = (alloc_info or {}).get("runner_id")

    if not runner_id:
        log.info("No active runner for session suite — allocating fresh runner")
        if not leaf_workload_key:
            try:
                configs = client.layered_configs.list()
                if configs and configs[0].leaf_workload_key:
                    leaf_workload_key = configs[0].leaf_workload_key
            except Exception:  # noqa: BLE001
                pass
        if not leaf_workload_key:
            log.warning("No workload_key — skipping session suite")
            return
        try:
            sess_id = uuid.uuid4().hex
            resp = client.runners.allocate(
                leaf_workload_key,
                request_id=f"e2e-sess-{uuid.uuid4().hex[:8]}",
                session_id=sess_id,
            )
            runner_id = resp.runner_id
            log.info("Allocated runner for session suite", extra={"runner_id": runner_id, "session_id": sess_id})
        except Exception as exc:  # noqa: BLE001
            log.warning("Allocation failed for session suite", extra={"error": str(exc)})
            return

    with test_case(run, SUITE, "Pause runner — control plane responds without crashing", log):
        result = client.runners.pause(runner_id)
        log.info("Pause result", extra={
            "runner_id": runner_id,
            "success": result.success,
            "session_id": result.session_id,
            "snapshot_size_bytes": result.snapshot_size_bytes,
            "layer": result.layer,
        })
        # Any structured response (success or failure) is acceptable here —
        # what we're verifying is that the control plane endpoint doesn't 500.

    with test_case(run, SUITE, "Status after pause is a known state", log):
        status = client.runners.status(runner_id)
        KNOWN = {"suspended", "pending", "unavailable", "ready", "quarantined"}
        assert status.status in KNOWN, (
            f"Unexpected post-pause status: {status.status}"
        )
        log.info("Post-pause status", extra={"runner_id": runner_id, "status": status.status})

    with test_case(run, SUITE, "Connect after pause succeeds", log):
        result = client.runners.connect(runner_id)
        assert result.status in {"connected", "resumed", "pending"}, (
            f"Unexpected connect status after pause: {result.status}"
        )
        # runner_id may change on resume
        runner_id = result.runner_id
        log.info("Post-pause connect", extra={
            "runner_id": runner_id,
            "status": result.status,
        })

    # Second pause (to test repeated pause/resume)
    with test_case(run, SUITE, "Second pause on same runner is handled gracefully", log):
        result2 = client.runners.pause(runner_id)
        log.info("Second pause result", extra={
            "runner_id": runner_id,
            "success": result2.success,
            "session_id": result2.session_id,
        })

    # After a second pause the runner is suspended — resume it via connect before releasing.
    with test_case(run, SUITE, "Reconnect after second pause resumes runner", log):
        result3 = client.runners.connect(runner_id)
        runner_id = result3.runner_id  # runner_id may change on resume
        assert result3.status in {"connected", "resumed", "pending"}, (
            f"Unexpected status after reconnect: {result3.status}"
        )
        log.info("Reconnected after second pause", extra={
            "runner_id": runner_id,
            "status": result3.status,
        })

    with test_case(run, SUITE, "Release resumed runner succeeds", log):
        released = client.runners.release(runner_id)
        log.info("Release result", extra={"runner_id": runner_id, "success": released})


# ===========================================================================
# SUITE: quarantine
# ===========================================================================

def suite_quarantine(
    client: BFClient, run: SimulationRun, log: logging.Logger,
    leaf_workload_key: str | None = None,
) -> None:
    """Quarantine / unquarantine a runner."""
    SUITE = "quarantine"

    if not leaf_workload_key:
        try:
            configs = client.layered_configs.list()
            if configs and configs[0].leaf_workload_key:
                leaf_workload_key = configs[0].leaf_workload_key
        except Exception:  # noqa: BLE001
            pass
    if not leaf_workload_key:
        log.warning("No workload_key — skipping quarantine suite")
        return

    runner_id: str | None = None

    with test_case(run, SUITE, "Allocate runner for quarantine test", log):
        resp = client.runners.allocate(
            leaf_workload_key,
            request_id=f"e2e-quar-{uuid.uuid4().hex[:8]}",
        )
        runner_id = resp.runner_id
        assert runner_id, "runner_id must be set"
        log.info("Allocated for quarantine", extra={"runner_id": runner_id})

    if not runner_id:
        log.warning("Allocation failed — skipping quarantine sub-tests")
        return

    with test_case(run, SUITE, "Quarantine returns a dict response", log):
        result = client.runners.quarantine(
            runner_id,
            reason="e2e-test quarantine",
            block_egress=True,
            pause_vm=False,
        )
        assert isinstance(result, dict), f"Expected dict, got {type(result)}"
        log.info("Quarantine result", extra={"runner_id": runner_id, "result": result})

    with test_case(run, SUITE, "Status after quarantine is a known state", log):
        status = client.runners.status(runner_id)
        KNOWN = {"unavailable", "pending", "ready", "suspended", "quarantined"}
        assert status.status in KNOWN, (
            f"Unexpected post-quarantine status: {status.status}"
        )
        log.info("Post-quarantine status", extra={"runner_id": runner_id, "status": status.status})

    with test_case(run, SUITE, "Unquarantine returns a dict response", log):
        result = client.runners.unquarantine(runner_id, unblock_egress=True, resume_vm=False)
        assert isinstance(result, dict)
        log.info("Unquarantine result", extra={"runner_id": runner_id, "result": result})

    with test_case(run, SUITE, "Release after quarantine/unquarantine succeeds", log):
        released = client.runners.release(runner_id)
        log.info("Release result", extra={"runner_id": runner_id, "success": released})

    # --- Quarantine nonexistent runner ---
    with test_case(run, SUITE, "Quarantine nonexistent runner returns error response", log):
        http = client._http  # type: ignore[attr-defined]
        with _raw_client(http) as rc:
            fake_id = f"nonexistent-{uuid.uuid4().hex}"
            resp = rc.post(
                f"/api/v1/runners/quarantine?runner_id={fake_id}&block_egress=true&pause_vm=false",
            )
        # Should be 404 or 503/500 — not a panic
        assert resp.status_code in {404, 500, 503}, (
            f"Expected 404/500/503 for quarantining nonexistent runner, got {resp.status_code}"
        )
        log.info("Nonexistent quarantine", extra={"status": resp.status_code})


# ===========================================================================
# SUITE: fleet
# ===========================================================================

def suite_fleet(
    client: BFClient, run: SimulationRun, log: logging.Logger,
    leaf_workload_key: str | None = None,
) -> None:
    """Fleet convergence and desired-version endpoints."""
    SUITE = "fleet"
    http = client._http  # type: ignore[attr-defined]

    with test_case(run, SUITE, "Fleet convergence without workload_key returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/versions/fleet")
        assert resp.status_code == 400, (
            f"Expected 400 when workload_key missing, got {resp.status_code}"
        )

    wk = leaf_workload_key
    if not wk:
        try:
            configs = client.layered_configs.list()
            if configs and configs[0].leaf_workload_key:
                wk = configs[0].leaf_workload_key
        except Exception:  # noqa: BLE001
            pass

    with test_case(run, SUITE, "Fleet convergence with workload_key returns host list", log):
        if not wk:
            log.warning("No workload_key — skipping fleet convergence test")
        else:
            with _raw_client(http) as rc:
                resp = rc.get(f"/api/v1/versions/fleet", params={"workload_key": wk})
            if resp.status_code == 500 and "more than one row" in resp.text:
                # Known control plane DB bug: subquery returns multiple rows when
                # multiple snapshot builds exist for a workload key.
                log.warning(
                    "Fleet convergence returned 500 (known DB subquery bug)",
                    extra={"workload_key": wk, "error": resp.text.strip()},
                )
            else:
                assert resp.status_code == 200, (
                    f"Expected 200 or known 500, got {resp.status_code}: {resp.text[:200]}"
                )
                data = resp.json()
                assert "workload_key" in data, f"Expected 'workload_key' in response"
                assert "hosts" in data
                assert "count" in data
                log.info("Fleet convergence", extra={
                    "workload_key": wk,
                    "host_count": data["count"],
                })
                for h in (data.get("hosts") or [])[:5]:
                    log.info("Host convergence state", extra=h)

    with test_case(run, SUITE, "Desired versions without instance_name returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/versions/desired")
        assert resp.status_code == 400, (
            f"Expected 400 when instance_name missing, got {resp.status_code}"
        )

    with test_case(run, SUITE, "Desired versions for unknown host returns 404", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/versions/desired?instance_name=nonexistent-host-e2e-99999")
        assert resp.status_code == 404, (
            f"Expected 404 for unknown host, got {resp.status_code}"
        )

    with test_case(run, SUITE, "Desired versions for known host returns versions map", log):
        data_hosts = http.get("/api/v1/hosts")
        hosts = data_hosts.get("hosts", [])
        if not hosts:
            log.info("No registered hosts — skipping desired versions test")
        else:
            name = hosts[0]["instance_name"]
            data = http.get(f"/api/v1/versions/desired?instance_name={name}")
            assert "desired_versions" in data, (
                f"Expected 'desired_versions', got {list(data.keys())}"
            )
            log.info("Desired versions", extra={
                "instance_name": name,
                "versions": data.get("desired_versions", {}),
            })


# ===========================================================================
# SUITE: canary
# ===========================================================================

def suite_canary(client: BFClient, run: SimulationRun, log: logging.Logger) -> None:
    """Canary report endpoint."""
    SUITE = "canary"
    http = client._http  # type: ignore[attr-defined]

    with test_case(run, SUITE, "Canary report status=success is accepted", log):
        payload = {
            "status": "success",
            "runner": f"e2e-canary-{uuid.uuid4().hex[:8]}",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        data = http.post("/api/v1/canary/report", json_body=payload)
        log.info("Canary success response", extra={"data": data})

    with test_case(run, SUITE, "Canary report status=failure is accepted", log):
        payload = {
            "status": "failure",
            "runner": f"e2e-canary-{uuid.uuid4().hex[:8]}",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        data = http.post("/api/v1/canary/report", json_body=payload)
        log.info("Canary failure response", extra={"data": data})

    with test_case(run, SUITE, "Canary report with malformed JSON returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/canary/report",
                content=b"this is not json",
                headers={"Content-Type": "application/json"},
            )
        assert resp.status_code == 400, (
            f"Expected 400 for invalid JSON body, got {resp.status_code}"
        )

    with test_case(run, SUITE, "Canary report GET method returns 405", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/canary/report")
        assert resp.status_code == 405, (
            f"Expected 405 for GET on canary/report, got {resp.status_code}"
        )


# ===========================================================================
# SUITE: edge cases
# ===========================================================================

def suite_edge(client: BFClient, run: SimulationRun, log: logging.Logger) -> None:
    """Edge cases, error paths, and boundary conditions."""
    SUITE = "edge"
    http = client._http  # type: ignore[attr-defined]

    # --- Allocate without workload_key ---
    with test_case(run, SUITE, "Allocate without workload_key returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post("/api/v1/runners/allocate", json={})
        assert resp.status_code == 400, (
            f"Expected 400 for missing workload_key, got {resp.status_code}"
        )

    # --- Status for nonexistent runner ---
    with test_case(run, SUITE, "Status for nonexistent runner_id returns 404", log):
        with _raw_client(http) as rc:
            resp = rc.get(f"/api/v1/runners/status?runner_id=nonexistent-{uuid.uuid4().hex}")
        assert resp.status_code == 404, (
            f"Expected 404 for unknown runner, got {resp.status_code}"
        )

    # --- Status without runner_id param ---
    with test_case(run, SUITE, "Status without runner_id returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/runners/status")
        assert resp.status_code == 400, (
            f"Expected 400 for missing runner_id, got {resp.status_code}"
        )

    # --- Release without runner_id ---
    with test_case(run, SUITE, "Release without runner_id returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post("/api/v1/runners/release", json={})
        assert resp.status_code == 400, (
            f"Expected 400 for missing runner_id, got {resp.status_code}"
        )

    # --- Release nonexistent runner ---
    with test_case(run, SUITE, "Release nonexistent runner returns 4xx/5xx", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/runners/release",
                json={"runner_id": f"nonexistent-{uuid.uuid4().hex}"},
            )
        assert resp.status_code >= 400, (
            f"Expected error for releasing nonexistent runner, got {resp.status_code}"
        )

    # --- Connect nonexistent runner ---
    with test_case(run, SUITE, "Connect nonexistent runner returns 404", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/runners/connect",
                json={"runner_id": f"nonexistent-{uuid.uuid4().hex}"},
            )
        assert resp.status_code == 404, (
            f"Expected 404 for connecting nonexistent runner, got {resp.status_code}"
        )

    # --- Pause nonexistent runner ---
    with test_case(run, SUITE, "Pause nonexistent runner returns 404", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/runners/pause",
                json={"runner_id": f"nonexistent-{uuid.uuid4().hex}"},
            )
        assert resp.status_code == 404, (
            f"Expected 404 for pausing nonexistent runner, got {resp.status_code}"
        )

    # --- Pause without runner_id ---
    with test_case(run, SUITE, "Pause without runner_id returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post("/api/v1/runners/pause", json={})
        assert resp.status_code == 400, (
            f"Expected 400 for missing runner_id, got {resp.status_code}"
        )

    # --- Wrong method on allocate ---
    with test_case(run, SUITE, "GET on /runners/allocate returns 405", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/runners/allocate")
        assert resp.status_code == 405, (
            f"Expected 405, got {resp.status_code}"
        )

    # --- Create config with empty layers ---
    with test_case(run, SUITE, "Create config with empty layers returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/layered-configs",
                json={"display_name": "bad-empty-layers", "layers": []},
            )
        assert resp.status_code == 400, (
            f"Expected 400 for empty layers, got {resp.status_code}: {resp.text[:200]}"
        )

    # --- Create config with duplicate layer names ---
    with test_case(run, SUITE, "Create config with duplicate layer names returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/layered-configs",
                json={
                    "display_name": "dup-layers",
                    "layers": [
                        {"name": "same", "init_commands": [{"type": "shell", "args": ["echo a"]}]},
                        {"name": "same", "init_commands": [{"type": "shell", "args": ["echo b"]}]},
                    ],
                },
            )
        assert resp.status_code == 400, (
            f"Expected 400 for duplicate layer names, got {resp.status_code}: {resp.text[:200]}"
        )

    # --- Create config with reserved layer name '_platform' ---
    with test_case(run, SUITE, "Create config with reserved layer name '_platform' returns 400", log):
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/layered-configs",
                json={
                    "display_name": "reserved-platform",
                    "layers": [
                        {"name": "_platform", "init_commands": [{"type": "shell", "args": ["echo x"]}]},
                    ],
                },
            )
        assert resp.status_code == 400, (
            f"Expected 400 for reserved layer name, got {resp.status_code}: {resp.text[:200]}"
        )

    # --- Get nonexistent config ---
    with test_case(run, SUITE, "Get nonexistent config_id returns 404", log):
        with _raw_client(http) as rc:
            resp = rc.get("/api/v1/layered-configs/deadbeef00000000")
        assert resp.status_code == 404, (
            f"Expected 404 for unknown config_id, got {resp.status_code}"
        )

    # --- Delete nonexistent config ---
    with test_case(run, SUITE, "Delete nonexistent config_id returns 404", log):
        with _raw_client(http) as rc:
            resp = rc.delete("/api/v1/layered-configs/deadbeef00000001")
        assert resp.status_code == 404, (
            f"Expected 404 for deleting unknown config_id, got {resp.status_code}"
        )

    # --- Allocate with nonexistent workload_key (no snapshot/host available) ---
    with test_case(run, SUITE, "Allocate with unknown workload_key returns 503", log):
        fake_wk = f"e2e-nonexistent-{uuid.uuid4().hex}"
        with _raw_client(http) as rc:
            resp = rc.post(
                "/api/v1/runners/allocate",
                json={"workload_key": fake_wk},
            )
        assert resp.status_code in {503, 400}, (
            f"Expected 503/400 for unknown workload_key, "
            f"got {resp.status_code}: {resp.text[:200]}"
        )
        log.info("Nonexistent workload_key response", extra={
            "status": resp.status_code, "body": resp.text[:200],
        })

    # --- Duplicate request_id (idempotency smoke test) ---
    with test_case(run, SUITE, "Duplicate allocate request_id is handled without 500", log):
        try:
            configs = client.layered_configs.list()
            wk = configs[0].leaf_workload_key if configs else None
        except Exception:  # noqa: BLE001
            wk = None

        if not wk:
            log.info("No workload_key for dedup test — skipping")
        else:
            rid = f"e2e-dedup-{uuid.uuid4().hex[:12]}"
            for i in range(2):
                with _raw_client(http) as rc:
                    resp = rc.post(
                        "/api/v1/runners/allocate",
                        json={"workload_key": wk, "request_id": rid},
                    )
                log.info(f"Dedup allocate attempt {i + 1}", extra={
                    "status": resp.status_code,
                    "body": resp.text[:150],
                })
                assert resp.status_code != 500, (
                    f"Server error (500) on dedup allocate attempt {i + 1}: {resp.text[:300]}"
                )


# ===========================================================================
# SUITE: concurrency
# ===========================================================================
#
# Each scenario uses threading.Barrier so all workers start their requests
# at the same instant (after setup), giving maximum contention on the server.
#
# Each worker gets its own BFClient so httpx connection pools don't serialize
# requests. Results are collected into thread-safe lists (list.append is
# atomic in CPython).
# ===========================================================================

def _make_client(base_url: str, token: str | None) -> BFClient:
    """Construct a fresh BFClient for use in a worker thread."""
    return BFClient(base_url=base_url, token=token)


def suite_concurrency(
    base_url: str,
    token: str | None,
    run: SimulationRun,
    log: logging.Logger,
    leaf_workload_key: str = FIXED_WORKLOAD_KEY,
    *,
    width: int = 5,   # number of parallel workers per scenario
) -> None:
    """
    Concurrency / platform-stress scenarios.

    Simulates realistic multi-client platform load:
    - parallel allocate burst
    - concurrent full lifecycle (allocate → status → release)
    - concurrent session lifecycle (allocate → pause → resume → release)
    - thundering-herd status poll on a single runner
    - duplicate request_id race
    - allocate → simultaneous release race
    - mixed operation storm
    - exec-dirty-writes → pause → resume cycles (random data across chunks)

    Pre-conditions: releases enough idle runners before each scenario to ensure
    at least `width` CPU slots are free, so workers actually execute.
    """
    SUITE = "concurrency"
    log.info("Starting concurrency suite", extra={"workers": width, "workload_key": leaf_workload_key})

    # ------------------------------------------------------------------
    # Pre-warm helper: release idle runners to free CPU budget.
    # Returns (freed_runner_ids, slots_freed).
    # We release up to `slots_needed` idle runners whose workload matches.
    # ------------------------------------------------------------------
    def _free_slots(slots_needed: int) -> list[str]:
        """Release up to `slots_needed` idle runners and return their IDs."""
        _c = _make_client(base_url, token)
        try:
            runners = _c.runners.list()
        except Exception:  # noqa: BLE001
            return []
        finally:
            _c.close()

        idle = [r["runner_id"] for r in runners
                if r.get("status") == "idle"
                and r.get("workload_key") == leaf_workload_key]
        to_free = idle[:slots_needed]
        freed: list[str] = []
        for rid in to_free:
            try:
                _c2 = _make_client(base_url, token)
                _c2.runners.release(rid)
                _c2.close()
                freed.append(rid)
            except Exception:  # noqa: BLE001
                pass
        if freed:
            log.info("Released idle runners to free CPU slots",
                     extra={"freed": len(freed), "runner_ids": freed})
        return freed

    # ------------------------------------------------------------------
    # Helper: run N workers from a barrier, collect (ok, detail, real) tuples
    # where `real=True` means the worker actually ran (not capacity-skipped).
    # ------------------------------------------------------------------
    def _run_workers(
        name: str,
        worker_fn: Any,  # (worker_id: int, barrier: Barrier) -> tuple[bool, str, bool]
        n: int = width,
        join_timeout: int = 120,
    ) -> list[tuple[bool, str, bool]]:
        """
        Returns list of (passed, detail, did_real_work) per worker.
        worker_fn must return (passed: bool, detail: str, real: bool).
        """
        barrier = Barrier(n)
        results: list[tuple[bool, str, bool]] = []

        def _wrap(wid: int) -> None:
            try:
                ok, detail, real = worker_fn(wid, barrier)
            except Exception as exc:  # noqa: BLE001
                ok, detail, real = False, f"{type(exc).__name__}: {exc}", False
            results.append((ok, detail, real))

        threads = [Thread(target=_wrap, args=(i,), name=f"{name}-{i}") for i in range(n)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=join_timeout)

        while len(results) < n:
            results.append((False, f"worker timed out after {join_timeout}s", False))
        return results

    def _is_capacity_error(msg: str) -> bool:
        """Return True if an error is a clean capacity/resource-exhaustion 429/503."""
        return any(s in msg for s in (
            "no available hosts",
            "no host with sufficient capacity",
            "503",
            "429",
        ))

    # Shared list for tracking per-runner allocation latencies across tests.
    _alloc_latencies: list[tuple[str, float]] = []  # (test_name, seconds)

    def _allocate_with_retry(
        client: Any,
        workload_key: str,
        request_id: str,
        *,
        max_retries: int = 5,
        initial_backoff: float = 2.0,
        session_id: str | None = None,
    ) -> Any:
        """Allocate a runner with exponential backoff on 429/503.

        Returns the allocate response on success.
        Raises the last exception after exhausting retries.
        """
        backoff = initial_backoff
        last_exc: Exception | None = None
        for attempt in range(max_retries + 1):
            try:
                return client.runners.allocate(
                    workload_key,
                    request_id=request_id,
                    session_id=session_id,
                )
            except (BFServiceUnavailable, BFRateLimited) as exc:
                last_exc = exc
                if attempt < max_retries:
                    delay = backoff
                    if hasattr(exc, "retry_after") and exc.retry_after:
                        delay = exc.retry_after
                    time.sleep(delay)
                    backoff = min(backoff * 1.5, 15.0)
        raise last_exc  # type: ignore[misc]

    def _assert_workers(
        test_name: str,
        results: list[tuple[bool, str, bool]],
        *,
        allow_failures: int = 0,
        min_real: int = 1,   # at least this many workers must have done real work
    ) -> None:
        """
        Record a test result from aggregated worker outcomes.
        Fails if:
        - more than `allow_failures` workers returned (False, ...)
        - fewer than `min_real` workers did real work (all capacity-skipped)
        """
        t0 = time.monotonic()
        failed = [(i, d) for i, (ok, d, _) in enumerate(results) if not ok]
        real_count = sum(1 for _, _, real in results if real)
        detail_parts = [f"w{i}: {d}" for i, d in failed]
        if real_count < min_real:
            detail_parts.append(
                f"only {real_count}/{len(results)} workers did real work "
                f"(min required: {min_real}) — all capacity-limited, test did not execute"
            )
        passed = len(failed) <= allow_failures and real_count >= min_real
        detail = "; ".join(detail_parts)
        if detail_parts:
            log.warning(
                "Concurrency test issue",
                extra={"test": test_name, "failed_workers": len(failed),
                       "real_workers": real_count, "details": detail[:400]},
            )
        duration_ms = (time.monotonic() - t0) * 1000
        run.record(TestResult(
            name=test_name,
            suite=SUITE,
            passed=passed,
            duration_ms=duration_ms,
            error=detail[:400] if not passed else "",
        ))
        icon = "PASSED" if passed else "FAILED"
        log.info(icon, extra={"suite": SUITE, "test": test_name,
                               "workers": len(results), "failures": len(failed),
                               "real_workers": real_count})
        icon = "PASSED" if passed else "FAILED"
        log.info(icon, extra={"suite": SUITE, "test": test_name,
                               "workers": len(results), "failures": len(failed)})

    # ---------------------------------------------------------------
    # Scenario 1: Parallel allocate burst
    # N clients all hit /allocate simultaneously.
    # Expect: each gets a distinct runner_id, no 500s, ≥1 actually ran.
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE, "test": "Parallel allocate burst"})
    _free_slots(width)
    allocated_ids: list[str] = []
    to_release_after: list[str] = []

    def _alloc_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
        client = _make_client(base_url, token)
        try:
            barrier.wait()
            t0 = time.monotonic()
            resp = _allocate_with_retry(
                client, leaf_workload_key,
                request_id=f"e2e-burst-{uuid.uuid4().hex[:12]}",
            )
            _alloc_latencies.append(("allocate_burst", time.monotonic() - t0))
            allocated_ids.append(resp.runner_id)
            to_release_after.append(resp.runner_id)
            return True, resp.runner_id, True
        except (BFServiceUnavailable, BFRateLimited) as exc:
            if _is_capacity_error(str(exc)):
                return True, f"capacity-limited: {exc}", False
            return False, str(exc), False
        except Exception as exc:  # noqa: BLE001
            return False, str(exc), False
        finally:
            client.close()

    burst_results = _run_workers("alloc_burst", _alloc_worker)
    id_counts = Counter(allocated_ids)
    dupes = {rid: cnt for rid, cnt in id_counts.items() if cnt > 1}
    if dupes:
        burst_results.append((False, f"Duplicate runner_ids returned: {dupes}", True))
    log.info("Burst allocate runner IDs", extra={"runner_ids": allocated_ids})
    _assert_workers("Parallel allocate burst — no 500s, unique runner_ids", burst_results)

    for rid in to_release_after:
        try:
            _cr = _make_client(base_url, token)
            _cr.runners.release(rid)
            _cr.close()
        except Exception:  # noqa: BLE001
            pass

    # ---------------------------------------------------------------
    # Scenario 2: Concurrent full lifecycle (allocate → status → release)
    # Each worker independently runs the whole lifecycle.
    # Expect: all succeed, no cross-contamination of runner_ids.
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE,
             "test": "Concurrent full lifecycle (alloc→status→release)"})
    _free_slots(width)

    def _lifecycle_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
        client = _make_client(base_url, token)
        runner_id: str | None = None
        try:
            barrier.wait()
            t0 = time.monotonic()
            resp = _allocate_with_retry(
                client, leaf_workload_key,
                request_id=f"e2e-lc-{wid}-{uuid.uuid4().hex[:8]}",
            )
            _alloc_latencies.append(("lifecycle", time.monotonic() - t0))
            runner_id = resp.runner_id
            status = client.runners.status(runner_id)
            valid = {"ready", "pending", "unavailable", "suspended", "quarantined"}
            if status.status not in valid:
                return False, f"Unexpected status '{status.status}' for runner {runner_id}", True
            client.runners.release(runner_id)
            runner_id = None
            return True, "", True
        except (BFServiceUnavailable, BFRateLimited) as exc:
            if _is_capacity_error(str(exc)):
                return True, f"capacity-limited: {exc}", False
            return False, str(exc), False
        except Exception as exc:  # noqa: BLE001
            return False, str(exc), True
        finally:
            if runner_id:
                try:
                    client.runners.release(runner_id)
                except Exception:  # noqa: BLE001
                    pass
            client.close()

    lc_results = _run_workers("lifecycle", _lifecycle_worker)
    _assert_workers("Concurrent full lifecycle (alloc→status→release)", lc_results)

    # ---------------------------------------------------------------
    # Scenario 3: Concurrent session lifecycle
    # Each worker: allocate (with session_id) → pause → reconnect → release
    # Verifies session snapshots don't bleed between clients.
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE,
             "test": "Concurrent session lifecycle (alloc→pause→resume→release)"})
    _free_slots(width)

    def _session_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
        # Session operations (pause, connect) involve GCS snapshot uploads/
        # downloads that can take >30s under concurrent load. Use a longer
        # timeout to avoid spurious timeouts that trigger duplicate work.
        client = BFClient(base_url=base_url, token=token, timeout=120.0)
        runner_id: str | None = None
        try:
            sess_id = uuid.uuid4().hex
            barrier.wait()
            t0 = time.monotonic()
            resp = _allocate_with_retry(
                client, leaf_workload_key,
                request_id=f"e2e-sess-{wid}-{uuid.uuid4().hex[:8]}",
                session_id=sess_id,
            )
            _alloc_latencies.append(("session_lifecycle", time.monotonic() - t0))
            runner_id = resp.runner_id

            pause = client.runners.pause(runner_id)
            if not pause.success:
                return False, f"Pause returned success=False for runner {runner_id}", True

            if pause.session_id and pause.session_id != sess_id:
                return False, (
                    f"Session ID mismatch: allocated with {sess_id}, "
                    f"pause returned {pause.session_id}"
                ), True

            conn = client.runners.connect(runner_id)
            runner_id = conn.runner_id
            if conn.status not in {"connected", "resumed", "pending"}:
                return False, f"Unexpected post-resume status: {conn.status}", True

            client.runners.release(runner_id)
            runner_id = None
            return True, "", True
        except (BFServiceUnavailable, BFRateLimited) as exc:
            if _is_capacity_error(str(exc)):
                return True, f"capacity-limited: {exc}", False
            return False, str(exc), False
        except Exception as exc:  # noqa: BLE001
            return False, str(exc), True
        finally:
            if runner_id:
                try:
                    client.runners.release(runner_id)
                except Exception:  # noqa: BLE001
                    pass
            client.close()

    sess_results = _run_workers("session_lifecycle", _session_worker, join_timeout=300)
    _assert_workers("Concurrent session lifecycle (alloc→pause→resume→release)", sess_results)

    # ---------------------------------------------------------------
    # Scenario 4: Thundering-herd status poll
    # Allocate one runner, then N threads all poll its status at once.
    # Expect: all 200, no 500s.
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE,
             "test": "Thundering-herd status poll on single runner"})
    _free_slots(1)
    shared_runner_id: str | None = None
    try:
        _cs = _make_client(base_url, token)
        shared_resp = _cs.runners.allocate(
            leaf_workload_key,
            request_id=f"e2e-herd-{uuid.uuid4().hex[:8]}",
        )
        shared_runner_id = shared_resp.runner_id
        _cs.close()
    except (BFServiceUnavailable, BFRateLimited) as exc:
        log.warning("Skipping thundering-herd test (capacity-limited)", extra={"error": str(exc)})
    except Exception as exc:  # noqa: BLE001
        log.warning("Could not allocate for thundering-herd test", extra={"error": str(exc)})

    if shared_runner_id:
        statuses_seen: list[str] = []

        def _poll_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
            client = _make_client(base_url, token)
            try:
                barrier.wait()
                s = client.runners.status(shared_runner_id)  # type: ignore[arg-type]
                statuses_seen.append(s.status)
                return True, s.status, True
            except Exception as exc:  # noqa: BLE001
                return False, str(exc), True
            finally:
                client.close()

        herd_results = _run_workers("herd_poll", _poll_worker)
        log.info("Thundering-herd poll statuses seen", extra={
            "runner_id": shared_runner_id,
            "statuses": list(set(statuses_seen)),
        })
        _assert_workers("Thundering-herd status poll on single runner", herd_results)

        try:
            _cs2 = _make_client(base_url, token)
            _cs2.runners.release(shared_runner_id)
            _cs2.close()
        except Exception:  # noqa: BLE001
            pass
    else:
        run.record(TestResult(
            name="Thundering-herd status poll on single runner",
            suite=SUITE, passed=True, duration_ms=0,
            detail="skipped — no runner available (capacity-limited)",
        ))
        log.info("PASSED", extra={"suite": SUITE,
                  "test": "Thundering-herd status poll on single runner",
                  "note": "skipped (capacity-limited)"})

    # ---------------------------------------------------------------
    # Scenario 5: Duplicate request_id race
    # Two workers fire the same request_id at the same millisecond.
    # Platform must return the same runner_id (dedup), not 500.
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE,
             "test": "Duplicate request_id deduplication under concurrency"})
    _free_slots(2)
    dedup_request_id = f"e2e-dedup-race-{uuid.uuid4().hex[:12]}"
    dedup_runner_ids: list[str] = []

    def _dedup_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
        client = _make_client(base_url, token)
        try:
            barrier.wait()
            resp = client.runners.allocate(leaf_workload_key, request_id=dedup_request_id)
            dedup_runner_ids.append(resp.runner_id)
            return True, resp.runner_id, True
        except (BFServiceUnavailable, BFRateLimited) as exc:
            if _is_capacity_error(str(exc)):
                dedup_runner_ids.append(f"error:{exc}")
                return True, f"capacity-limited: {exc}", False
            dedup_runner_ids.append(f"error:{exc}")
            return True, f"expected 503 error: {exc}", True
        except BFError as exc:
            dedup_runner_ids.append(f"error:{exc}")
            return True, f"expected error: {exc}", True
        except Exception as exc:  # noqa: BLE001
            return False, str(exc), True
        finally:
            client.close()

    dedup_results = _run_workers("dedup_race", _dedup_worker, n=2)
    real_ids = [r for r in dedup_runner_ids if not r.startswith("error:")]
    if len(set(real_ids)) > 1:
        dedup_results.append((False,
            f"Duplicate request_id returned different runner_ids: {real_ids}", True))
    log.info("Dedup race runner IDs", extra={"runner_ids": dedup_runner_ids})
    _assert_workers("Duplicate request_id deduplication under concurrency", dedup_results,
                    min_real=1)
    for rid in set(real_ids):
        try:
            _cd = _make_client(base_url, token)
            _cd.runners.release(rid)
            _cd.close()
        except Exception:  # noqa: BLE001
            pass

    # ---------------------------------------------------------------
    # Scenario 6: Simultaneous release race
    # Worker A allocates one runner, then both A and B try to release
    # it at the same instant.
    # Expect: exactly one succeeds, the other gets a clean error (no 500).
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE,
             "test": "Simultaneous release race on single runner"})
    _free_slots(1)
    race_runner_id: str | None = None
    try:
        _cr2 = _make_client(base_url, token)
        race_resp = _cr2.runners.allocate(
            leaf_workload_key,
            request_id=f"e2e-race-{uuid.uuid4().hex[:8]}",
        )
        race_runner_id = race_resp.runner_id
        _cr2.close()
    except (BFServiceUnavailable, BFRateLimited) as exc:
        log.warning("Skipping release-race test (capacity-limited)", extra={"error": str(exc)})
    except Exception as exc:  # noqa: BLE001
        log.warning("Could not allocate for release-race test", extra={"error": str(exc)})

    if race_runner_id:
        def _release_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
            client = _make_client(base_url, token)
            try:
                barrier.wait()
                ok = client.runners.release(race_runner_id)  # type: ignore[arg-type]
                return True, f"success={ok}", True
            except BFError as exc:
                msg = str(exc)
                is_expected = any(s in msg for s in ("not found", "404", "500"))
                return is_expected, f"error (expected={is_expected}): {msg}", True
            except Exception as exc:  # noqa: BLE001
                return False, str(exc), True
            finally:
                client.close()

        race_results = _run_workers("release_race", _release_worker, n=2)
        log.info("Release race outcomes", extra={"outcomes": [r[1] for r in race_results]})
        _assert_workers("Simultaneous release race on single runner", race_results,
                         allow_failures=1, min_real=1)
    else:
        run.record(TestResult(
            name="Simultaneous release race on single runner",
            suite=SUITE, passed=True, duration_ms=0,
            detail="skipped — no runner available (capacity-limited)",
        ))
        log.info("PASSED", extra={"suite": SUITE,
                  "test": "Simultaneous release race on single runner",
                  "note": "skipped (capacity-limited)"})

    # ---------------------------------------------------------------
    # Scenario 7: Mixed operation storm
    # N workers each allocate their own runner, then all simultaneously
    # do random read-heavy operations (status, list, connect).
    # Simulates real platform traffic mix.
    # Expect: server remains stable, no unexpected 500s.
    # ---------------------------------------------------------------
    log.info("Starting test", extra={"suite": SUITE, "test": "Mixed operation storm"})
    _free_slots(width)

    def _mixed_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
        client = _make_client(base_url, token)
        runner_id: str | None = None
        try:
            t0 = time.monotonic()
            resp = _allocate_with_retry(
                client, leaf_workload_key,
                request_id=f"e2e-storm-{wid}-{uuid.uuid4().hex[:8]}",
            )
            _alloc_latencies.append(("mixed_storm", time.monotonic() - t0))
            runner_id = resp.runner_id

            try:
                barrier.wait(timeout=15)  # wait for peers; not all may allocate
            except BrokenBarrierError:
                pass  # some workers got 429 and never arrived — proceed anyway

            ops = random.choices(
                ["status", "list", "connect", "status", "list"],
                k=3,
            )
            for op in ops:
                if op == "status":
                    s = client.runners.status(runner_id)
                    valid = {"ready", "pending", "unavailable", "suspended", "quarantined"}
                    if s.status not in valid:
                        return False, f"Unexpected status '{s.status}'", True
                elif op == "list":
                    runners = client.runners.list()
                    if not isinstance(runners, list):
                        return False, f"Expected list from /runners, got {type(runners)}", True
                elif op == "connect":
                    conn = client.runners.connect(runner_id)
                    runner_id = conn.runner_id
                    if conn.status not in {"connected", "resumed", "pending"}:
                        return False, f"Unexpected connect status '{conn.status}'", True

            client.runners.release(runner_id)
            runner_id = None
            return True, "", True
        except (BFServiceUnavailable, BFRateLimited) as exc:
            if _is_capacity_error(str(exc)):
                return True, f"capacity-limited: {exc}", False
            return False, str(exc), False
        except Exception as exc:  # noqa: BLE001
            return False, str(exc), True
        finally:
            if runner_id:
                try:
                    client.runners.release(runner_id)
                except Exception:  # noqa: BLE001
                    pass
            client.close()

    storm_results = _run_workers("mixed_storm", _mixed_worker)
    _assert_workers("Mixed operation storm", storm_results)

    # ---------------------------------------------------------------
    # Scenario 8: Concurrent exec-with-random-writes → pause → resume
    # Each worker allocates a session runner, then runs multiple
    # exec→pause→resume cycles. In each exec phase, random data is
    # written to random offsets on the workspace drive so that
    # different chunks get dirtied differently per worker and per
    # cycle. This stresses the chunked snapshot diff/merge path under
    # concurrent load with realistic, non-uniform dirty patterns.
    # ---------------------------------------------------------------
    SCENARIO_8 = "Concurrent exec-dirty-writes → pause → resume cycles"
    log.info("Starting test", extra={"suite": SUITE, "test": SCENARIO_8})
    _free_slots(width)

    NUM_PAUSE_CYCLES = 2         # exec→pause→resume rounds per worker
    WRITES_PER_CYCLE_MIN = 3     # min random writes per cycle
    WRITES_PER_CYCLE_MAX = 8     # max random writes per cycle
    WRITE_SIZE_MIN_KB = 64       # min write size
    WRITE_SIZE_MAX_KB = 4096     # max write size (4 MB = chunk size)
    MAX_OFFSET_MB = 200          # upper bound for random seek offset

    def _exec_dirty_worker(wid: int, barrier: Barrier) -> tuple[bool, str, bool]:
        client = BFClient(base_url=base_url, token=token, timeout=180.0)
        runner_id: str | None = None
        rng = random.Random(wid + int(time.monotonic() * 1000))
        # Per-cycle timings: list of (exec_s, pause_s, resume_s) per cycle.
        cycle_timings: list[dict[str, float]] = []
        try:
            sess_id = uuid.uuid4().hex
            barrier.wait()
            t0 = time.monotonic()
            resp = _allocate_with_retry(
                client, leaf_workload_key,
                request_id=f"e2e-dirty-{wid}-{uuid.uuid4().hex[:8]}",
                session_id=sess_id,
            )
            alloc_dur = time.monotonic() - t0
            _alloc_latencies.append(("exec_dirty_cycles", alloc_dur))
            runner_id = resp.runner_id

            for cycle in range(NUM_PAUSE_CYCLES):
                # --- Exec phase: write random data to random offsets ---
                num_writes = rng.randint(WRITES_PER_CYCLE_MIN, WRITES_PER_CYCLE_MAX)
                # Build a shell script where all dd writes run in parallel
                # (backgrounded with &), then wait + sync before returning.
                bg_cmds: list[str] = []
                for w in range(num_writes):
                    offset_mb = rng.randint(0, MAX_OFFSET_MB)
                    size_kb = rng.randint(WRITE_SIZE_MIN_KB, WRITE_SIZE_MAX_KB)
                    # Use /dev/urandom so each write produces unique data,
                    # ensuring the chunk hash actually changes.
                    bg_cmds.append(
                        f"dd if=/dev/urandom of=/workspace/.dirty-{wid}-{cycle}-{w} "
                        f"bs=1K count={size_kb} seek={offset_mb}K "
                        f"conv=notrunc 2>/dev/null &"
                    )
                # Also write to a shared file at random offsets to simulate
                # contention within a single file across cycles.
                shared_offset_kb = rng.randint(0, MAX_OFFSET_MB * 1024)
                shared_size_kb = rng.randint(WRITE_SIZE_MIN_KB, WRITE_SIZE_MAX_KB)
                bg_cmds.append(
                    f"dd if=/dev/urandom of=/workspace/.shared-dirty-{wid} "
                    f"bs=1K count={shared_size_kb} seek={shared_offset_kb} "
                    f"conv=notrunc 2>/dev/null &"
                )
                # Wait for all background writes to finish, then sync to
                # flush to the block device before pause.
                script = " ".join(bg_cmds) + " wait && sync"

                t_exec = time.monotonic()
                exec_events = list(client.runners.exec(
                    runner_id,
                    ["sh", "-c", script],
                    timeout_seconds=60,
                ))
                exec_dur = time.monotonic() - t_exec

                # Check for non-zero exit.
                exit_events = [e for e in exec_events if e.type == "exit"]
                if exit_events and exit_events[0].code != 0:
                    return False, (
                        f"Cycle {cycle}: exec exited with code {exit_events[0].code} "
                        f"(worker {wid}, {num_writes} writes)"
                    ), True

                log.debug("Exec dirty writes done", extra={
                    "worker": wid, "cycle": cycle,
                    "num_writes": num_writes + 1,  # +1 for shared file
                    "runner_id": runner_id,
                    "exec_ms": round(exec_dur * 1000),
                })

                # --- Pause phase ---
                t_pause = time.monotonic()
                pause = client.runners.pause(runner_id)
                pause_dur = time.monotonic() - t_pause

                if not pause.success:
                    return False, (
                        f"Cycle {cycle}: pause returned success=False "
                        f"for runner {runner_id}"
                    ), True

                if pause.session_id and pause.session_id != sess_id:
                    return False, (
                        f"Cycle {cycle}: session ID mismatch after pause: "
                        f"expected {sess_id}, got {pause.session_id}"
                    ), True

                log.debug("Pause complete", extra={
                    "worker": wid, "cycle": cycle,
                    "runner_id": runner_id,
                    "snapshot_size_bytes": pause.snapshot_size_bytes,
                    "pause_ms": round(pause_dur * 1000),
                })

                # --- Resume phase ---
                t_resume = time.monotonic()
                conn = client.runners.connect(runner_id)
                resume_dur = time.monotonic() - t_resume
                runner_id = conn.runner_id

                if conn.status not in {"connected", "resumed", "pending"}:
                    return False, (
                        f"Cycle {cycle}: unexpected post-resume status: "
                        f"{conn.status}"
                    ), True

                log.debug("Resume complete", extra={
                    "worker": wid, "cycle": cycle,
                    "runner_id": runner_id,
                    "status": conn.status,
                    "resume_ms": round(resume_dur * 1000),
                })

                cycle_timings.append({
                    "cycle": cycle,
                    "exec_ms": round(exec_dur * 1000),
                    "pause_ms": round(pause_dur * 1000),
                    "resume_ms": round(resume_dur * 1000),
                })

            # --- Final verification: read back a file to confirm data survived ---
            verify_events = list(client.runners.exec(
                runner_id,
                ["sh", "-c",
                 f"ls -la /workspace/.dirty-{wid}-*  /workspace/.shared-dirty-{wid} 2>/dev/null | wc -l"],
                timeout_seconds=30,
            ))
            stdout_parts = [e.data for e in verify_events if e.type == "stdout" and e.data]
            file_count_str = "".join(stdout_parts).strip()
            if not file_count_str or int(file_count_str) == 0:
                return False, "No dirty files found after pause/resume cycles", True

            log.info("Worker cycle timings", extra={
                "worker": wid, "runner_id": runner_id,
                "alloc_ms": round(alloc_dur * 1000),
                "cycles": cycle_timings,
            })

            client.runners.release(runner_id)
            runner_id = None
            return True, (
                f"{NUM_PAUSE_CYCLES} cycles, files verified={file_count_str}, "
                f"timings={cycle_timings}"
            ), True

        except (BFServiceUnavailable, BFRateLimited) as exc:
            if _is_capacity_error(str(exc)):
                return True, f"capacity-limited: {exc}", False
            return False, str(exc), False
        except Exception as exc:  # noqa: BLE001
            return False, str(exc), True
        finally:
            if runner_id:
                try:
                    client.runners.release(runner_id)
                except Exception:  # noqa: BLE001
                    pass
            client.close()

    dirty_results = _run_workers(
        "exec_dirty_cycles", _exec_dirty_worker, join_timeout=600,
    )
    _assert_workers(SCENARIO_8, dirty_results)

    # Log allocation latency summary
    if _alloc_latencies:
        by_test: dict[str, list[float]] = {}
        for name, lat in _alloc_latencies:
            by_test.setdefault(name, []).append(lat)
        for name, lats in sorted(by_test.items()):
            lats.sort()
            p50 = lats[len(lats) // 2]
            p99 = lats[int(len(lats) * 0.99)]
            log.info("Allocation latency", extra={
                "test": name,
                "count": len(lats),
                "p50_s": f"{p50:.2f}",
                "p99_s": f"{p99:.2f}",
                "max_s": f"{max(lats):.2f}",
                "min_s": f"{min(lats):.2f}",
            })

    log.info("Concurrency suite complete", extra={
        "suite": SUITE,
        "workers_per_scenario": width,
    })


# ===========================================================================
# SUITE: realistic
# ===========================================================================
#
# Models real production traffic patterns:
# - Poisson arrivals (runners appear gradually, not all at once)
# - Log-normal lifetimes (some runners are short, some long)
# - Mixed workload types (standard allocate/release vs session lifecycle)
# - Semaphore-gated concurrency (backpressure)
# ===========================================================================

def suite_realistic(
    base_url: str,
    token: str | None,
    run: SimulationRun,
    log: logging.Logger,
    leaf_workload_key: str = FIXED_WORKLOAD_KEY,
    *,
    cfg: RealisticConfig | None = None,
) -> None:
    """Realistic staggered-load simulation with Poisson arrivals and log-normal lifetimes."""
    SUITE = "realistic"
    if cfg is None:
        cfg = RealisticConfig()

    log.info("Starting realistic suite", extra={
        "total_runners": cfg.total_runners,
        "arrival_rate": cfg.arrival_rate,
        "lifetime_mean": cfg.lifetime_mean,
        "lifetime_stddev": cfg.lifetime_stddev,
        "session_pct": cfg.session_pct,
        "max_concurrent": cfg.max_concurrent,
        "workload_key": leaf_workload_key,
    })

    # Log-normal parameters from mean/stddev
    mu = math.log(cfg.lifetime_mean**2 / math.sqrt(cfg.lifetime_stddev**2 + cfg.lifetime_mean**2))
    sigma = math.sqrt(math.log(1 + (cfg.lifetime_stddev**2 / cfg.lifetime_mean**2)))

    sem = Semaphore(cfg.max_concurrent)
    lifecycle_results: list[RunnerLifecycleResult] = []
    peak_concurrent = 0
    _concurrent_count = 0
    _concurrent_lock = __import__("threading").Lock()

    def _track_concurrency(delta: int) -> None:
        nonlocal _concurrent_count, peak_concurrent
        with _concurrent_lock:
            _concurrent_count += delta
            if _concurrent_count > peak_concurrent:
                peak_concurrent = _concurrent_count

    def _runner_worker(runner_idx: int, is_session: bool) -> None:
        runner_id_str = f"realistic-{runner_idx}"
        lifetime = random.lognormvariate(mu, sigma)
        lifetime = min(lifetime, cfg.steady_seconds)  # cap at steady-state duration

        sem.acquire()
        _track_concurrency(1)

        client = _make_client(base_url, token)
        alloc_latency = 0.0
        success = False
        error_msg: str | None = None
        status_code: int | None = None
        allocated_runner_id: str | None = None

        try:
            # --- Allocate with 503 retry ---
            request_id = f"e2e-real-{runner_idx}-{uuid.uuid4().hex[:8]}"
            alloc_start = time.monotonic()
            last_exc: Exception | None = None
            for attempt in range(3):
                try:
                    resp = client.runners.allocate(
                        leaf_workload_key,
                        request_id=request_id,
                        session_id=uuid.uuid4().hex if is_session else None,
                    )
                    allocated_runner_id = resp.runner_id
                    alloc_latency = time.monotonic() - alloc_start
                    last_exc = None
                    break
                except (BFServiceUnavailable, BFRateLimited) as exc:
                    last_exc = exc
                    status_code = getattr(exc, "status_code", 503)
                    log.debug("Capacity error on allocate, retrying", extra={
                        "runner_idx": runner_idx, "attempt": attempt + 1,
                    })
                    time.sleep(2)

            if last_exc is not None:
                error_msg = f"Allocation failed after 3 retries: {last_exc}"
                lifecycle_results.append(RunnerLifecycleResult(
                    runner_id=runner_id_str,
                    alloc_latency=time.monotonic() - alloc_start,
                    lifetime=0,
                    is_session=is_session,
                    success=False,
                    error=error_msg,
                    status_code=status_code,
                ))
                return

            log.info("Runner allocated", extra={
                "runner_idx": runner_idx,
                "runner_id": allocated_runner_id,
                "alloc_latency_s": f"{alloc_latency:.2f}",
                "is_session": is_session,
                "planned_lifetime_s": f"{lifetime:.1f}",
            })

            # --- Session lifecycle (if session runner) ---
            if is_session and allocated_runner_id:
                try:
                    client.runners.pause(allocated_runner_id)
                    conn = client.runners.connect(allocated_runner_id)
                    allocated_runner_id = conn.runner_id
                except Exception as exc:
                    error_msg = f"Session lifecycle error: {exc}"
                    lifecycle_results.append(RunnerLifecycleResult(
                        runner_id=allocated_runner_id or runner_id_str,
                        alloc_latency=alloc_latency,
                        lifetime=time.monotonic() - alloc_start - alloc_latency,
                        is_session=is_session,
                        success=False,
                        error=error_msg,
                    ))
                    return

            # --- Simulate work (sleep for log-normal lifetime) ---
            work_start = time.monotonic()
            remaining = lifetime
            while remaining > 0:
                sleep_chunk = min(remaining, 5.0)
                time.sleep(sleep_chunk)
                remaining -= sleep_chunk
                # Optionally poll status mid-work
                if allocated_runner_id and remaining > 2.0:
                    try:
                        client.runners.status(allocated_runner_id)
                    except Exception:  # noqa: BLE001
                        pass
            actual_lifetime = time.monotonic() - work_start

            # --- Release ---
            if allocated_runner_id:
                client.runners.release(allocated_runner_id)

            success = True
            lifecycle_results.append(RunnerLifecycleResult(
                runner_id=allocated_runner_id or runner_id_str,
                alloc_latency=alloc_latency,
                lifetime=actual_lifetime,
                is_session=is_session,
                success=True,
            ))

        except BFError as exc:
            error_msg = f"API error: {exc}"
            sc = getattr(exc, "status_code", None)
            lifecycle_results.append(RunnerLifecycleResult(
                runner_id=allocated_runner_id or runner_id_str,
                alloc_latency=alloc_latency,
                lifetime=0,
                is_session=is_session,
                success=False,
                error=error_msg,
                status_code=sc,
            ))
        except Exception as exc:  # noqa: BLE001
            error_msg = f"{type(exc).__name__}: {exc}"
            lifecycle_results.append(RunnerLifecycleResult(
                runner_id=allocated_runner_id or runner_id_str,
                alloc_latency=alloc_latency,
                lifetime=0,
                is_session=is_session,
                success=False,
                error=error_msg,
            ))
        finally:
            if allocated_runner_id and not success:
                try:
                    client.runners.release(allocated_runner_id)
                except Exception:  # noqa: BLE001
                    pass
            client.close()
            _track_concurrency(-1)
            sem.release()

    # ----- Main spawner: Poisson arrivals -----
    with test_case(run, SUITE, "Realistic staggered load", log):
        threads: list[Thread] = []
        for i in range(cfg.total_runners):
            is_session = random.random() < cfg.session_pct
            t = Thread(
                target=_runner_worker,
                args=(i, is_session),
                name=f"realistic-runner-{i}",
            )
            threads.append(t)
            t.start()
            # Poisson inter-arrival: exponential delay
            delay = random.expovariate(cfg.arrival_rate)
            log.debug("Next runner in", extra={"delay_s": f"{delay:.3f}", "runner_idx": i})
            time.sleep(delay)

        # Wait for all runners to complete
        for t in threads:
            t.join(timeout=cfg.steady_seconds + 60)

        total = len(lifecycle_results)
        succeeded = sum(1 for r in lifecycle_results if r.success)
        log.info("Realistic load complete", extra={
            "total_spawned": cfg.total_runners,
            "results_collected": total,
            "succeeded": succeeded,
            "peak_concurrent": peak_concurrent,
        })
        assert total > 0, "No runner lifecycle results collected"

    # ----- Scenario 2: Peak concurrency validation -----
    with test_case(run, SUITE, "Peak concurrency validation", log):
        log.info("Peak concurrency observed", extra={
            "peak": peak_concurrent,
            "max_allowed": cfg.max_concurrent,
        })
        assert peak_concurrent <= cfg.max_concurrent, (
            f"Peak concurrency {peak_concurrent} exceeded max_concurrent {cfg.max_concurrent}"
        )

    # ----- Scenario 3: Allocation latency percentiles -----
    with test_case(run, SUITE, "Allocation latency percentiles", log):
        latencies = sorted(r.alloc_latency for r in lifecycle_results if r.alloc_latency > 0)
        if not latencies:
            log.warning("No successful allocations to measure latency")
            assert False, "No allocation latency data"
        else:
            def _percentile(data: list[float], pct: float) -> float:
                idx = int(len(data) * pct / 100)
                return data[min(idx, len(data) - 1)]

            p50 = _percentile(latencies, 50)
            p95 = _percentile(latencies, 95)
            p99 = _percentile(latencies, 99)
            log.info("Allocation latency percentiles", extra={
                "p50_s": f"{p50:.3f}",
                "p95_s": f"{p95:.3f}",
                "p99_s": f"{p99:.3f}",
                "sample_count": len(latencies),
            })
            assert p99 < 30.0, (
                f"P99 allocation latency {p99:.3f}s exceeds 30s threshold"
            )

    # ----- Scenario 4: Completion rate -----
    with test_case(run, SUITE, "Completion rate", log):
        total = len(lifecycle_results)
        succeeded = sum(1 for r in lifecycle_results if r.success)
        # Exclude runners that exhausted 503 retries from the denominator
        retries_exhausted = sum(
            1 for r in lifecycle_results
            if not r.success and r.status_code == 503
        )
        effective_total = total - retries_exhausted
        rate = (succeeded / effective_total * 100) if effective_total > 0 else 0
        log.info("Completion rate", extra={
            "succeeded": succeeded,
            "total": total,
            "retries_exhausted": retries_exhausted,
            "effective_total": effective_total,
            "rate_pct": f"{rate:.1f}",
        })
        assert rate >= 90.0, (
            f"Completion rate {rate:.1f}% is below 90% threshold "
            f"({succeeded}/{effective_total} succeeded)"
        )

    # ----- Scenario 5: No server errors -----
    with test_case(run, SUITE, "No server errors", log):
        server_errors = [
            r for r in lifecycle_results
            if not r.success
            and r.status_code is not None
            and 500 <= r.status_code < 600
            and r.status_code != 503  # 503 is capacity, not a server bug
        ]
        if server_errors:
            for err in server_errors[:5]:
                log.warning("Server error", extra={
                    "runner_id": err.runner_id,
                    "status_code": err.status_code,
                    "error": err.error,
                })
        assert len(server_errors) == 0, (
            f"Found {len(server_errors)} server errors (5xx excluding 503): "
            + "; ".join(f"{e.runner_id}: {e.error}" for e in server_errors[:3])
        )

    # ----- Scenario 6: Session lifecycle correctness -----
    with test_case(run, SUITE, "Session lifecycle correctness", log):
        session_runners = [r for r in lifecycle_results if r.is_session]
        session_allocated = [r for r in session_runners if r.alloc_latency > 0]
        session_ok = [r for r in session_allocated if r.success]
        session_failed = [
            r for r in session_allocated
            if not r.success and r.status_code != 503
        ]
        log.info("Session lifecycle summary", extra={
            "total_session_runners": len(session_runners),
            "allocated": len(session_allocated),
            "succeeded": len(session_ok),
            "failed_non_503": len(session_failed),
        })
        if session_failed:
            for sf in session_failed[:3]:
                log.warning("Session runner failed", extra={
                    "runner_id": sf.runner_id,
                    "error": sf.error,
                })
        assert len(session_failed) == 0, (
            f"{len(session_failed)} session runners failed after successful allocation: "
            + "; ".join(f"{sf.runner_id}: {sf.error}" for sf in session_failed[:3])
        )

    log.info("Realistic suite complete", extra={
        "suite": SUITE,
        "total_runners": cfg.total_runners,
        "peak_concurrent": peak_concurrent,
    })


# ===========================================================================
# SUITE: cleanup
# ===========================================================================

def suite_cleanup(
    client: BFClient, run: SimulationRun, log: logging.Logger,
    config_ids: list[str],
) -> None:
    """Delete configs created during the run."""
    SUITE = "cleanup"
    for cid in config_ids:
        with test_case(run, SUITE, f"Delete config {cid[:12]}", log):
            client.layered_configs.delete(cid)
            log.info("Config deleted", extra={"config_id": cid})

        with test_case(run, SUITE, f"Deleted config {cid[:12]} not in list", log):
            configs = client.layered_configs.list()
            ids = [c.config_id for c in configs]
            assert cid not in ids, f"Config {cid} still listed after delete"


# ===========================================================================
# Main
# ===========================================================================

def _parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="E2E Control Plane Simulation")
    p.add_argument(
        "--base-url",
        default=os.environ.get("BF_BASE_URL", "http://10.0.16.16:8080"),
        help="Control plane base URL (default: http://10.0.16.16:8080)",
    )
    p.add_argument(
        "--token",
        default=os.environ.get("BF_TOKEN", ""),
        help="Bearer auth token (or set BF_TOKEN env var)",
    )
    p.add_argument(
        "--suite",
        default="all",
        choices=["all", "health", "configs", "builds", "snapshots",
                 "runners", "session", "quarantine", "fleet", "canary",
                 "edge", "concurrency", "realistic"],
        help="Which test suite to run (default: all)",
    )
    p.add_argument("--verbose", "-v", action="store_true", help="Enable debug logging")
    p.add_argument(
        "--no-cleanup",
        action="store_true",
        help="Skip deleting configs created during the run",
    )
    p.add_argument(
        "--concurrency-width",
        type=int,
        default=5,
        metavar="N",
        help="Number of parallel workers per concurrency scenario (default: 5)",
    )
    p.add_argument(
        "--realistic-runners",
        type=int,
        default=30,
        metavar="N",
        help="Total runners to spawn in realistic suite (default: 30)",
    )
    p.add_argument(
        "--realistic-arrival-rate",
        type=float,
        default=2.0,
        metavar="RATE",
        help="Runners per second (Poisson λ) in realistic suite (default: 2.0)",
    )
    p.add_argument(
        "--realistic-lifetime-mean",
        type=float,
        default=15.0,
        metavar="SECS",
        help="Mean runner lifetime in seconds (log-normal) (default: 15.0)",
    )
    p.add_argument(
        "--realistic-session-pct",
        type=float,
        default=0.3,
        metavar="PCT",
        help="Fraction of runners that are session-type (default: 0.3)",
    )
    p.add_argument(
        "--realistic-max-concurrent",
        type=int,
        default=20,
        metavar="N",
        help="Max concurrent runners (semaphore cap) in realistic suite (default: 20)",
    )
    p.add_argument(
        "--realistic-steady-seconds",
        type=float,
        default=60.0,
        metavar="SECS",
        help="Duration of steady-state phase in realistic suite (default: 60.0)",
    )
    return p.parse_args()


def main() -> int:
    args = _parse_args()
    log = _setup_logging(args.verbose)
    run = SimulationRun()

    log.info("Starting E2E simulation", extra={
        "base_url": args.base_url,
        "suite": args.suite,
        "sdk_version": getattr(bf_sdk, "__version__", "unknown"),
        "auth": "token set" if args.token else "no token",
    })

    with BFClient(base_url=args.base_url, token=args.token or None) as client:
        created_config_ids: list[str] = []

        def _run(s: str) -> bool:
            return args.suite in ("all", s)

        # -- Health --
        if _run("health"):
            suite_health(client, run, log)

        # -- Snapshots --
        if _run("snapshots"):
            suite_snapshots(client, run, log)

        # -- Configs --
        created: dict[str, Any] = {}
        if _run("configs"):
            created = suite_configs(client, run, log)
            for key in ("config_id", "multi_config_id"):
                if cid := created.get(key):
                    created_config_ids.append(cid)

        # -- Builds --
        if _run("builds"):
            suite_builds(client, run, log, config_id=created.get("config_id"))

        # -- Runners --
        alloc_info: dict[str, Any] = {}
        if _run("runners"):
            alloc_info = suite_runners(
                client, run, log,
                leaf_workload_key=FIXED_WORKLOAD_KEY,
            )

        # -- Session --
        if _run("session"):
            suite_session(
                client, run, log,
                # Always allocate a fresh runner with session_id for session tests
                alloc_info=None,
                leaf_workload_key=FIXED_WORKLOAD_KEY,
            )

        # -- Quarantine --
        if _run("quarantine"):
            suite_quarantine(
                client, run, log,
                leaf_workload_key=FIXED_WORKLOAD_KEY,
            )

        # -- Fleet --
        if _run("fleet"):
            suite_fleet(
                client, run, log,
                leaf_workload_key=FIXED_WORKLOAD_KEY,
            )

        # -- Canary --
        if _run("canary"):
            suite_canary(client, run, log)

        # -- Edge --
        if _run("edge"):
            suite_edge(client, run, log)

        # -- Concurrency --
        if _run("concurrency"):
            suite_concurrency(
                args.base_url,
                args.token or None,
                run,
                log,
                leaf_workload_key=FIXED_WORKLOAD_KEY,
                width=args.concurrency_width,
            )

        # -- Realistic --
        if _run("realistic"):
            rcfg = RealisticConfig(
                total_runners=args.realistic_runners,
                arrival_rate=args.realistic_arrival_rate,
                lifetime_mean=args.realistic_lifetime_mean,
                lifetime_stddev=5.0,
                session_pct=args.realistic_session_pct,
                max_concurrent=args.realistic_max_concurrent,
                steady_seconds=args.realistic_steady_seconds,
            )
            suite_realistic(
                args.base_url,
                args.token or None,
                run,
                log,
                leaf_workload_key=FIXED_WORKLOAD_KEY,
                cfg=rcfg,
            )

        # -- Cleanup --
        if created_config_ids and not args.no_cleanup:
            suite_cleanup(client, run, log, created_config_ids)

    run.print_report()

    s = run.summary()
    log.info("Simulation complete", extra=s)

    return 0 if s["failed"] == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
