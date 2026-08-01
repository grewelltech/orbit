# Ephemeral CI testbed — planning

Status: **planning / draft, 2026-08-01.** Not an accepted design. This records
the problem, the constraints that actually bind, the options, and the decisions
still to make. When the shape settles it becomes an ADR; nothing here should be
treated as decided.

**Goal.** A 5G core testbed that CI builds **fresh for every run**, validates
against, and destroys — so a merge into `main` is gated by integration evidence
that carries no state from any previous run.

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
confirmed once §8 Q1 is answered:

| Tier | Trigger | Needs a core? | Content |
|---|---|---|---|
| **T0 unit** | every push | no | `go test ./...`, `vet`, `gofmt`, `go mod tidy` drift |
| **T1 smoke** | every merge to `main` | yes, fresh | NG Setup, one attach, one PDU session, one ping over N3, teardown |
| **T2 integration** | every merge to `main` | yes, fresh | Multi-UE fleet, N2 + Xn handover with data continuity, app session (VoIP) |
| **T3 conformance** | every merge, or nightly | yes, fresh | The `orbit conformance` suite — the negative tests that need an expendable core |
| **T4 performance** | nightly / on demand | yes, fresh + warmed | Attach-rate sweep, soak, handover-under-load. Regression-tracked, not pass/fail on absolute numbers (§7) |

T0 already exists in `.github/workflows/ci.yml`. T1–T4 are what this document is
about. The existing `integration.yml` documents the contract but has no runner.

---

## 5. Options for the ephemeral core

### 5.1 Topology profile is a separate axis from isolation

Two supported shapes, and the choice is independent of how the core is built:

- **Collapsed** — N2, N3 and often N6 share one network. What SD-Core/Aether
  developers commonly run. No Multus/macvlan, no second interface, so the RAN
  and core can sit on one host and the user plane needs no special placement.
- **Separated** — N2, N3 and N6 on distinct networks, as ATB-01 is built and as
  a real deployment looks.

**Collapsed is the better default for most tiers.** It removes the constraint
that currently forces user-plane and handover tests onto the RAN node, makes an
ephemeral single-host build practical, and cuts the moving parts CI has to stand
up and verify.

**But it cannot be the only profile.** A collapsed run cannot catch the class of
bug where an interface is *selected* wrongly, because every interface is the same
one: an N3 socket bound to the wrong address, an N3 address advertised that
differs from the one actually used, or source-address selection that happens to
work when everything is on one subnet. Those are exactly the failures that bit
during Phase 1b/3 bring-up. Handover's distinct-routed-source-IP-per-gNB
requirement is satisfiable with multiple addresses on one network, so it does
*not* by itself force separation — but verifying that the AMF really keys on the
association address does benefit from it.

Proposal: **run most tiers collapsed, and at least one tier separated**, so the
fast path is cheap and interface-selection regressions still get caught. Which
tier lands where depends on §8 Q1.

The dedicated test environment being stood up will mirror the CI build exactly
and support both profiles, which removes the "works in CI, fails locally" gap
before it opens.

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

**Current lean, revised:** **B collapsed for T1–T3**, falling back to C if
BESS-UPF will not run nested, and **C separated for the one tier that has to
exercise real interface separation** (§5.1) plus T4, where a stable substrate
matters more than build speed. The earlier lean was C throughout, on the
assumption that a two-network build was unavoidable; the collapsed profile makes
that assumption wrong for most tiers.

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
8. **Which tier runs separated?** §5.1 argues at least one must. T2 (handover +
   data continuity) is the natural candidate, since it is where
   interface-selection bugs would show. Confirm once bring-up cost is known.
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
