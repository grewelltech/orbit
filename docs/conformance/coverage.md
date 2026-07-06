# Conformance coverage map

A **living checklist** for ORBIT's conformance / regression suite. It exists so
coverage is walked *systematically* from the 3GPP specs rather than assembled ad
hoc, and so anyone can see what's covered, what's next, and the citation behind
each check.

> Grounding rule: every cell's exact clause number and IE criticality is
> resolved from the **actual TS at the version under test** when the check is
> authored — not from memory. The procedure names and directions below are the
> stable scaffold; the precise citations get filled in per check.

## How this map is built

3GPP protocol specs don't read like one linear RFC, but they have a consistent
shape you can walk. For each interface spec (NGAP here) the coverage comes from
two clauses:

- **Procedural coverage** ← the *Elementary Procedures* clause (NGAP clause 8),
  which enumerates every procedure. One "does the happy path complete?" check
  per procedure.
- **Negative coverage** ← the *Handling of unknown / unforeseen / erroneous
  protocol data* clause (NGAP clause 10) **×** the **criticality** (`reject` /
  `ignore` / `notify`) and presence (M/O/C) of each IE in the message-contents
  tables (clause 9.2). This is a finite, spec-defined matrix: for each message ×
  each IE × its criticality, construct the malformation and assert the clause-10
  behaviour.
- **Security** ← TS 33.501 (integrity, ciphering, replay, key handling).
- **Timing** ← concurrent-procedure interleavings (flaky-allowed category).

The per-check authoring cycle (pick clause → set verdict semantics → build
stimulus → run live + pcap-disambiguate → calibrate honestly → cite) is in
[DESIGN.md](../DESIGN.md).

## Release policy

- **The suite is release-*aware*, not multi-release-exhaustive.** Every check is
  tagged with the 3GPP **Release** and TS version it asserts against (carried in
  its `SpecRef`, e.g. `TS 38.413 v16.8.0 §8.7.1`).
- **Baseline: match the core under test.** ORBIT's baseline tracks the release
  the DUT implements. For the SD-Core testbed that is the omec NGAP/NAS
  lineage — **confirm the exact release from the deployed omec version** before
  asserting release-specific behaviour (it is roughly Rel-15/16-era; do not
  assume Rel-17+ IEs are present).
- **Cross-release deltas are mostly additive.** The core NGAP/NAS procedures are
  stable Rel-15 → Rel-18; most deltas are new IEs / new procedures / new
  features (slicing enhancements, RedCap, NR-NTN, etc.). A check that depends on
  a release-specific IE or procedure declares that dependency and is skipped
  (`SKIP`) against older cores rather than failing them.
- **Codec constraint (real limit):** encode/decode is bounded by free5gc's type
  coverage (~Rel-15/16, some Rel-17). **Rel-18-only** messages/IEs need codec
  support before they can even be built — so "cover Rel-18" is gated on codec
  work, not just on writing a check. Track that honestly; don't claim a release
  the codec can't represent.

## What is testable, and how

ORBIT plays the **gNB + UE**; the **device under test is the core**. That sets
where each kind of check is meaningful:

- **RAN → core messages** (NG Setup, Initial UE Message, Handover Required, Path
  Switch, …): ORBIT controls the bytes, so these are the primary **negative /
  crash-safety** surface. ✅ best signal.
- **Bidirectional** (NG Reset, Error Indication): RAN-initiated variants are
  testable as procedural + negative.
- **Core → RAN messages** (Paging, Initial Context Setup, Handover Request, …):
  ORBIT is the *responder*, so a malformed *message* would test ORBIT, not the
  core. The core-testing angle here is **wrong / absent / late RAN responses**
  (does the AMF time out and clean up gracefully?) — that lives in the **timing**
  category, not negative-IE.

Legend: ✅ covered · 🎯 high-value next · ⬜ candidate · — not core-testable this way

## NGAP — TS 38.413

### Interface management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| NG Setup | RAN→AMF | ✅ (used everywhere) | 🎯 missing mandatory IE / bad PLMN / bad TAC → NG Setup Failure | the densest entry point |
| RAN Configuration Update | RAN→AMF | ⬜ | 🎯 malformed IE (high-value set) | |
| NG Reset | RAN→AMF | 🎯 → NG Reset Acknowledge | ⬜ | RAN-initiated reset |
| Error Indication | both | ✅ (we assert the core *sends* it) | — | |
| AMF Configuration Update | AMF→RAN | — | — | ORBIT receives |
| Overload Start / Stop | AMF→RAN | — | — | ORBIT receives |
| AMF Status Indication | AMF→RAN | — | — | ORBIT receives |

