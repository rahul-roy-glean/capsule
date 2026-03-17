# Capsule Secure Access Plane: OpenShell Comparison and Future Direction

This document captures a strategy-level synthesis of two external artifacts and
connects them to Capsule's architecture:

- Capsule secure access plane proposal:
  <https://github.com/rahul-roy-glean/caspsule-access-plane/blob/main/secure-access-plane.md>
- NVIDIA OpenShell docs:
  <https://github.com/NVIDIA/OpenShell/tree/main/docs>

The goal is not to restate those sources line by line. The goal is to explain
what they imply for Capsule and to identify the right long-term design
direction.

## Executive Summary

The Capsule secure access plane proposal is fundamentally about **where
authority should live** in an agent system.

Its central claim is that:

- the sandbox is compute
- the access plane is authority

That is a stronger position than a traditional sandboxing model. It means:

- long-lived sessions are acceptable
- durable authority inside long-lived sessions is not
- privileged access should be granted explicitly, narrowly, and temporarily
- CLI-heavy and SDK-heavy workflows must be first-class, not treated as edge
  cases behind an HTTP-only abstraction

NVIDIA OpenShell, as documented today, is a strong secure runtime with:

- kernel-backed sandbox isolation
- declarative filesystem, process, and network policy
- gateway-managed credentials
- privacy-aware inference routing

OpenShell is strong at enforcing runtime boundaries. The Capsule proposal goes
one layer higher and asks a different question:

> How should privileged access be modeled for long-lived, partially untrusted
> agent sessions so identity, scope, audit, and revocation remain correct?

That distinction matters. OpenShell is best understood as a powerful substrate
for safe agent execution. Capsule's future secure access plane should use a
runtime substrate like that, but it should not stop there.

The future Capsule architecture should be:

- a VM or sandbox substrate that enforces local runtime constraints
- a separate secure access plane that owns identity, grants, approvals, remote
  execution, helper sessions, and audit
- remote broker execution as the default for privileged access
- direct HTTP grants and local helper sessions as explicit exceptions rather
  than the ambient default

## Why This Problem Exists

Capsule is optimized for warm, reusable runtime state. That is valuable for:

- developer sandboxes
- agent sessions
- CI and debugging workflows
- long-lived stateful tools

But once sessions become long-lived, access control gets harder. The platform
must avoid a common failure mode:

- the session persists
- the credential persists with it
- privilege becomes ambient

That is the core risk model behind the secure access plane proposal.

The proposal is correct to assume:

- model-directed code is not a trust boundary
- prompt injection is plausible
- generated code may read files, spawn subprocesses, and probe local surfaces
- reusable long-lived credentials inside a sandbox are high-risk

This becomes especially important for real-world workflows that rely on:

- `kubectl`
- `gcloud`
- `gh`
- provider SDKs
- metadata-based auth
- exec plugins
- credential helpers
- token caches

Those workflows do not fit neatly into a single HTTP proxy abstraction.

## What the Capsule Secure Access Plane Proposal Gets Right

The proposal in `secure-access-plane.md` is strong because it frames the
problem as authority placement rather than network plumbing.

### 1. Sandbox is compute, not authority

This is the most important design principle in the proposal.

The sandbox should execute work, preserve local state, and provide isolation.
It should not be the place where durable authority lives.

That means long-lived credentials, refresh tokens, and durable provider auth
state should stay outside the sandbox by default.

### 2. One semantic authority, multiple delivery lanes

The proposal distinguishes between:

- semantic authority: identity, policy, scope, audit, approvals
- delivery lane: how access is materialized at execution time

This is the right abstraction. It prevents transport mechanics from becoming
the policy model.

### 3. Broker execution should be the default for privileged access

The proposal's default lane is remote broker execution. This is the strongest
general answer for:

- provider CLIs
- write operations
- internal debug/admin tools
- new tool families not yet modeled safely in the sandbox

