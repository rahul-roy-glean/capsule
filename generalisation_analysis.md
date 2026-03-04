# Generalisation Analysis: Removing GitHub/Bazel Deep Wiring

## Premise

The platform was built as a GitHub Actions + Bazel CI runner. Over time, generic
abstractions emerged (`DriveSpec`, `SnapshotCommand`, `ExtensionDrive`,
`StartCommand`, MMDS metadata) that are capable of expressing everything the
named special cases do. This document audits every remaining named construct and
asks: can it be deleted in favour of the generic machinery that already exists?

---

## The Generic Machinery (what already exists)

### DriveSpec (snapshot-builder time)
```go
type DriveSpec struct {
    DriveID   string            `json:"drive_id"`
    Label     string            `json:"label"`
    SizeGB    int               `json:"size_gb"`
    ReadOnly  bool              `json:"read_only"`
    Commands  []SnapshotCommand `json:"commands,omitempty"`
    MountPath string            `json:"mount_path,omitempty"`
}
```
Declared in `LayerDef.Drives`, drives are created by snapshot-builder, chunked
into `ExtensionDrive` entries in the manifest, downloaded by the host, and
attached to the VM via `buildDrives(extensionPaths)`. The full lifecycle is
already generic.

### ExtensionDrive (runtime chunked restore)
```go
type ExtensionDrive struct {
    Chunks    []ChunkRef `json:"chunks"`
    ReadOnly  bool       `json:"read_only"`
    SizeBytes int64      `json:"size_bytes"`
}
```
Stored in `ChunkedSnapshotMetadata.ExtensionDrives`, keyed by `DriveID`. The
manager resolves them to local paths via `snapshotPaths.ExtensionDriveImages` and
passes them to `buildDrives`. Completely generic — no knowledge of what the
drives contain.

### buildDrives (VM assembly)
```go
func (m *Manager) buildDrives(extensionDrivePaths map[string]string) []firecracker.Drive
```
Takes `map[string]string` (driveID → host path), sorts by ID, emits Firecracker
drive configs. Already handles arbitrary extension drives with no special cases.
The only hardcoded entry is the `credentials` drive (always first).

### SnapshotCommand (warmup/init)
```go
type SnapshotCommand struct {
    Type      string   `json:"type"`      // "shell", "git-clone", "exec", "gcp-auth", ...
    Args      []string `json:"args"`
    RunAsRoot bool     `json:"run_as_root,omitempty"`
}
```
Dispatched by thaw-agent's `dispatchCommand()` in warmup mode. Generic
typed-command array that replaces the old `BazelVersion`/`WarmupTargets` fields.

### StartCommand (user service)
```go
type StartCommand struct {
    Command    []string          `json:"command"`
    Port       int               `json:"port"`
    HealthPath string            `json:"health_path"`
    Env        map[string]string `json:"env,omitempty"`
    RunAs      string            `json:"run_as,omitempty"`
}
```
Runs a long-lived process inside the VM with health checking. Already generic.

### MMDS (host → guest metadata)
Free-form JSON injected by the host, read by thaw-agent. Can carry arbitrary
`map[string]string` metadata without schema changes.

---

## Construct-by-construct analysis

### 1. `repo_cache_upper` — writable per-runner cache overlay

**What it does:** Creates a per-runner ext4 image at allocation time, injected
into `extensionPaths["repo_cache_upper"]` and attached to the VM. Thaw-agent
mounts it as the upper layer of an overlayfs over the shared read-only seed.

**Question asked:** "Why should all ExtensionDrives follow this model? Isn't the
overlay mount special?"

**Answer:** No, not all extension drives need overlayfs. But overlayfs is not
special to this construct — it's just what this particular drive *does* with its
mount. The question is whether the *creation and attachment* of the drive should
be special. It shouldn't.

Today `repo_cache_upper` is created by a hardcoded block in `AllocateRunner`:
```go
if m.config.Bazel.RepoCacheUpperSizeGB > 0 {
    repoCacheUpperPath = filepath.Join(...)
    createExt4Image(repoCacheUpperPath, m.config.Bazel.RepoCacheUpperSizeGB, "ARTIFACT_CACHE_UPPER")
}
extensionPaths["repo_cache_upper"] = repoCacheUpperPath
```

