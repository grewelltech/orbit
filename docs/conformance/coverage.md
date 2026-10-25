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

- **The suite is release-*aware*, not multi-release-exhaustive.** Every check
  names its exact clauses with their provenance in code (`SpecRefs()`: spec,
  clause, strength, since, the versions read in the 3GPP archive through
  Rel-19, and the releases whose wording changed), and a check without verified
  provenance cannot merge. The generated table is [checks.md](checks.md); the
  quoted normative text per check is in [provenance/](provenance/).
  `--target-release` SKIPs a check that requires a newer release than the core
  claims.
- **Baseline: match the core under test.** ORBIT's baseline tracks the release
  the DUT implements. For SD-Core that is Rel-18 (NGAP, NAS) with PFCP at
  Rel-16, the default target; the evidence and caveats are in
  [target-release.md](target-release.md).
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
| NG Setup | RAN→AMF | ✅ (used everywhere) | ✅ `NGAP-NGSETUP-MISSING-TALIST` (live PASS: NG SETUP FAILURE) · ⬜ bad PLMN / bad TAC | the densest entry point |
| RAN Configuration Update | RAN→AMF | ⬜ | 🎯 malformed IE (high-value set) | |
| NG Reset | RAN→AMF | ✅ `NGAP-NG-RESET-ACK` (live PASS) | ⬜ | RAN-initiated reset-all → NG RESET ACKNOWLEDGE |
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

**6 checks today** (the first four live on the SD-Core testbed):
- `NGAP-UNKNOWN-UE-SURVIVES` (negative-ie, PASS) — UE-associated message for an
  unestablished UE-NGAP-ID pair → crash-safety (clause-10 AP-ID handling).
- `NGAP-NG-RESET-ACK` (procedural, PASS) — RAN→AMF NG RESET (reset-all) → the AMF
  completes the Reset procedure with NG RESET ACKNOWLEDGE (§8.7.4, a genuine
  "shall"). Wire-confirmed the stimulus went out as a well-formed
  InitiatingMessage/NGReset; reply decoded as NGResetAcknowledge.
- `NGAP-NGSETUP-MISSING-TALIST` (negative-ie, PASS) — NG Setup with the mandatory
  Supported TA List (id-102) removed → the AMF **rejects with NG SETUP FAILURE**
  (spec-ideal). Confirms the core does not accept a setup missing a mandatory IE.
- `GTPU-UNKNOWN-TEID-ERRIND` (gtpu, FAIL-class) — G-PDU with a non-zero
  unknown TEID → TS 29.281 §7.3.1 says the UPF **shall** return an Error
  Indication; SD-Core's BESS-UPF returns none. Wire-confirmed (G-PDU reached the
  UPF, nothing back), so it scores FAIL; a lab that tolerates it lists it on
  its expected-failures list. See interop/sdcore.md.
- `GTPU-END-MARKER-ON-PATH-SWITCH` (mobility, FAIL-class) — a UE with a PDU
  session hands over by Xn; the core must send a GTP-U End Marker on the old
  F-TEID to the source gNB, and no G-PDU on that tunnel after it (TS 23.501
  5.8.2.9.1, TS 29.281 7.3.2). Needs a UE (`--supi`, `--ki/--opc`,
  `--gnb-n3`), else SKIP. Measured on Ref 2 (SD-Core images amf rel-3.3.2,
  smf rel-4.3.3, upf-bess rel-2.6.3; Orbit c89f23b; 2026-10-12):
  - **FAIL** with the UPF's `enable_end_marker` off, which is the chart and
    OnRamp default. The SMF sets SNDEM on the path switch, and no End Marker
    arrives.
  - **PASS** with it on: one End Marker on the old TEID and no G-PDU after
    it, with 488 downlink G-PDUs on the old path before the Path Switch
    Request Acknowledge and 2,470 on the new path after it.

  A separate Xn handover run at 100 echoes/s with the flag on (not the
  conformance check) delivered 2,913 of 2,913, with the End Marker 11.5 ms
  after the last old-path G-PDU. Evidence for both runs is on #126. Under the verdict rule, a lab that runs with it off
  lists `no End Marker on the old F-TEID` on its expected-failures list
  ([expected-failures.md](expected-failures.md)).

- `NAS-T3560-AUTH-RETRANSMIT` (nas, FAIL-class) — the UE withholds its
  Authentication Response; the AMF must retransmit the Authentication
  Request on each T3560 expiry, four times, and on the fifth expiry release
  the N1 NAS signalling connection, a UE Context Release Command on N2
  (TS 24.501 5.4.1.3.7 b)). The timer is estimated from the observed gaps
  (at least the 6 s default; up to 1.5 × 24 s for the first), so a core
  configured slower still passes; the spacing is recorded, not asserted.
  Needs a UE, else SKIP, and up to 190 s (about 33 s at the default), so it
  raises its own per-test timeout. Not yet run on Ref 2;
  the source analyst predicts PASS for the retransmissions and FAIL for the
  release (the AMF removes the UE locally without a release command).

**Verdict note:** every missed **shall** is FAIL, including one that leaves the
core running; `DEVIATION` is reserved for a missed **should**. A known failure
that a lab tolerates goes on that core's expected-failures list, which keeps it
FAIL in the report but out of the gate. See
[expected-failures.md](expected-failures.md).

**Highest-value next batch** (remaining):
1. **NAS replay** (security) — TS 33.501; repeat a secured NAS message → expect rejection.

> **Tooling note:** the testbed has no tshark, so wire disambiguation currently
> uses a byte-level PPID scan that reads uplink cleanly but misses downlink
> responses; response identity is confirmed via ORBIT's free5gc decode of the
> received bytes. For checks where a FAIL is possible (e.g. the GTP-U "shall"),
> a proper SCTP/NGAP pcap decoder (or tshark) would strengthen disambiguation.

## Other interfaces (matrices to grow)

Separate sections/files as coverage extends beyond NGAP:

- **XnAP** — TS 38.423 (Xn handover already exercised on the happy path).
- **NAS-5GS** — TS 24.501 (its own procedures + error-handling clause; security ties to TS 33.501). ✅ T3560 Authentication Request retransmission and release (`NAS-T3560-AUTH-RETRANSMIT`); ⬜ T3550 Registration Accept retransmission (#138).
- **GTP-U** — TS 29.281 (§7 error handling). ✅ unknown-TEID → Error Indication (`GTPU-UNKNOWN-TEID-ERRIND`, FAIL on SD-Core); ✅ End Marker on Xn path switch (`GTPU-END-MARKER-ON-PATH-SWITCH`); ⬜ Echo Request/Response.
- **PFCP / N4** — TS 29.244 (only if ORBIT ever drives N4 directly; today the core owns it).