This is the correct default because it:

- keeps durable credentials out of the sandbox
- centralizes policy and audit
- works for HTTP, CLI, and non-HTTP tools
- scales better than custom auth logic embedded in each guest runtime

### 4. Direct HTTP and local helper access are explicit exceptions

The proposal does not ban direct in-sandbox access. It says that:

- direct HTTP grants are useful when in-sandbox HTTP fidelity matters
- local helper sessions are useful when local CLI fidelity matters

But both should be:

- short-lived
- narrowly scoped
- policy-gated
- revocable

That is the correct risk posture.

### 5. Long-lived sessions and short-lived grants must be decoupled

This is one of the most important operational implications.

Capsule sessions can last hours or days. Access grants should not.

The proposal's idea of binding grants to:

- `session_id`
- `runner_id`
- ideally `turn_id`
- target
- capability scope
- TTL

is exactly the right direction.

### 6. The trust model is realistic

The proposal distinguishes four trust layers:

- control plane and access plane
- harness
- generated code and model-directed execution
- external systems

The subtle but important point is that harness trust depends on placement. If
the harness shares local surfaces with generated code, then generated code can
often reach what the harness can reach. The document calls this out explicitly,
and that is critical for any helper-session design.

## Why a Pure Proxy Is Not Enough

The proposal is especially strong in its argument against a pure network-proxy
model.

A proxy is useful for:

- host allowlists
- port allowlists
- header transforms
- method and path filtering
- logging

But it does not fully solve:

- arbitrary CLI authentication
- provider SDK local auth chains
- metadata and helper-based flows
- non-HTTP tools
- session-aware or identity-aware grant semantics

For Capsule, that means a proxy can be part of the system, but not the entire
system.

The secure access plane needs to be more than a proxy. It needs to be:

- a broker
- a policy engine
- an executor
- an approval surface
- an audit authority

## The Lane Model in the Proposal

The proposal's four-lane model is the right organizing framework.

### Lane 0: Local Uncredentialed Compute

This covers:

- build and test tools
- local code execution
- file inspection
- ordinary sandbox work with no privileged external auth

This is the baseline sandbox value proposition.

### Lane 1: Remote Broker Execution

This is the default for privileged access. The access plane executes HTTP or
CLI actions on trusted compute and streams results back.

This lane is the best default for:

- provider CLIs
- write operations
- high-risk production actions
- internal admin surfaces
- tool families that do not yet have a safe in-sandbox model

### Lane 2: Direct HTTP Grants

This preserves in-sandbox HTTP behavior while keeping durable credentials out
of the guest.

It is useful for:

- REST or GraphQL calls from code running inside the sandbox
- narrow direct API access where local fidelity matters
- bounded reads, then selected writes as policy matures

### Lane 3: Local Auth Helper Sessions

This exists for selected CLI and SDK families where local execution fidelity is
important enough to justify the complexity.

Examples include:

- Kubernetes exec credential plugins
- Google workload identity federation and executable-sourced ADC
- AWS `credential_process`
- Git or GitHub credential helpers

This lane is powerful, but it is also the riskiest. If arbitrary generated code
can use the same helper surface as the harness, then the sandbox effectively
has authority for the helper lifetime.

That is why the proposal is right to say Lane 3 must not be the ambient
default.

## What NVIDIA OpenShell Actually Provides

OpenShell is a secure runtime for agent sandboxes. The docs describe a clear
four-part system:

- gateway
- sandbox
- policy engine
- privacy router

In practice, OpenShell is best understood as an execution substrate with strong
runtime controls.

### Gateway

The gateway is OpenShell's control plane. It:

- provisions sandboxes
- stores provider credentials
- distributes policies
- manages inference configuration
- acts as the CLI auth boundary

The gateway can run:

- locally
- on a remote host over SSH
- behind a reverse proxy in cloud mode

### Sandbox