### NAS transport
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| Initial UE Message | RAN→AMF | ✅ (attach) | 🎯 malformed / oversized NAS PDU, bad TAI | |
| Uplink NAS Transport | RAN→AMF | ✅ | ✅ `NGAP-UNKNOWN-UE-SURVIVES` (unknown UE-NGAP-ID pair) | our first check; clause-10 AP-ID case |
| Downlink NAS Transport | AMF→RAN | — | — | timing angle only |
| NAS Non Delivery Indication | RAN→AMF | ⬜ | ⬜ | |
| Reroute NAS Request | AMF→RAN | — | — | |

### UE context management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| Initial Context Setup | AMF→RAN | ✅ (attach) | — | timing angle (bad/late response) |
| UE Context Release Request | RAN→AMF | ⬜ | ⬜ | |
| UE Context Release (Command/Complete) | AMF→RAN | ✅ | — | |
| UE Context Modification | AMF→RAN | ⬜ | — | timing angle |

### PDU session management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| PDU Session Resource Setup | AMF→RAN | ✅ (attach + data) | — | timing angle |
| PDU Session Resource Release | AMF→RAN | ⬜ | — | |
| PDU Session Resource Modify | AMF→RAN | ⬜ | — | |
| PDU Session Resource Notify | RAN→AMF | ⬜ | ⬜ | |
| PDU Session Resource Modify Indication | RAN→AMF | ⬜ | 🎯 | RAN-initiated → good negative surface |

### Mobility management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| Handover Preparation (Required→Command) | RAN→AMF | ✅ (N2) | ⬜ | transfer-IE bug already documented (interop) |
| Handover Resource Allocation (Request→Ack) | AMF→RAN | ✅ | — | ORBIT admits/rejects |
| Handover Notification | RAN→AMF | ✅ | ⬜ | |
| Path Switch Request | RAN→AMF | ✅ (Xn) | 🎯 malformed transfer / unknown IDs | |
| Handover Cancellation | RAN→AMF | ⬜ | ⬜ | |
| Uplink/Downlink RAN Status Transfer | both | ⬜ | ⬜ | |

### Lower-priority families (enumerate, then prioritise)
| Family | Dir | Status | Notes |
|---|---|---|---|
| Paging | AMF→RAN | — | ORBIT receives; timing angle only |
| Location Reporting (Control / Report / Failure) | mixed | ⬜ | Report is RAN→AMF |
| UE Radio Capability (Info Indication / Check) | mixed | ⬜ | Info Indication is RAN→AMF |
| Trace (Start / Deactivate / Failure / Cell Traffic Trace) | mixed | ⬜ | low value for core conformance |
| Warning / PWS (Write-Replace / Cancel / Restart / Failure) | mixed | ⬜ | low value |
| Configuration Transfer (RAN/AMF, SON) | both | ⬜ | low value |
| NRPPa transport (UE / non-UE associated) | both | ⬜ | positioning; low value |
| Secondary RAT Data Usage Report | RAN→AMF | ⬜ | |

## Current coverage

**1 check today** — `NGAP-UNKNOWN-UE-SURVIVES` (negative-ie): UE-associated
message for an unestablished UE-NGAP-ID pair → crash-safety (clause-10 AP-ID
handling). Live: PASS on the SD-Core testbed.

**Highest-value next batch** (the first authoring-cycle session):
1. **NG Reset** (procedural) — RAN→AMF reset → expect NG Reset Acknowledge.
2. **RAN Configuration Update, malformed IE** (negative-ie) — clause-10 reject path.
3. **GTP-U unknown-TEID → Error Indication** (gtpu) — TS 29.281 §7.3.1 is a genuine "shall", so a *real* FAIL is possible; runs from the RAN node. (Tracked here; GTP-U gets its own matrix below as it grows.)
4. **NAS replay** (security) — TS 33.501; repeat a secured NAS message → expect rejection.

## Other interfaces (matrices to grow)

Separate sections/files as coverage extends beyond NGAP:

- **XnAP** — TS 38.423 (Xn handover already exercised on the happy path).
- **NAS-5GS** — TS 24.501 (its own procedures + error-handling clause; security ties to TS 33.501).
- **GTP-U** — TS 29.281 (§7 error handling: unknown-TEID → Error Indication, End Marker, Echo).
- **PFCP / N4** — TS 29.244 (only if ORBIT ever drives N4 directly; today the core owns it).
