# Ephemeral CI testbed — planning

Status: **planning / draft, 2026-08-01.** Not an accepted design. This records
the problem, the constraints that actually bind, the options, and the decisions
still to make. When the shape settles it becomes an ADR; nothing here should be
treated as decided.

**Goal.** A 5G core testbed that CI builds **fresh for every run**, validates
against, and destroys — so a merge into `main` is gated by integration evidence
that carries no state from any previous run.

**And built in every network topology ORBIT must support** (§5.1). ORBIT is a
test tool aimed at cores it does not control, so the deployment shape is part of
what has to be validated, not a convenience CI picks. A configuration that is
never exercised is a configuration ORBIT is not known to work in.

---

## 1. Why: leftover state is not hypothetical

ORBIT's integration testing today runs by hand from `grewell01` against ATB-01,
a long-lived testbed. That testbed has been mutated repeatedly in the course of
normal work, and its current state is the argument for this document:

- **Patched NF binaries.** AMF, UDM and UDR are running locally-built binaries
  bind-mounted from `/opt/orbit-patch` over the stock images, to carry
  unreleased `nrfcache` and UDR fixes.
- **Mutated resource limits.** MongoDB's StatefulSet was raised from the chart
  default of 750m CPU / 768Mi to 8 CPU / 6Gi.
- **10,000 injected subscribers.** Provisioned by cloning documents directly
  into MongoDB across six collections, bypassing `simapp` entirely.
- **Accumulated runtime state.** UE contexts, authentication status rows, TEIDs,
  NF registrations and Kafka topics from weeks of runs. NF processes had
  28 days of uptime before some were restarted.

Every one of those is invisible to a test that merely passes. A benchmark run
there measures *that* core, not SD-Core — which is exactly why
[`interop/sdcore-performance.md`](../interop/sdcore-performance.md) has to carry
an explicit "not quotable" banner. A CI gate cannot rest on a substrate whose
provenance nobody can state.

**Second-order problem: warm state changes results.** Measured on ATB-01, the
first run after restarting an NF was materially slower than steady state
(16.1 attach/s vs ~22 on the same build; 172/s cold vs 208–240/s warm after the
cache fixes). A fresh core is therefore *not* automatically a fair benchmark
substrate — see §7.

---

## 2. What must be ephemeral

Ranked by how badly leftovers corrupt a result:

| Layer | Leftover risk | Must be fresh? |
|---|---|---|
| Subscriber DB (Mongo: auth, provisioned, policy data) | Stale SQNs, leftover UEs, mutated provisioning | **Yes** |
| NF runtime state (UE contexts, TEIDs, NF registrations, NRF cache) | Stale associations, ID collisions, warm caches | **Yes** |
| NF images/config | Patched binaries, hand-edited configmaps, changed limits | **Yes** |
| UPF data-plane state (BESS flows, PFCP sessions) | Wedged UPF, stale FARs | **Yes** |
| Kafka topics / metricfunc | Event backlog skewing consumers | Yes |
| Kubernetes cluster | CRDs, PVCs, admission state | Probably |
| Node OS / kernel | Sysctls, conntrack, interfaces, routes | Decide (§5) |

**The RAN side needs the same treatment.** ORBIT's own leftovers bite: gNB IDs,
source IP bindings, and `bin/orbit` build provenance. A stale gNB ID is a known
failure — the omec AMF does not cleanly re-key a reused gNB ID from a new
address, and the resulting stale association makes it drop the Handover Request.
CI must use fresh gNB IDs per run, or a fresh core makes that moot.

---

## 3. Constraints that actually bind

These are measured or directly observed, not assumed. They rule out otherwise
attractive designs.

1. **N3 needs the gNB's N3 address to be reachable from the UPF** — the UPF sends
   downlink GTP-U there. On ATB-01 the access network is an isolated
   Multus/macvlan segment unreachable from the control-plane host, which is why
   user-plane tests run from the RAN node.
   **That is a property of ATB-01's deployment, not a protocol requirement.**
   SD-Core/Aether developers commonly run a **collapsed** topology with N2, N3
   and often N6 sharing one network, and it is a supported configuration. CI can
   therefore choose its topology rather than being forced into a two-network
   build — see §5.1.