This is exactly what `DriveSpec` models. The drive could be declared as:
```yaml
drives:
  - drive_id: repo_cache_upper
    label: ARTIFACT_CACHE_UPPER
    size_gb: 10
    read_only: false
```

The overlay mount in thaw-agent (`setupRepoCacheOverlay`) is just application
logic — it reads the labels, mounts the devices, and creates an overlayfs. This
doesn't need framework support. Thaw-agent already dispatches arbitrary
`SnapshotCommand` arrays; the overlay setup could be an `init_command` of type
`shell` that runs `mount -t overlay ...`. Or thaw-agent could simply auto-mount
every labelled drive at its `MountPath` (if present in MMDS metadata) and let
the application layer do the overlay on top.

The genuinely special part — that this drive is per-runner and writable while the
seed is per-snapshot and read-only — is already modelled by `DriveSpec.ReadOnly`.
The seed is a snapshot-time `DriveSpec` with `ReadOnly: true`; the upper is a
per-runner `DriveSpec` with `ReadOnly: false`. The only missing piece is telling
the manager "create this drive fresh per-allocation instead of using the snapshot
copy." This could be a `PerRunner: true` field on `DriveSpec`, or simply inferred
from `ReadOnly: false` (writable drives must be per-runner to avoid cross-VM
contamination).

**Verdict:** Replaceable by `DriveSpec`. The overlay mount is application logic,
not framework concern.

---

### 2. `credentials` drive — host-seeded read-only secrets

**What it does:** `ensureCredentialsImage` builds an ext4 image on host startup,
optionally seeded from a directory (`BuildbarnCertsDir`). Attached as the first
drive to every VM. Thaw-agent mounts it and sets up symlinks for `.netrc`,
`.git-credentials`, CA certs, etc.

**Question asked:** "So this can be deleted?"

**Answer:** Yes, the entire `ensureCredentialsImage` codepath can be deleted.
Here's why:

The `credentials` drive is a snapshot-builder concern, not a per-boot concern.
Today `ensureCredentialsImage` rebuilds the image every time the host starts,
from the host's `/etc/glean/ci/certs` directory. But the correct place to do
this is snapshot-builder, using the `DriveSpec.Commands` mechanism:

```yaml
drives:
  - drive_id: credentials
    label: CREDENTIALS
    size_gb: 1
    read_only: true
    mount_path: /mnt/credentials
    commands:
      - type: shell
        args: ["cp -r /host-certs/* /mnt/credentials/"]
        run_as_root: true
```

The credentials get baked into the snapshot. The manager doesn't need to know
about credentials at all — it just restores the snapshot and all drives are
already populated.

For secrets that can't be baked (runtime tokens, per-job credentials), those
already flow through MMDS (`Job.GitToken`, `Job.GCPAccessToken`) and don't need
a drive.

The symlink setup in thaw-agent (`setupCredentialSymlinks`) is the only part with
real logic. It could be a `SnapshotCommand` of type `shell` in the layer's
`init_commands`, or thaw-agent could generically auto-mount all `DriveSpec`
drives by label at their declared `MountPath`. The symlinks for `.netrc`,
`.git-credentials`, and CA cert installation are application-layer concerns that
belong in the `LayeredConfig`'s `init_commands`, not hardcoded in thaw-agent.

The one subtlety: credentials that change between snapshots (e.g., rotated TLS
certs) need to be refreshable. This is what `LayerDef.RefreshCommands` +
`RefreshInterval` handles — the layer gets rebuilt when secrets rotate.

**Verdict:** `ensureCredentialsImage` can be deleted. The credentials drive
becomes a `DriveSpec` in the `LayeredConfig`. The hardcoded `credentials` entry
in `buildDrives` should become just another extension drive.

---

### 3. `git-cache` drive — pre-fetched git mirrors

**What it does:** A read-only ext4 image with git mirrors of target repos.
Created by the snapshot-builder, baked into the snapshot state file. Manager
populates MMDS `GitCache` struct (5 fields). Thaw-agent mounts it, then uses
`git clone --reference` to set up the workspace.

