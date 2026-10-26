# Ephemeral CI testbed — planning

Status: **planning / draft, 2026-08-01.** Not an accepted design. This records
the problem, the constraints that actually bind, the options, and the decisions
still to make. When the shape settles it becomes an ADR; nothing here should be
treated as decided.

**Reviewed 2026-10-26.** The topology argument (§5.1) stands unchanged. Added:
UPF-5 to §3, which currently blocks the collapsed profiles. Rewritten: §5.2 E,
because the tool it describes was renamed to cleanslate and its clean-by-default
premise has reversed — the substrate lean is now an open question, not a lean.

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
   and often N6 sharing one network, and it is a supported configuration. CI is
   therefore not forced into a two-network build by the protocol — but see
   constraint 7, which currently blocks the collapsed profiles anyway, and §5.1.
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
7. **A stock BESS UPF has no downlink to a gNB on its own access subnet**
   (sdcore-lab triage **UPF-5**, `conf/route_control.py:713-714`, confirmed in
   code and by test). `route_control` programs only kernel routes that have a
   gateway, silently skipping connected routes, so BESS `accessRoutes` never
   learns the on-link subnet and downlink falls through to `accessbad_route`
   (12 of 12 packets in one test). Uplink works and the application even
   replies, so **a session comes up looking healthy while the return path is
   dead** — the failure does not announce itself.
   **This blocks `collapsed-all` and `collapsed-n2n3` (§5.1):** both place the
   gNB on the UPF's access subnet, so neither can pass T1's ping over N3 until
   UPF-5 is fixed. `separated` is unaffected only because its far access subnet
   is reached via a gateway. Until the fix lands, either the collapsed profiles
   are expected-fail at T1, or the matrix waits on it. It also distorts N6:
   `coreRoutes` carries only the default route, so uplink to an on-link N6
   server hairpins through the site router.

---

## 4. Test tiers

Not everything can run per-merge if bring-up is minutes. Proposed split, to be
confirmed once §8 Q1 is answered. The matrix is tiers x **topology profiles**
(§5.1) — breadth is spent on the cheap tiers, depth on the primary profile.