The sandbox is the execution environment where the agent runs. It includes:

- the agent process
- the local proxy path for egress decisions
- runtime supervision
- policy enforcement hooks

### Policy Engine

OpenShell's policy system enforces:

- filesystem policy
- process policy
- network policy

The docs distinguish:

- static policy sections applied at sandbox creation time
- dynamic network policies that can hot-reload without restarting the sandbox

That hot-reloadability is practically important.

### Privacy Router

OpenShell treats `https://inference.local` as a special path:

- sandbox code calls a local endpoint
- OpenShell strips sandbox-supplied credentials
- configured provider credentials are injected centrally
- the selected model/backend is gateway-controlled

This is one of OpenShell's most useful ideas because it preserves local calling
ergonomics while keeping model credentials under platform control.

## OpenShell's Security Model

The OpenShell docs emphasize defense in depth.

### Filesystem isolation

Filesystem access is restricted with Landlock. Paths are explicitly marked as:

- read-only
- read-write

Anything not declared is inaccessible.

### Process isolation

The sandbox process runs as an unprivileged user with seccomp restrictions. The
docs are explicit that there is no `sudo`, no setuid path, and no normal path
to privilege escalation.

### Network policy

Every outbound connection goes through the proxy. Policy is evaluated based on:

- destination host and port
- the binary that opened the connection

For REST endpoints with TLS termination enabled, OpenShell can also enforce:

- HTTP method
- HTTP path

This is stronger than a coarse egress allowlist because it ties network access
to specific executables instead of only to the sandbox as a whole.

### Inference policy

Inference traffic can be routed through `inference.local`, with credentials and
backend selection managed by the platform rather than by code inside the
sandbox.

## What OpenShell Is Especially Good At

OpenShell is particularly strong in these areas:

### 1. Strong local execution safety

The docs describe a serious sandbox stack:

- Landlock for filesystem restrictions
- seccomp for syscall restrictions
- network namespace isolation
- proxy-mediated egress

That is a strong runtime posture.

### 2. Declarative, reviewable policy

Policy YAML acts as a concrete artifact teams can inspect and version.

### 3. Binary-aware egress control

This is one of the best ideas in the OpenShell docs. It is materially better
than plain host allowlisting.

### 4. Hot-reloadable network policy

This is operationally important. It supports an iterative deny-observe-adjust
workflow without forcing sandbox recreation.

### 5. A strong pattern for inference routing

`inference.local` is a clean example of local ergonomics combined with central
authority over credentials and routing.

## Where OpenShell Stops Short of Capsule's Secure Access Plane

OpenShell is powerful, but its documented scope is different.

The key gaps relative to the Capsule proposal are below.

### 1. OpenShell is primarily a runtime policy system, not a full semantic access plane

OpenShell reasons mostly in terms of:

- sandbox
- binary path
- endpoint
- method and path
- provider record

The Capsule proposal needs more:

- user identity
- virtual identity
- agent identity
- `session_id`
- `runner_id`
- `turn_id`
- approval state
- revocation hooks tied to runtime lifecycle

### 2. OpenShell does not present remote broker execution as the default privileged lane

The Capsule proposal treats remote broker execution as the safest general answer
for credentialed and CLI-heavy work.

OpenShell's docs focus on secure in-sandbox execution plus policy mediation.
That is valuable, but it is a different default.

### 3. OpenShell's provider model is convenient, but closer to ambient sandbox credentials

The docs describe provider records whose credentials are attached to sandboxes
at runtime, often through environment-variable injection.

That is practical, but it is not the same as the Capsule proposal's default
position that durable credentials should remain outside the sandbox.

OpenShell mitigates this with strong runtime controls, but the Capsule secure
access plane is trying to go further by reducing the presence of reusable
authority inside the sandbox in the first place.

### 4. OpenShell does not expose a general helper-session model for CLI families

The Capsule proposal explicitly defines a lane for:

- exec plugins
- `credential_process`
- workload identity federation helpers
- Git credential helpers

OpenShell may be able to host some of these patterns, but the documented model
is not centered on helper-session issuance, helper TTLs, lifecycle revocation,
or helper grants bound to session and turn context.

### 5. OpenShell's cloud story is still relatively early

The docs explicitly note that cloud gateways are not yet intended for shared
team access. That suggests OpenShell's documented scope today is closer to:

- secure single-user or small-scope agent execution

than to:

- a fully multi-tenant, team-grade secure access authority for long-lived cloud
  agent sessions

That matters for Capsule's long-term direction.

## The Most Important Synthesis

OpenShell and the Capsule proposal are not substitutes.

The clean framing is:

- OpenShell answers: how do we run an agent safely inside a controlled sandbox?
- Capsule secure access plane answers: how do we model privileged access so
  long-lived agents do not accumulate ambient authority?

That leads to a clear architectural conclusion:

> Capsule should not replace a secure access plane with a sandbox policy engine.
> It should use a sandbox policy engine as a subordinate enforcement layer under
> a stronger access plane.

## What Capsule Should Borrow from OpenShell

OpenShell demonstrates several runtime patterns worth adopting or emulating.

### 1. Binary-aware network policy

Capsule's future Lane 2 direct HTTP grants would be materially stronger if
network policy can match:

- destination
- protocol
- optional HTTP method and path
- calling binary

### 2. Dynamic policy updates

Capsule's access plane will need to install and revoke temporary grants. A
hot-reloadable network policy model is a strong fit for that.

### 3. Privacy-router style local endpoints

`inference.local` shows a useful pattern:

- local stable endpoint for code
- central authority over credentials
- central control over routing target

Capsule should generalize that idea beyond inference where it makes sense.

### 4. Strong local runtime isolation

If Capsule is going to support harness-in-sandbox patterns, then:

- filesystem isolation
- process identity separation
- syscall restrictions
- local surface hardening

will matter even more, not less.

## What Capsule Should Not Copy Directly

Capsule should be careful not to adopt the parts of the OpenShell model that
would weaken the secure access plane vision.

### 1. Do not default to provider credentials being ambient in the sandbox

This is the biggest architectural caution. OpenShell's provider model is
convenient, but Capsule's proposal is aiming for a stricter authority boundary.

### 2. Do not reduce policy to network-only semantics

Host, port, binary, method, and path are useful, but they are not the whole
policy model. Capsule still needs actor-aware and lifecycle-aware grant
semantics.

### 3. Do not let direct in-sandbox access become the default for CLI-heavy work

That path creates both security pressure and implementation pressure. It nudges
the runtime toward becoming an auth aggregator instead of an execution
substrate.

## Recommended Future Architecture for Capsule

Capsule should evolve into a two-level system.

### Level 1: Runtime substrate

This layer should provide:

- sandbox or VM isolation
- filesystem controls
- process controls
- binary-aware network policy
- dynamic egress updates
- local endpoint and proxy primitives
- lifecycle notifications for pause, resume, fork, and migration

This is where OpenShell-like mechanisms are most useful.

### Level 2: Secure access plane

This layer should provide:

- identity resolution
- virtual identity mapping
- policy evaluation
- guardrails and approvals
- credential brokering
- grant issuance
- remote execution
- helper-session orchestration
- audit and tracing

This is the semantic authority layer and should remain distinct from the guest
runtime.

## Remote Broker Execution Should Stay the Default

The proposal is right to prioritize remote execution first.

It should remain the default for:

- provider CLIs
- internal admin tools
- write operations
- production-impacting calls
- unknown or new tool families

The reason is simple:

- it gives the strongest protection against credential exfiltration
- it yields the simplest audit story
- it avoids leaking provider-specific auth logic into Capsule itself

The tradeoff is reduced local fidelity for some workflows, but that tradeoff is
safer than making local credential use ambient.