**Components:**
- **Host side:** `BazelConfig` has 7 git-cache fields. Manager stores
  `gitCacheImage` field. `buildMMDSData` populates `Latest.GitCache.*`.
- **Guest side:** `mountGitCache()` mounts the drive. `setupWorkspaceFromGitCache()`
  does `git clone --reference`. `findGitCacheReference()` looks up cache path.
  3 dedicated flags.

**Question asked:** "Think deeply about the need for thaw-agent mount logic?"

**Answer:** There is no need for thaw-agent-specific mount logic. Here's the
reasoning:

The git-cache drive is a `DriveSpec` with `ReadOnly: true` and
`MountPath: /mnt/git-cache`. It is already created by snapshot-builder as an
extension drive and baked into the snapshot. The manager doesn't create it at
runtime — it's already attached via `snapshotPaths.ExtensionDriveImages`.

Thaw-agent's `mountGitCache()` is:
```go
func mountGitCache(data *MMDSData) error {
    mountPath := *gitCacheMount
    dev := resolveDevice(*gitCacheDevice, *gitCacheLabel)
    exec.Command("mount", "-o", "ro", dev, mountPath)
}
```

This is generic drive mounting. Every extension drive that has a `MountPath`
needs the same operation: find the device by label, mount it at the path. If
thaw-agent had a generic "mount all extension drives by label" phase, this
function would not exist. And the `DriveSpec` already carries both `Label` and
`MountPath` — the metadata is there, thaw-agent just doesn't use it generically.

The `setupWorkspaceFromGitCache()` function is a `git clone --reference` — this
is genuinely useful application logic, but it doesn't need to be a hardcoded boot
phase. It could be a `SnapshotCommand` of type `shell` or `git-clone` in the
`LayerDef.InitCommands`:
```yaml
init_commands:
  - type: shell
    args: ["git clone --reference /mnt/git-cache/scio --dissociate --no-checkout file:///mnt/git-cache/scio /workspace/scio/scio"]
```

Or even better: since this runs during warmup (snapshot-building), it's already
expressed as warmup commands. At runtime (post-restore), the workspace is already
set up inside the snapshot. The only runtime work is the symlink creation for
`PreClonedPath`, which is 5 lines of code that could be driven by a generic MMDS
metadata field.

**MMDS metadata:** The 5-field `GitCache` struct in MMDS exists because
thaw-agent needs to know where things are mounted. But this is what `DriveSpec`
already declares. If MMDS carried a list of `DriveSpec`s (or a
`map[string]string` of drive metadata), thaw-agent could generically mount drives
and the git-cache struct would be unnecessary.

**Verdict:** The git-cache drive, all 7 `BazelConfig` fields, the MMDS `GitCache`
struct, `mountGitCache()`, and the dedicated flags can all be deleted. The drive
is a `DriveSpec`. The mount is generic. The workspace setup is a warmup command.

---

### 4. Buildbarn certs — already replaced, dead code

**Status:** `mountBuildbarnCerts()` in thaw-agent is dead code. `mountCredentials()`
already handles the same drive with a backwards-compat symlink. Two parallel
codepaths exist (`!*skipBuildbarnCerts` and `!*skipCredentials`).

**Verdict:** Delete `mountBuildbarnCerts()`, `skipBuildbarnCerts` flag,
`buildbarnCertsDevice`/`buildbarnCertsMount`/`buildbarnCertsLabel` flags. The
`mountCredentials` path (which itself should eventually become generic drive
mounting) already handles Buildbarn certs via the `certs/buildbarn` directory
within the credentials drive.

---

### 5. `WarmupConfig.BazelVersion` / `WarmupTargets` — already replaced

**Status:** The modern warmup path uses `Latest.Warmup.Commands
[]SnapshotCommand`, which is generic. The old `WarmupConfig` struct with
`BazelVersion` and `WarmupTargets` is only used when `Commands` is empty (legacy
fallback).

**Verdict:** Delete. Legacy snapshots using the old format are no longer in
production.

---

### 6. `SnapshotMetrics.CacheHitRatio` canary logic — defer

