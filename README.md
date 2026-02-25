<p align="center">
  <img src="assets/logo.png" width="220" alt="bazel-firecracker logo" />
</p>

# bazel-firecracker

Self-hosted GitHub Actions runners on Firecracker microVMs, built for Bazel.

## The problem

Bazel CI is slow not because builds are slow — it's because every runner starts cold. Each job re-downloads externals, recomputes the analysis graph, and waits for the Bazel server JVM to warm up. On a shared GCE MIG this compounds: many small VMs, low utilization, cold caches every time.

## The approach

Snapshot a fully warmed Bazel environment — analysis graph, fetched externals, live Bazel server — and restore it in under a second using Firecracker's snapshot/restore on local NVMe. Each CI job gets a fresh microVM from that snapshot, pre-warmed, isolated, ready in ~100–500ms.

GCE hosts run multiple microVMs (up to 16/host), autoscaled as a MIG. A lightweight control plane on GKE handles scheduling, snapshot version rollouts, and the GitHub webhook integration. Hosts pull new snapshot versions automatically and roll them out with no downtime.

**What this gives you:**

- Sub-second runner startup (vs. 30–120s for a cold GCE VM)
- Bazel analysis graph and server state preserved across jobs
- Strong isolation: dedicated microVM per job, not shared containers
- Multi-repo support: per-repo snapshots, routed by label
- Canary rollouts for new snapshot versions
- Fast host provisioning via GCP disk snapshots (~30s vs. minutes pulling from GCS)

## Getting started

```yaml
# Use it in your workflow — no other changes needed
jobs:
  build:
    runs-on: [self-hosted, firecracker]
    steps:
      - uses: actions/checkout@v4
      - run: bazel build //...
```

For deployment: see [docs/DEV_SETUP.md](docs/DEV_SETUP.md) to set up a development environment, [docs/setup.md](docs/setup.md) for initial deployment, and [PRODUCTION_ROLLOUT.md](PRODUCTION_ROLLOUT.md) for a full production rollout guide.

The `onboard` tool automates end-to-end infrastructure setup:

```bash
cp onboard.yaml my-config.yaml
# edit my-config.yaml
make onboard CONFIG=my-config.yaml
```

## Development

```bash
make dev-setup   # install toolchain deps
make build       # build all binaries
make test-unit   # unit tests (macOS + Linux)
make check       # build + unit tests (pre-commit)
make lint
```

See `make help` for the full list of targets. Integration tests require Linux + KVM (`make test-integration`).

## Docs

- [docs/architecture.md](docs/architecture.md) — system design and component breakdown
- [docs/DEV_SETUP.md](docs/DEV_SETUP.md) — local development setup
- [docs/HOWTO.md](docs/HOWTO.md) — operational guides
- [docs/operations.md](docs/operations.md) — day-to-day ops and runbooks
- [bazel-firecracker-rfc.md](bazel-firecracker-rfc.md) — original design RFC

## License

Apache 2.0