2. **Handover needs a distinct routed source IP per gNB.** The AMF distinguishes
   gNBs by association address.
3. **The BESS UPF wedges** under sustained load (data plane hangs; pod stays
   Running with 0 restarts). Recovery requires deleting `upf-0` **and**
   restarting the SMF so it re-establishes the PFCP association. A CI run that
   hits this must fail loudly, not hang — and ephemerality makes recovery
   "rebuild" rather than "repair".
4. **Chart defaults are not benchmark-ready.** MongoDB ships at 750m CPU and was
   throttled ~24% of all periods. If CI benchmarks against chart defaults it
   measures the chart, not the core. Whatever CI builds must pin resources
   explicitly and record them alongside results.
5. **Conformance negative tests are deliberately hostile.** They send malformed
   NGAP and assert the core survives. They need a core nobody else is using —
   which ephemerality gives for free, and which is why they have never run
   against ATB-01.
6. **Bring-up is not instant.** SD-Core is ~16 pods plus a MongoDB replica set,
   with NF registration and NRF convergence after that. Bring-up time is the
   single biggest input to whether this is per-merge or nightly, and it is
   **not yet measured** (§8).

---

## 4. Test tiers

Not everything can run per-merge if bring-up is minutes. Proposed split, to be
confirmed once §8 Q1 is answered. The matrix is tiers x **topology profiles**
(§5.1) — breadth is spent on the cheap tiers, depth on the primary profile.

| Tier | Trigger | Needs a core? | Topology (§5.1) | Content |
|---|---|---|---|---|
| **T0 unit** | every push | no | n/a | `go test ./...`, `vet`, `gofmt`, `go mod tidy` drift |
| **T1 smoke** | every merge to `main` | yes, fresh | **all profiles** | NG Setup, one attach, one PDU session, one ping over N3, teardown |
| **T2 integration** | every merge to `main` | yes, fresh | **all profiles** (or primary + rotating, full matrix nightly) | Multi-UE fleet, N2 + Xn handover with data continuity, app session (VoIP) |
| **T3 conformance** | every merge, or nightly | yes, fresh | one per merge, full matrix nightly | The `orbit conformance` suite — the negative tests that need an expendable core |
| **T4 performance** | nightly / on demand | yes, fresh + warmed | **pinned to one**; others tracked as separate trends | Attach-rate sweep, soak, handover-under-load. Regression-tracked, not pass/fail on absolute numbers (§7) |

T0 already exists in `.github/workflows/ci.yml`. T1–T4 are what this document is
about. The existing `integration.yml` documents the contract but has no runner.

---

## 5. Options for the ephemeral core

### 5.1 Network topology is a validated dimension, not a deployment choice

ORBIT is a test tool: it has to work against whatever topology the core under
test happens to be deployed in. So topology is not something CI picks for
convenience — **every supported configuration is validated, every time**, because
a configuration that is not exercised is a configuration ORBIT is not known to
support.

#### The blind spots are mutually exclusive

This is the load-bearing argument for a matrix rather than a default. Each
profile is blind to the other's failure mode:

- **Collapsed-only** cannot catch a wrongly *selected* interface, because every
  interface is the same one. An N3 socket bound to the wrong address, an N3
  address advertised that differs from the one actually used, or source-address
  selection that happens to work on a single subnet — all pass. These are the
  failures that bit during Phase 1b/3 bring-up.
- **Separated-only** cannot catch code that *assumes* separation. Anything that
  implicitly relies on the N2 and N3 addresses being different — binding logic,
  socket or port collisions when both planes share one address, demux keys that
  are only unique because the addresses differ, dedup that silently collapses —
  all pass when the addresses are distinct and break the moment they are not.

Neither profile is a superset of the other, so neither can stand in for the
other. Running one and inferring the other is how a tool ships broken against a
deployment shape nobody tested.

#### Named profiles

Declared in-repo so tests reference them by name and every result is attributable
to a topology:

| Profile | N2 | N3 | N6 | Hosts | Represents |
|---|---|---|---|---|---|
| `collapsed-all` | shared | shared | shared | single | simplest dev setup; all planes on one network |
| `collapsed-n2n3` | shared | shared | separate | single | the common SD-Core/Aether dev shape |
| `separated` | distinct | distinct | distinct | split RAN/core | ATB-01, and what a real deployment looks like |

Each profile must also exercise **multiple gNB addresses**, since handover
requires a distinct routed source IP per gNB and that requirement interacts with
topology differently in each shape.

Adding a profile is how ORBIT records that it supports a new deployment shape.
The list is expected to grow — IPv6 and dual-stack are the obvious next
candidates once the matrix mechanism exists.

#### Cost, and where it is spent

A full matrix is tiers x profiles, which multiplies bring-up cost — the one thing
§8 Q1 says we cannot yet size. The strategy is to make **breadth cheap and depth
selective**:

- **T1 smoke runs on every profile, every merge.** It is the cheapest tier and
  it is exactly what catches "this configuration does not come up at all", which
  is the most likely topology-specific breakage. Breadth here is the highest
  value per second of CI time.
- **T2 integration runs on every profile, every merge** if bring-up allows;
  otherwise on the primary profile every merge plus a rotating second profile,
  with the full matrix nightly. Handover and data continuity are where
  interface-selection bugs actually surface, so this tier benefits most from
  breadth.
- **T3 conformance runs on one profile per merge, full matrix nightly.** The
  checks are control-plane and largely topology-insensitive, so breadth buys
  less here.
- **T4 performance pins one profile.** Numbers are not comparable across
  topologies, so a matrix would produce trend lines that move for reasons
  unrelated to the change. Any other profile is benchmarked as its own separate
  trend, never compared against the primary.

Profiles run **in parallel** where capacity allows, so matrix breadth costs
wall-clock once rather than N times.

#### Failure attribution

Every result records the profile it ran under, and a failure names it. "T2 failed"
is not actionable; "T2 failed on `collapsed-all`, passed on `separated`" points
straight at address-selection logic and is often the whole diagnosis.

The dedicated test environment being stood up mirrors the CI build exactly and
supports every profile, so a topology-specific failure is reproducible by hand
without rebuilding CI's world.

### 5.2 Build options

Ordered by isolation strength. None chosen yet.

**A — Kubernetes namespace per run.** Deploy SD-Core into a fresh namespace on a
persistent cluster; delete the namespace after.
*For:* fastest; reuses the existing RKE2 cluster and Helm charts.
*Against:* shares node kernel, conntrack, sysctls and any host-level UPF state;
namespace deletion can hang on finalizers; Multus/macvlan address allocation must
be namespace-scoped or it collides between concurrent runs. **Weakest isolation
against exactly the class of leftovers §1 describes.**

**B — Ephemeral cluster per run** (kind / k3d / RKE2-in-VM).
*For:* clean cluster state including CRDs and PVCs; destroy is a single
operation; plausibly reproducible from a pinned manifest set. **A collapsed
topology (§5.1) removes the multi-network requirement, which was this option's
main obstacle** — the remaining question is only whether BESS-UPF tolerates a
nested cluster.
*Against:* the UPF still needs privileges; nested-cluster support is
**unverified** (§8 Q2).

**C — Fresh VMs per run** (LXD/libvirt from a golden image).
*For:* clean OS, kernel, interfaces and routes; reproduces the real two-node
topology including the access network; matches how ATB-01 is actually built.
*Against:* slowest; needs image build and maintenance; needs a hypervisor host
with capacity for concurrent runs.

**D — Bare-metal reimage.** Cleanest, far too slow for per-merge. Mentioned only
to be ruled out except as a periodic baseline.