Hardwired `< 0.5 = unhealthy` threshold for Bazel cache hit ratio in canary
rollout code. Needs to become a generic `health_score` or pluggable health check,
but this is a separate concern from the structural cleanup.

**Verdict:** Defer to a later pass. Not blocking.

---

### 7. `CIConfig` / `IntegrationName` / `ci.Adapter`

**Question asked:** "Does IntegrationName itself need to exist? What are other
examples where it can be a generic abstraction?"

**Deep analysis:**

`IntegrationName` currently does exactly three things:

1. **Thaw-agent boot:** `switch ciSystem { case "github-actions": registerGitHubRunner() }`
2. **Manager MMDS:** `data.Latest.Runner.IntegrationName = m.ciAdapter.Name()`
3. **Control-plane routing:** `if ciSystemEnv == "github-actions" { ... }`

Let's examine what the alternatives look like:

**What if there was no IntegrationName at all?**

Runner registration would be driven by the presence of credentials and commands,
not by a named integration. Consider:

- If `CIRunnerToken` is non-empty in MMDS → a registration script runs.
  Which script? It's already baked into the snapshot. The thaw-agent doesn't
  decide "I'm a GitHub runner" — the snapshot was *built* as a GitHub runner
  image with the runner binary installed. The registration command is just
  another `init_command` or `start_command`:

```yaml
start_command:
  command: ["/home/runner/config.sh", "--url", "https://github.com/org", "--token", "${CI_RUNNER_TOKEN}", "--ephemeral"]
  env:
    CI_RUNNER_TOKEN: "${ci_runner_token}"  # injected from MMDS
```

- Drain (label removal) is a GitHub API call. This doesn't need an abstraction —
  it needs the GitHub adapter to exist as a library that the control-plane calls
  directly. The `ci.Adapter` interface wrapping this is premature: there is
  exactly one implementation that does work (`github`), and one that does nothing
  (`noop`). An interface with one real implementation is just indirection.

- Webhook handling is HTTP routing. The control-plane mounts `POST /webhook/github`
  → `GitHubWebhookHandler`. This is direct, not adapter-mediated.