| Tier | Trigger | Needs a core? | Topology (§5.1) | Content |
|---|---|---|---|---|
| **T0 unit** | every push | no | n/a | `go test ./...`, `vet`, `gofmt`, build (`go mod tidy` drift is *not* gated today — see note) |
| **T1 smoke** | every merge to `main` | yes, fresh | **all profiles** | NG Setup, one attach, one PDU session, one ping over N3, teardown |
| **T2 integration** | every merge to `main` | yes, fresh | **all profiles** (or primary + rotating, full matrix nightly) | Multi-UE fleet, N2 + Xn handover with data continuity, app session (VoIP) |
| **T3 conformance** | every merge, or nightly | yes, fresh | one per merge, full matrix nightly | The `orbit conformance` suite — the negative tests that need an expendable core. **Runtime is now material:** 9 checks, two of which budget 190 s and 145 s because they wait out real NAS timers (orbit#179) |
| **T4 performance** | nightly / on demand | yes, fresh + warmed | **pinned to one**; others tracked as separate trends | Attach-rate sweep, soak, handover-under-load. Regression-tracked, not pass/fail on absolute numbers (§7) |

T0 already exists in `.github/workflows/ci.yml` — gofmt, `go vet ./...`,
`go vet -tags=integration ./...`, `go test ./...` and build, plus a web job
(typecheck, vitest, build). It does **not** check `go mod tidy` drift; that gap
is why pflag sat mislabelled as indirect until orbit#177. Adding the check is a
new gate, not a cleanup, so it is listed here as intended scope rather than
current behaviour. T1–T4 are what this document is about. The existing
`integration.yml` documents the contract but has no runner — it is
`workflow_dispatch` against a self-hosted `lab-core` runner.

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
  value per second of CI time. **Blocked today for the collapsed profiles by
  UPF-5 (§3.7)**, whose ping over N3 cannot pass on a stock UPF — see §8 Q3 for
  the wait-or-expected-fail decision.
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

**E — cleanslate** ([bgrewell/cleanslate](https://github.com/bgrewell/cleanslate),
formerly `testbox`; the old links redirect). A custom Ubuntu-based distro built
from a baseline image you produce once and install on as many machines as you
like. Each machine then runs a **slate**: a named, persistent line of work.
A checkpoint is taken automatically at every boot, so there is always a way
back; `fork` branches a slate, `switch` moves between them, `rollback` returns
to a checkpoint, and `reset` returns a slate to the pristine baseline, which is
never pruned. Rollback and switch take effect at the **next boot** — a slate is
the running system and cannot be replaced underneath itself. The boot menu
carries one entry per slate plus `scratch` (a throwaway copy of the baseline,
discarded at reboot) and `rescue` (the baseline, read-only).

*For:*
- **Named slates map onto the topology matrix.** A converged core can be forked
  per profile — `sdcore-deployed` → `collapsed-all` / `collapsed-n2n3` /
  `separated` — so adding a profile is a fork off a shared parent rather than a
  separate deployment.
- **Restore replaces deploy.** Attacks §8 Q1 directly: bring-up becomes boot +
  service start + NF reconverge instead of a full SD-Core deploy.
- **Deterministic starting state**, better than either cold or warm (§7) — a
  fixed point on that curve rather than wherever the previous run left things.
- **The baseline is declarative**, so the golden image is *derived*, not
  hand-crafted. That is what stops this from recreating §1's drift problem at a
  different level.
- **A disposable mode exists** for the runs that want it:
  `switch <slate> --scratch --reboot` runs from a copy and throws it away at the
  next reboot.

*Against / to resolve:*
- **Clean is NOT the default — keeping things is.** This reverses the premise an
  earlier draft of this section rested on. Persistence is the normal case and
  disposability is opt-in per run (`--scratch`), so CI gets cleanliness only by
  deliberately asking for it on every run. That is much closer to option A,
  where cleanliness depends on a teardown step running and being verified, than
  the earlier draft claimed. **§6 therefore still has to assert that things are
  clean; it cannot be reduced to "assert nothing was deliberately persisted".**
- **Copy-on-write makes databases about 4x slower, and the usual mitigation does
  not work.** Slates live on a CoW filesystem, and marking a data directory
  no-copy-on-write is defeated by the checkpoint taken at every boot, which
  forces the copying anyway. cleanslate's own documentation measures a database
  at roughly a quarter of its otherwise achievable speed, says so plainly, and
  advises putting such data on a separate disk — adding that **numbers taken on
  a slate partly measure the filesystem underneath**. §3 already identifies
  MongoDB as the first bottleneck found, and T4 is the tier that most depends on
  trustworthy numbers, so this is a direct argument against pinning T4 to this
  substrate without a separate disk for Mongo.
- **A filesystem nested inside a slate cannot be checkpointed.** If something on
  a slate creates its own filesystem — a container runtime configured to do so,
  or a hand-made subvolume — a checkpoint cannot capture its contents and a
  rollback would leave it empty. `status` warns, `list` marks the slate, and
  `checkpoint` refuses without `--allow-incomplete`. **This bears directly on
  hosting B inside E**, since an ephemeral k3s/RKE2 whose etcd lives on the
  slate is exactly that case; it needs verifying before the combination is
  assumed to work.
- **Reset costs a reboot**, not a snapshot rollback of a running system, and NF
  processes restart cold. Real, but far cheaper than redeploying.
- **Layers are local to a box.** A slate cannot yet be built once and
  distributed to N runners, which is **the blocking gap for multi-runner CI** —
  until it exists, either every runner builds its own slates (slow, and
  reintroduces drift) or CI is single-box.
- **Diagnostics are lost on reset**, which is exactly when they are wanted. Logs
  and artifacts must be shipped off-box *during* the run, or the failure state
  checkpointed for post-mortem before reset.
- UEFI only, and the project has not been pushed since 2026-08-03.

*Filed against the project* (2026-08-01), so the work has a home rather than
living only in this document — all four were still open at the 2026-10-26 review:
[#1 state export/import (S5): layers are local to a box, which forces single-box CI](https://github.com/bgrewell/cleanslate/issues/1),
[#2 Unattended scratch runs discard their diagnostics on reset](https://github.com/bgrewell/cleanslate/issues/2),
[#3 Automatic checkpoints weaken nodatacow for databases and other CoW-hostile workloads](https://github.com/bgrewell/cleanslate/issues/3),
[#4 Automation ergonomics: switch, rollback and reset all defer to the next boot](https://github.com/bgrewell/cleanslate/issues/4).

**Substrate choice: OPEN, and needs re-deciding.** An earlier draft leaned on
E as the substrate hosting B or C, on the strength of a clean-by-default
guarantee the tool does not provide. With that premise gone, E's remaining case
is fork-per-profile and restore-instead-of-deploy, both still real and both
still valuable — but it no longer dominates A, and its CoW cost argues against
it for T4 specifically. The options were never exclusive, so a split decision
(E for the profile matrix and T1–T3, something else for T4 benchmarking) is now
worth considering on its merits rather than assumed away. **Deciding this needs
a measured Q1 number and the §8 Q2 spike, not another round of reasoning.**

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

These need answers before this can become an ADR. Q1, Q2 and Q3 are on the
critical path.

1. **How long does a fresh SD-Core take to be attach-ready?** Not just pods
   Running — NRF converged and an NG Setup accepted. This decides per-merge vs
   nightly for T2–T4. *Measure it: script the deploy and time to first successful
   `orbit cell ngsetup`.*
2. **Does BESS-UPF work in a nested/ephemeral cluster (option B)?** Needs
   privileges. A collapsed topology removes the Multus requirement, so this and
   UPF-5 (§3.7) are what stand between B and being the default for T1–T3.
   *Spike it — highest-value single experiment in this document.*
3. **When does UPF-5 land, and what do the collapsed profiles do until then?**
   §3.7 blocks `collapsed-all` and `collapsed-n2n3` at T1 on a stock UPF. Either
   the matrix waits on the fix, or those profiles are recorded as expected-fail
   so the gate stays meaningful. This now gates the central proposal of §5.1, so
   it ranks with Q1 and Q2.
4. **Where does CI run?** ATB-01 has no spare capacity for concurrent ephemeral
   environments. A dedicated environment **now exists** — the Ref 2 LXD testbed
   in `testing/lxd-testbed` (8 VMs: `rsite`, `wanem`, `rran`, `ctrl`, `core`,
   `ran-near`, `ran-far`, `app`), stood up 2026-10-24 and verified at 29/29
   checks with the far site passing 5/5 at 50 Mbps. **It is a `separated`
   topology only**, so it does not yet supply the collapsed profiles the matrix
   needs; near-site user plane fails on UPF-5 (§3.7), which is why user-plane
   work runs from `ran-far`. Concurrency limit: 1, or N?
5. **Can the full matrix run per-merge, or only T1?** Depends entirely on Q1.
   If bring-up is fast and profiles run in parallel, T2 across all profiles is
   affordable per-merge; if not, T2 falls back to primary-plus-rotating with the
   full matrix nightly. Decide with a measured number, not a guess.
6. **Which profile is primary** (the T4 pin, and the one T2 always runs)?
   `collapsed-n2n3` was the likely answer as the common dev shape, but it is
   currently the least viable of the three under §3.7, while `separated` is both
   what production looks like and the only profile the existing testbed provides.
   Revisit once UPF-5 lands rather than deciding around the bug.
7. **Can a cleanslate slate be distributed to N runners?** Decides multi-runner
   vs single-box CI (§5.2 E). The highest-leverage dependency outside this repo.
   Tracked as [cleanslate#1](https://github.com/bgrewell/cleanslate/issues/1),
   still open at the 2026-10-26 review.
8. **Does MongoDB on a CoW slate distort T4?** cleanslate's own documentation
   already answers the general case — about 4x slower, and `nodatacow` is
   defeated by the per-boot checkpoint (§5.2 E) — so the open part is narrower:
   whether a separate disk for Mongo restores trustworthy T4 numbers. Tracked as
   [cleanslate#3](https://github.com/bgrewell/cleanslate/issues/3).
9. **Which SD-Core version does CI track** — a pinned release, or `latest`? A
   pinned digest makes ORBIT's results reproducible; `latest` makes CI an early
   warning for upstream regressions. Possibly both, on different tiers.
10. **Do the unreleased patches** (`nrfcache`, UDR race) get baked into the CI
    core, or does CI track stock? If stock, T4 numbers stay at the stock ceiling
    and the patched numbers remain unquotable — which is the honest default until
    those land upstream.
11. **Credentials.** Ki/OPc must reach the runner without landing in logs or
    artifacts. Generated per-run alongside provisioning is cleanest, since
    nothing then needs to be a long-lived secret.
12. **Self-hosted runner security.** A runner that can build and destroy
    testbeds is a high-value target; PRs from forks must not reach it.

---

## 9. Success criteria

This is done when:

- A merge to `main` triggers a build that stands up a core from pinned artifacts,
  provisions subscribers, runs T1–T3, tears down, and **verifies teardown**.
- **Every supported network topology is exercised on every merge** at least at
  T1, so no configuration is silently unvalidated, and every result names the
  profile it ran under. A profile that cannot pass for a known upstream reason
  (UPF-5, §3.7) is recorded as expected-fail with that reason named — never
  quietly dropped from the matrix, which would return it to being unvalidated.
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