## Direct HTTP Grants Should Be a Controlled Fidelity Escape Hatch

Direct in-sandbox HTTP is valuable in some cases:

- generated code needs to make the call directly
- the code path depends on local in-process behavior
- a REST or GraphQL client should not be rewritten as a remote CLI call

But this should be granted explicitly and bounded tightly:

- allowlisted destination
- optional method and path controls
- caller-binary matching where possible
- short TTL
- turn association
- explicit revocation on lifecycle changes

## Local Helper Sessions Should Be Rare and Highly Engineered

Local helper sessions are necessary for some workflows, but they should be
treated as the highest-complexity lane.

They are justified for a small number of families such as:

- `kubectl`
- Google WIF or executable ADC
- AWS `credential_process`
- selected Git credential helpers

But they should only be expanded after Capsule invests in stronger local
sub-isolation between:

- harness process
- generated code
- helper config and helper sockets

Without that, helper sessions risk turning into ambient local authority.

## Why `turn_id` Should Become First-Class

The proposal poses this as an open question. For Capsule, it should likely
become a first-class concept before broad grant support ships.

`turn_id` improves:

- audit precision
- approval semantics
- revocation semantics
- access duration control

Without turn-level binding, session-level grants can become too broad in
long-lived workflows.

## What "Good" Looks Like for Capsule

A mature Capsule secure access plane should support this flow:

1. An attested runner presents verifiable workload identity.
2. The access plane resolves actor context:
   - user
   - virtual identity
   - agent identity
   - session
   - runner
   - turn
3. Policy computes effective authority.
4. Guardrails determine whether approval is needed.
5. A short-lived grant is issued for a chosen lane.
6. Capsule installs only the minimal runtime changes required for that grant.
7. Audit records the full actor chain and resulting action.
8. Grant state is revoked automatically on expiry or lifecycle transition.

That is the target operating model.

## Recommended Sequencing

The rollout sequence in the proposal is sound and should be preserved.

### Phase 1: Remote broker execution first

Build:

- access API
- identity resolution
- policy engine
- audit pipeline
- remote executor fleet
- workload attestation integration

This covers the broadest surface safely.

### Phase 2: Direct HTTP grants

Add:

- dynamic per-runner egress updates
- narrow destination allowlists
- header or proxy transforms where necessary
- TTL and lifecycle-coupled revocation

This introduces in-sandbox fidelity for HTTP-heavy cases.

### Phase 3: Supported local helper sessions

Start with a small, explicit list of high-value families. Do not begin with
generic arbitrary helper support.

### Phase 4: Stronger in-sandbox local separation

If harness-in-sandbox remains strategic, invest in:

- separate users
- tighter filesystem permissions
- protected helper directories
- local socket isolation
- anti-ptrace hardening

Without this, Lane 3 remains substantially riskier.

## Final Recommendations

The most important decisions for Capsule are:

1. Externalize the secure access plane from Capsule's core runtime.
2. Treat sandbox and VM infrastructure as enforcement substrate, not semantic
   authority.
3. Keep remote broker execution as the default lane for privileged access.
4. Use direct HTTP grants and local helper sessions only as explicit,
   short-lived exceptions.
5. Optimize for real CLI-heavy workflows rather than an HTTP-only abstraction.
6. Move toward secretless or near-secretless sandboxes by default.
7. Make grants lifecycle-aware and, ideally, turn-aware.
8. Build full actor-chain audit from the beginning.

## Bottom Line

OpenShell is a strong example of secure runtime design for agent sandboxes.
Capsule's secure access plane proposal is a stronger statement about authority.

The future Capsule platform should combine both lessons:

- OpenShell-like runtime controls below
- Capsule secure access plane semantics above

That combination would let Capsule support long-lived, high-fidelity agent
sessions without letting durable authority quietly become ambient inside the
sandbox.