**Other integrations that could exist (but don't):**

- **GitLab CI:** Runner registration is `gitlab-runner register`. Same pattern:
  a token in MMDS, a binary in the snapshot, a `start_command` that runs
  registration.
- **Buildkite:** Agent registration via `buildkite-agent start`. Token in MMDS,
  binary in snapshot.
- **Jenkins:** Agent JNLP connection. Token + master URL in MMDS, agent.jar in
  snapshot.

In every case, the pattern is:
1. A CI-specific binary is installed during snapshot building (`init_commands`)
2. Runtime credentials are passed via MMDS metadata
3. Registration runs as a `start_command` or `init_command`

None of these need `IntegrationName`. The snapshot knows what it is because it
was built that way. The host doesn't need to know — it just passes credentials
through MMDS and attaches drives.

**What `ci.Adapter` actually does that matters:**

Looking at the 8 methods:
- `Name()` — returns a string. Used to set `IntegrationName` in MMDS. If we
  delete `IntegrationName`, this is unused.
- `GetRunnerToken()` — GitHub API call to get ephemeral registration token.
  This is the one genuinely useful method. But it's GitHub-specific: it calls
  `GetOrgRunnerRegistrationToken` or `GetRunnerRegistrationToken`. No other CI
  system has this API shape.
- `RunnerURL()` — returns `https://github.com/{org}`. Used to set `Job.Repo` in
  MMDS when the host auto-fetches tokens. Purely GitHub-specific.
- `OnDrain()` — removes labels from GitHub runners via API. GitHub-specific.
- `OnRelease()` — no-op for all implementations.
- `WebhookHandler()/WebhookPath()` — returns the GitHub webhook handler.
  GitHub-specific.
- `RepoFromCommands()` — parses `git clone` URLs to extract `org/repo`.
  GitHub-specific URL format.
- `Close()` — no-op.

**Every method that does real work is GitHub-specific.** The interface doesn't
abstract over CI systems — it abstracts over "GitHub" and "nothing." The
`NoopAdapter` exists only to satisfy the interface. This is the hallmark of a
premature abstraction.

**Recommendation:**

Delete `ci.Adapter` interface. Replace with:
- `*cigithub.Client` (nilable) on the manager and control-plane, for the three
  things that actually need GitHub API access: token fetch, drain, webhook.
- No `IntegrationName`. The snapshot knows what it is. Thaw-agent reads MMDS
  credentials and runs `start_command`. If a registration step is needed, it's
  expressed as a `start_command` or `init_command`.
- `CIConfig` struct in `HostConfig` → delete entirely. The GitHub-specific fields
  (`AppID`, `AppSecret`, `Repo`, `Org`, `Labels`, `Ephemeral`) belong in the
  `cigithub.Config` struct, which the control-plane and firecracker-manager
  construct locally from flags/env.
- `BazelConfig` struct → delete entirely (see below).

---

### 8. `BazelConfig` struct

```go
type BazelConfig struct {
    RepoCacheUpperSizeGB      int
    BuildbarnCertsDir         string
    BuildbarnCertsMountPath   string
    BuildbarnCertsImageSizeMB int
    GitCacheEnabled           bool
    GitCacheDir               string
    GitCacheImagePath         string
    GitCacheMountPath         string
    GitCacheRepoMappings      map[string]string
    GitCacheWorkspaceDir      string
    GitCachePreClonedPath     string
}
```

This struct bundles three unrelated concerns:

1. **`RepoCacheUpperSizeGB`** — Size of per-runner writable cache drive.
   Replaceable by `DriveSpec.SizeGB` on a drive with `ReadOnly: false`.

2. **`BuildbarnCerts*`** (4 fields) — Credentials drive configuration.
   Replaceable by `DriveSpec` + `LayeredConfig` credentials layer.
   `ensureCredentialsImage` → delete (snapshot-builder handles it).

3. **`GitCache*`** (7 fields) — Git mirror drive configuration.
   Replaceable by `DriveSpec` in `LayeredConfig`. Mount is generic.
   Workspace setup is a warmup `SnapshotCommand`.

**Verdict:** Delete `BazelConfig` entirely. Every field maps to an existing
generic abstraction. The 8 git-cache flags in `firecracker-manager/main.go` and
the 7 `BazelConfig` fields in `HostConfig` can all go away.

---

## Thaw-agent boot sequence: what remains after generalisation

Today's thaw-agent boot sequence has these hardcoded phases:

```
1. Wait for MMDS
2. setupRepoCacheOverlay()         ← special case
3. mountCredentials()              ← special case
4. mountBuildbarnCerts() [legacy]  ← dead code
5. mountGitCache()                 ← special case
6. configureNetwork()              ← genuinely needed (kernel-level)
7. regenerateHostname()            ← genuinely needed
8. resyncClock()                   ← genuinely needed
9. setupWorkspaceFromGitCache()    ← special case
10. runWarmupMode() [if warmup]    ← generic (dispatches SnapshotCommand[])
11. startHealthServer()            ← genuinely needed
12. runStartCommand()              ← generic
13. CI registration switch         ← special case
14. signalReady()                  ← genuinely needed
```

After generalisation, the boot sequence becomes:

```
1. Wait for MMDS
2. mountExtensionDrives()          ← generic: mount all labelled drives
3. configureNetwork()
4. regenerateHostname()
5. resyncClock()
6. runWarmupMode() [if warmup]     ← dispatches SnapshotCommand[]
7. startHealthServer()
8. runStartCommand()               ← handles CI registration too
9. signalReady()
```

Steps 2-4 of the old sequence (repo cache overlay, credentials, buildbarn certs,
git-cache) collapse into one generic step: "mount every extension drive at its
declared MountPath." The overlay (step 2) is application logic expressed as a
`SnapshotCommand`. The workspace setup (step 9) is a warmup command baked into
the snapshot. CI registration (step 13) is a `start_command`.

The only new thing thaw-agent needs is: **read `DriveSpec` metadata from MMDS
and auto-mount drives by label.** This is ~20 lines of code replacing ~200 lines
of special-case mount functions.

---

## What gets deleted

### Structs/types:
- `BazelConfig` (11 fields) — `pkg/runner/types.go`
- `CIConfig` (9 fields) — `pkg/runner/types.go`
- `MMDSData.Latest.GitCache` struct (5 fields) — `pkg/runner/types.go` + `cmd/thaw-agent/main.go`
- `MMDSData.Latest.Buildbarn` struct — `pkg/runner/types.go` + `cmd/thaw-agent/main.go`
- `ci.Adapter` interface — `pkg/ci/adapter.go`
- `ci.NoopAdapter` — `pkg/ci/noop.go`

### Functions:
- `ensureCredentialsImage()` — `pkg/runner/manager.go`
- `createExt4ImageMB()` — `pkg/runner/manager.go`
- `seedExt4ImageFromDir()` — `pkg/runner/manager.go`
- `mountGitCache()` — `cmd/thaw-agent/main.go`
- `setupWorkspaceFromGitCache()` — `cmd/thaw-agent/main.go`
- `findGitCacheReference()` — `cmd/thaw-agent/main.go`
- `setupGitAlternates()` — `cmd/thaw-agent/main.go`
- `mountBuildbarnCerts()` — `cmd/thaw-agent/main.go`
- `setupRepoCacheOverlay()` — becomes generic or a SnapshotCommand

### Fields on Manager:
- `credentialsImage string` — becomes an extension drive like any other
- `gitCacheImage string` — already baked into snapshot, field is unused for
  drive attachment
- `ciAdapter ci.Adapter` — replaced by nilable `*cigithub.Client`

### Flags (firecracker-manager):
- 8 `--git-cache-*` flags
- 3 `--buildbarn-certs-*` flags
- `--repo-cache-upper-size-gb` (moves to DriveSpec in LayeredConfig)
- `--ci-system` (deleted — no IntegrationName)

### Flags (thaw-agent):
- `--skip-git-cache`, `--git-cache-device`, `--git-cache-mount`, `--git-cache-label`
- `--skip-buildbarn-certs`, `--buildbarn-certs-device`, `--buildbarn-certs-mount`,
  `--buildbarn-certs-label`
- `--skip-artifact-cache`, `--artifact-cache-seed-mount`, `--artifact-cache-upper-mount`,
  `--artifact-cache-overlay-target`
- All deprecated aliases in `init()`

Replaced by: thaw-agent reads drive metadata from MMDS and auto-mounts. No
per-drive flags needed.

---

## What survives

1. **`cigithub` package** — The GitHub adapter continues to exist as a library
   for token fetch, drain label removal, and webhook handling. It's just no
   longer behind an interface.

2. **`DriveSpec` / `ExtensionDrive`** — The generic drive machinery. Already
   exists, just needs wider adoption.

3. **`SnapshotCommand`** — The generic command dispatch. Already exists and
   handles warmup. Needs to also handle runtime init (currently hardcoded boot
   phases).

4. **`StartCommand`** — User service lifecycle. Already generic. Can absorb
   CI runner registration.

5. **`LayeredConfig`** — The declarative snapshot configuration. Already
   supports `Drives`, `InitCommands`, `RefreshCommands`. This is where the
   credentials drive, git-cache drive, and cache overlay configuration belong.

6. **Network, clock, hostname** — Genuine kernel/OS-level concerns that
   thaw-agent must handle. These are not CI or Bazel constructs.

---

## Summary

The platform already has all the generic abstractions it needs. The named
special cases (`BazelConfig`, `CIConfig`, `ci.Adapter`, `credentials` drive,
`git-cache` drive, `repo_cache_upper`, `mountBuildbarnCerts`, etc.) are legacy
scaffolding from before the generic machinery existed. Every one of them maps
cleanly onto `DriveSpec` + `SnapshotCommand` + `StartCommand` + MMDS metadata.

The `ci.Adapter` interface is a premature abstraction with exactly one real
implementation. `IntegrationName` is unnecessary — the snapshot knows what it
is because it was built that way. Delete both.

The path forward is not more renaming (`github_runner_id` → `external_runner_id`,
`CISystem` → `IntegrationName`). Renaming preserves the structural coupling; it
just changes the labels. The path forward is deletion: remove the named special
cases and let the generic machinery do the work it was designed to do.