**E — testbox layered OS** ([bgrewell/testbox](https://github.com/bgrewell/testbox)).
An mkosi-built Ubuntu 24.04 image on btrfs subvolumes: an immutable `@base`, an
ephemeral `@runtime` **recreated as a fresh snapshot of `@base` on every boot**,
a `@hostid` carve-out so SSH identity survives, and named `@<state>` layers that
can be saved, switched from the bootloader, and chained (a state may snapshot
another state).

*For:*
- **Clean is the default, not a step.** Everything outside a saved layer is
  discarded on reboot, so a leaked artifact requires someone to have gone out of
  their way. Compare option A, where cleanliness depends on teardown running and
  being verified. This inverts §6 from "assert seven things are clean" to "assert
  nothing was deliberately persisted".
- **Chained states map onto the topology matrix.** `@base` →
  `@sdcore-deployed` → `@collapsed-all` / `@collapsed-n2n3` / `@separated`, so
  each profile is a cheap snapshot off a shared converged core rather than a
  separate deployment. Adding a profile is a snapshot.
- **Restore replaces deploy.** Attacks §8 Q1 directly: bring-up becomes reboot +
  service start + NF reconverge instead of a full SD-Core deploy.
- **Deterministic starting state**, which is better than either cold or warm
  (§7) — a fixed point on that curve rather than wherever the previous run left
  things.
- **The base is declarative** (mkosi), so the golden image is *derived*, not
  hand-crafted. That is what stops this from recreating §1's drift problem at a
  different level.
- `bootctl set-oneshot` switching leaves the default target alone, so the box
  returns to `fresh` on the following boot without an explicit cleanup step.

*Against / to resolve:*
- **`state export` / `import` is deferred (roadmap S5).** Named layers are local
  to a box, so a layer cannot yet be built once and distributed to N runners.
  **This is the blocking gap for multi-runner CI** — until it exists, either
  every runner rebuilds its layers (slow, and reintroduces drift) or CI is
  single-box.
- **Journals do not survive an ephemeral reboot.** Deliberate, and correct for
  isolation — but a failing CI run that reboots loses its logs, which is exactly
  when they are wanted. Logs and artifacts must be shipped off-box *during* the
  run, or the failure state saved as a layer for post-mortem before reset.
- **Reset costs a reboot**, not a snapshot rollback of a running system, and NF
  processes restart cold — memory state is not preserved. Real, but far cheaper
  than redeploying.
- **MongoDB on btrfs CoW** needs measuring before T4 numbers are trusted (§3.4
  already flags Mongo as the first bottleneck found). CoW fragmentation under a
  database is a known hazard; `chattr +C` is the usual mitigation and interacts
  with snapshotting.
- UEFI only, and the project has not been touched since 2026-05.

**Current lean, revised again: E (testbox) as the substrate, hosting B or C.**
The options are not exclusive — testbox supplies the *machine* and its
clean-by-default guarantee, and the core still has to run on something inside it
(an ephemeral k3s/RKE2 whose etcd lives on `@runtime`, so a reboot wipes the
cluster too). That combination gets clean-by-default from E, cluster isolation
from B, and real interface separation from C for the `separated` profile.

Blocking question for E is roadmap S5 (`state export`/`import`), which decides
whether CI can be multi-runner or stays single-box. Worth asking whether S5 moves
up.

---

## 6. What "clean" has to mean, concretely

A build is clean only if all of these hold at the start of a run. Each should be
**asserted by the harness**, not assumed — a silent leftover is worse than a
failed build.

- Zero subscriber documents except those this run provisioned, across all six
  collections that key on `ueId`.
- Zero UE contexts / `amfState` rows.
- NF images match a pinned digest; no bind-mounted binaries, no hand-edited
  configmaps. Record the digests in the run artifact.
- Resource limits come from the pinned values, not chart defaults (§3.4).
- No PFCP sessions or FARs on the UPF.
- gNB IDs unused within this core's lifetime.
- Kafka topics empty or freshly created.

**Provisioning must be part of the build.** ORBIT currently relies on subscribers
that already exist. CI has to create them as a build step — via `simapp`'s
configmap for the supported path, or direct Mongo insertion for bulk (the
approach used to add 10,000 subscribers, recorded in
`interop/sdcore-performance.md`). Pick one and make it the only one.

**Teardown must be verified, not fired-and-forgotten.** The harness should assert
the namespace/cluster/VM is gone before reporting success, so a leaked
environment fails the run that leaked it rather than corrupting the next one.

---

## 7. Benchmarks on a fresh core: the cold-start problem

A fresh core is the right substrate for **correctness**, but it is a subtle one
for **performance**, because measured numbers differ cold vs warm (§1). Options:

- **Warm-up phase** before measurement (attach and discard N UEs) so NRF caches,
  Mongo working set and JIT-ish effects settle. Cheap; makes runs comparable.
- **Report cold explicitly** as its own metric — arguably the more honest number,
  since it reflects a real cold core.
- **Both**, reported separately, in the same spirit as sim-capability vs
  integration-capability.

Whichever is chosen, T4 must be **regression-tracked against the previous run on
the same substrate**, not gated on absolute thresholds. Absolute SLO gates on a
freshly built core will be flaky, and a flaky perf gate gets ignored, which is
worse than not having one.

---

## 8. Open questions

These need answers before this can become an ADR. Q1 and Q2 are on the critical
path.

1. **How long does a fresh SD-Core take to be attach-ready?** Not just pods
   Running — NRF converged and an NG Setup accepted. This decides per-merge vs
   nightly for T2–T4. *Measure it: script the deploy and time to first successful
   `orbit cell ngsetup`.*
2. **Does BESS-UPF work in a nested/ephemeral cluster (option B)?** Needs
   privileges; a collapsed topology removes the Multus requirement, so this is
   now the *only* thing standing between B and being the default for T1–T3.
   *Spike it — highest-value single experiment in this document.*
3. **Where does CI run?** ATB-01 has no spare capacity for concurrent ephemeral
   environments. A dedicated environment is being stood up, mirroring the CI
   build and supporting both topology profiles. Concurrency limit: 1, or N?
8. **Can the full matrix run per-merge, or only T1?** Depends entirely on Q1.
   If bring-up is fast and profiles run in parallel, T2 across all profiles is
   affordable per-merge; if not, T2 falls back to primary-plus-rotating with the
   full matrix nightly. Decide with a measured number, not a guess.
9. **Does testbox S5 (`state export`/`import`) land?** Decides multi-runner vs
   single-box CI (§5.2 E). The highest-leverage dependency outside this repo.
10. **Does MongoDB on btrfs CoW distort T4?** Measure before trusting any number
    produced on a testbox substrate.
11. **Which profile is primary** (the T4 pin, and the one T2 always runs)?
   `collapsed-n2n3` is the likely answer as the common dev shape, but the
   argument for `separated` is that it is what production looks like.
4. **Which SD-Core version does CI track** — a pinned release, or `latest`? A
   pinned digest makes ORBIT's results reproducible; `latest` makes CI an early
   warning for upstream regressions. Possibly both, on different tiers.
5. **Do the unreleased patches** (`nrfcache`, UDR race) get baked into the CI
   core, or does CI track stock? If stock, T4 numbers stay at the stock ceiling
   and the patched numbers remain unquotable — which is the honest default until
   those land upstream.
6. **Credentials.** Ki/OPc must reach the runner without landing in logs or
   artifacts. Generated per-run alongside provisioning is cleanest, since nothing
   then needs to be a long-lived secret.
7. **Self-hosted runner security.** A runner that can build and destroy testbeds
   is a high-value target; PRs from forks must not reach it.

---

## 9. Success criteria

This is done when:

- A merge to `main` triggers a build that stands up a core from pinned artifacts,
  provisions subscribers, runs T1–T3, tears down, and **verifies teardown**.
- **Every supported network topology is exercised on every merge** at least at
  T1, so no configuration is silently unvalidated, and every result names the
  profile it ran under.
- Any leftover from a previous run fails the build rather than silently changing
  a result.
- The run artifact records exactly what was tested: image digests, chart values,
  resource limits, subscriber count, ORBIT commit.
- T4 tracks performance as a trend on a known substrate, and its numbers are
  quotable — i.e. the provenance question that `sdcore-performance.md` has to
  hedge today is answered automatically.

## 10. Non-goals

- Replacing ATB-01 for exploratory and debugging work. Long-lived testbeds are
  useful precisely because state persists; this is about the gate, not the lab.
- Testing a real gNB or real UE (out of scope for a PHY-less tool; see DESIGN
  "Beyond P7").
- Multi-vendor core coverage. One core, built reproducibly, first.
