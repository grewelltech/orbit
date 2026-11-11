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
| NG Setup | RAN→AMF | ✅ `NGAP-NG-SETUP` (owner of the NG Setup; INFO when accepted, WARN when rejected, DEVIATION when unanswered) | ✅ `NGAP-NGSETUP-MISSING-TALIST` (live PASS: NG SETUP FAILURE) · ✅ unknown PLMN `NGAP-NGSETUP-UNKNOWN-PLMN` · ✅ duplicate Global RAN Node ID `NGAP-DUPLICATE-RAN-NODE-ID` (characterisation) · ⬜ bad TAC | the densest entry point |
| RAN Configuration Update | RAN→AMF | ✅ `NGAP-RAN-CONFIG-UPDATE-KEEPS-TA` (an absent Supported TA List keeps the TAs) · ✅ `NGAP-RAN-CONFIG-UPDATE-ACK` (characterisation: is a valid update acknowledged?) | ✅ `NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX` (transfer syntax error) | every IE but Message Type is optional, so there is no missing-mandatory-IE case |
| NG Reset | RAN→AMF | ✅ `NGAP-NG-RESET-ACK` (live PASS) | ⬜ | RAN-initiated reset-all → NG RESET ACKNOWLEDGE |
| Error Indication | both | ✅ (we assert the core *sends* it) | — | |
| AMF Configuration Update | AMF→RAN | — | — | ORBIT receives |
| Overload Start / Stop | AMF→RAN | — | — | ORBIT receives |
| AMF Status Indication | AMF→RAN | — | — | ORBIT receives |

### NAS transport
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| Initial UE Message | RAN→AMF | ✅ `NAS-INITIAL-REGISTRATION` (owner of the registration; PASS when accepted, WARN when rejected, FAIL when unanswered or released) · ✅ `NAS-REGISTRATION-AFTER-RELEASE` (a registration right after the same UE's release) | 🎯 malformed / oversized NAS PDU, bad TAI | |
| Uplink NAS Transport | RAN→AMF | ✅ | ✅ `NGAP-UNKNOWN-UE-SURVIVES` (unknown UE-NGAP-ID pair) | our first check; clause-10 AP-ID case |
| Downlink NAS Transport | AMF→RAN | — | — | timing angle only |
| NAS Non Delivery Indication | RAN→AMF | ⬜ | ⬜ | |
| Reroute NAS Request | AMF→RAN | — | — | |

### UE context management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| Initial Context Setup | AMF→RAN | ✅ (attach; with the Service Accept in `NAS-SERVICE-REQUEST-REACTIVATES`) | — | timing angle (bad/late response) |
| UE Context Release Request | RAN→AMF | ✅ `NGAP-UE-CTX-RELEASE-ON-REQUEST` (DEVIATION-class); also the AN release of `NGAP-PAGING-ON-DL-DATA` (its failure is an ERROR, not a verdict) | ⬜ | 8.3.2.2 is a "should" |
| UE Context Release (Command/Complete) | AMF→RAN | ✅ `NGAP-CTX-RELEASE-AFTER-SWITCH-OFF` (UE NGAP IDs of the release after a switch-off) | — | the release after a deregistration is descriptive (TS 23.502), so its absence is WARN (availability), not FAIL |
| UE Context Modification | AMF→RAN | ⬜ | — | timing angle |

### PDU session management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| PDU Session Resource Setup | AMF→RAN | ✅ (attach + data) | — | timing angle |
| PDU Session Resource Release | AMF→RAN | ✅ `NAS-PDU-RELEASE-UE-REQUESTED` (UE-requested release) | — | |
| PDU Session Resource Modify | AMF→RAN | ⬜ | — | |
| PDU Session Resource Notify | RAN→AMF | ⬜ | ⬜ | |
| PDU Session Resource Modify Indication | RAN→AMF | ⬜ | 🎯 | RAN-initiated → good negative surface |

### Mobility management
| Procedure | Dir | Procedural | Negative-IE | Notes |
|---|---|---|---|---|
| Handover Preparation (Required→Command) | RAN→AMF | ✅ (N2) · ✅ `NGAP-HANDOVER-COMMAND-TRANSFER` (owner of the N2 handover preparation; Handover List with a transfer per item) | ⬜ | transfer-IE bug already documented (interop); the check runs under strict-3gpp |
| Handover Resource Allocation (Request→Ack) | AMF→RAN | ✅ | — | ORBIT admits/rejects |
| Handover Notification | RAN→AMF | ✅ | ⬜ | |
| Path Switch Request | RAN→AMF | ✅ (Xn) · ✅ `NGAP-PATH-SWITCH-ACK-TRANSFER` (owner of the Xn path switch; Acknowledge with a transfer per switched session) · ✅ `NGAP-PATH-SWITCH-SECCAP-MISMATCH` (stored UE security capabilities returned on a mismatch) | 🎯 malformed transfer / unknown IDs | |
| Handover Cancellation | RAN→AMF | ✅ `NGAP-HANDOVER-CANCEL-ACK` (characterisation: is the cancel acknowledged?) | ⬜ | no "shall" or "should" for the acknowledge in 8.4.5 |
| Uplink/Downlink RAN Status Transfer | both | ⬜ | ⬜ | |

### Lower-priority families (enumerate, then prioritise)
| Family | Dir | Status | Notes |
|---|---|---|---|
| Paging | AMF→RAN | ✅ `NGAP-PAGING-ON-DL-DATA` (downlink data to a CM-IDLE UE → PAGING with its 5G-S-TMSI) | needs an N6 responder as the downlink trigger; the TAI List for Paging is recorded only |
| Location Reporting (Control / Report / Failure) | mixed | ⬜ | Report is RAN→AMF |
| UE Radio Capability (Info Indication / Check) | mixed | ⬜ | Info Indication is RAN→AMF |
| Trace (Start / Deactivate / Failure / Cell Traffic Trace) | mixed | ⬜ | low value for core conformance |
| Warning / PWS (Write-Replace / Cancel / Restart / Failure) | mixed | ⬜ | low value |
| Configuration Transfer (RAN/AMF, SON) | both | ⬜ | low value |
| NRPPa transport (UE / non-UE associated) | both | ⬜ | positioning; low value |
| Secondary RAT Data Usage Report | RAN→AMF | ⬜ | |

## Current coverage

**37 checks today** (the first four live on the SD-Core testbed):
- `NGAP-UNKNOWN-UE-SURVIVES` (negative-ie, PASS) — UE-associated message for an
  unestablished UE-NGAP-ID pair → crash-safety (clause-10 AP-ID handling).
- `NGAP-NG-RESET-ACK` (procedural, PASS) — RAN→AMF NG RESET (reset-all) → the AMF
  completes the Reset procedure with NG RESET ACKNOWLEDGE (§8.7.4, a genuine
  "shall"). Wire-confirmed the stimulus went out as a well-formed
  InitiatingMessage/NGReset; reply decoded as NGResetAcknowledge.
- `NGAP-NGSETUP-MISSING-TALIST` (negative-ie, PASS) — NG Setup with the mandatory
  Supported TA List (id-102) removed → the AMF **rejects with NG SETUP FAILURE**
  (spec-ideal). Confirms the core does not accept a setup missing a mandatory IE.
  NG SETUP FAILURE is PASS; acceptance, another PDU, no answer within 4 s or a
  reset is FAIL (TS 38.413 10.3.5); a core that then stops completing NG Setup
  is CRASH.
- `NGAP-NGSETUP-UNKNOWN-PLMN` (negative-ie, FAIL-class) — NG Setup naming only
  a PLMN the AMF does not serve (999/99, with a TAC distinct from the served
  one) → the AMF **shall** reject it (TS 38.413 8.7.1.4); NG SETUP FAILURE is
  PASS, with the cause recorded ("Unknown PLMN or SNPN" expected). Acceptance,
  another PDU, a silent drop or a reset is FAIL; a core that serves the
  stimulus PLMN is ERROR, and one that then stops completing NG Setup is
  CRASH. It never presents a served
  PLMN with an unknown TAC, which an AMF rejects with the same cause. Predicted
  PASS on SD-Core from source; not yet run live.
- `NGAP-RAN-CONFIG-UPDATE-KEEPS-TA` (procedural, FAIL-class) — after NG
  Setup, a RAN CONFIGURATION UPDATE carrying only the RAN Node Name; the UE
  then registers over the same association. An IE the update does not
  include leaves that configuration unchanged (TS 38.413 8.7.2.2, a shall
  from V15.3.0), so after a RAN CONFIGURATION UPDATE ACKNOWLEDGE the AMF
  must still serve the NG Setup tracking area: a registration is PASS, a
  Registration Reject with #12, #13 or #15 is FAIL, any other registration
  failure is BLOCKED on the registration. A RAN CONFIGURATION UPDATE FAILURE
  is SKIP (8.7.2.3 permits it), and no outcome within 5 s or a lost
  association is WARN (availability). Needs a UE, else SKIP. Not yet run on Ref 2.
- `NGAP-DUPLICATE-RAN-NODE-ID` (procedural, DEVIATION-class) — with one
  association up after NG Setup, a second association from another N2 source
  address sends an NG SETUP REQUEST with the same Global RAN Node ID. TS
  38.413 is silent on the duplicate (8.7.1.2 only describes the NG SETUP
  RESPONSE; 9.3.1.5 only says the ID identifies a node): an acceptance or a
  rejection with the first association kept is INFO, either with the first
  association dropped is WARN (availability). No answer, another PDU or a
  reset of the new association is a DEVIATION from the should of 8.7.1.3,
  Observed naming both halves. Needs a second address in the target's
  `ran.n2_source_ips`, else SKIP. A core that does not survive (a fresh NG
  Setup afterwards fails) is CRASH. Which association the core then uses for
  the ID is not examined. Not yet run on Ref 2.
- `NGAP-RAN-CONFIG-UPDATE-ACK` (procedural, WARN-class) — after NG Setup, a
  valid RAN CONFIGURATION UPDATE restating the Supported TA List the NG Setup
  presented. TS 38.413 8.7.2.2 describes the answer ("The AMF responds with a
  RAN CONFIGURATION UPDATE ACKNOWLEDGE message") without requiring it, and
  8.7.2.3 requires a FAILURE only of an AMF that cannot accept the update, so
  no requirement is graded: an ACKNOWLEDGE is INFO; a RAN CONFIGURATION
  UPDATE FAILURE, an ERROR INDICATION, no answer within 5 s or a lost
  association is WARN (availability), with a fresh NG Setup tried
  afterwards: a core that no longer completes it did not survive and is
  CRASH. Not yet run on Ref 2.
- `NGAP-RAN-CONFIG-UPDATE-TRANSFER-SYNTAX` (negative-ie, DEVIATION-class) — a
  RAN CONFIGURATION UPDATE whose Supported TA Item is cut off after its TAC,
  so the item's mandatory Broadcast PLMN List is missing: a transfer syntax
  error, for which the receiver **should** initiate the Error Indication
  procedure (TS 38.413 10.2). An ERROR INDICATION is PASS. A silent drop,
  another PDU, or a dropped association with the core still running is
  DEVIATION; a core that stops completing NG Setup is CRASH. It cites a
  should and no shall, as `NGAP-UE-CTX-RELEASE-ON-REQUEST` does. Not yet run on Ref 2.
- `GTPU-UNKNOWN-TEID-ERRIND` (gtpu, FAIL-class) — G-PDU with a non-zero
  unknown TEID → TS 29.281 §7.3.1 says the UPF **shall** return an Error
  Indication; SD-Core's BESS-UPF returns none. Wire-confirmed (G-PDU reached the
  UPF, nothing back), so it scores FAIL; a lab that tolerates it lists it on
  its expected-failures list. See interop/sdcore.md.
- `GTPU-ECHO-RESPONSE` (gtpu, FAIL-class) — a GTP-U Echo Request to the
  UPF's N3 address, with no session → the UPF **shall** reply with an Echo
  Response (TS 29.281 7.2.1, 7.2.2) that copies the request's sequence number
  (4.3.1). Up to three requests, each with its own sequence number, so one
  lost datagram is not a FAIL. No response, or a response with a sequence
  number that was not sent, is FAIL. It owns the GTP-U Echo:
  `GTPU-UNKNOWN-TEID-ERRIND` is BLOCKED when the UPF does not answer its Echo
  before the G-PDU. Silence cannot tell a UPF without Echo
  handling from one `--upf-n3` does not reach. Needs `--upf-n3` and
  `--n3-bind`, else SKIP. Not yet run on Ref 2.
- `NGAP-PAGING-ON-DL-DATA` (procedural, FAIL-class) — a UE with a PDU session is
  released to CM-IDLE by an AN release, a responder on N6 sends downlink data
  to its address, and the core must send NGAP PAGING naming the UE's 5G-S-TMSI
  within 10 s (TS 24.501 5.6.2.2.1, a "shall"; TS 23.502 4.2.3.3 step 4b and
  TS 38.413 8.5.1.2 describe it). Needs a UE and a target responder as the
  downlink trigger; SKIPs without either. Not yet run live.
- `NAS-SERVICE-REQUEST-REACTIVATES` (procedural, FAIL-class) — a UE with a PDU
  session is released to CM-IDLE by an AN release and sends a Service Request
  with the session in its Uplink data status; the core must answer Service
  Accept and re-establish the session's user plane: the N3 tunnel set up
  again on N2, and downlink data from a responder on N6 arriving on the gNB's
  new tunnel (TS 24.501 5.6.1.4.1, 5.6.1.4 in Rel-15, a "shall"; TS 23.502
  4.2.3.2 describes it). A Service Reject, no answer, an Accept without the
  tunnel and a tunnel without downlink all FAIL. The same downlink is sent
  once before the release; when it does not arrive the run is an ERROR, not
  a verdict. Needs a UE and a target responder; SKIPs without either. Not yet
  run live.
- `N3-DL-QFI-CONTAINER` (gtpu, FAIL-class) — a UE with a PDU session sends
  to the `--traffic` echo peer, and every downlink G-PDU that comes back on
  N3 must carry the PDU Session Container extension header (TS 29.281
  5.2.2.7, a shall from V15.5.0; "may" in V15.2.0 to V15.4.0), whose DL PDU
  SESSION INFORMATION frame has the QFI (TS 38.415 5.4.1.1, 5.5.2.1). Up to
  50 G-PDUs are sampled within 3 s; one without the container is FAIL. The
  QFI values are recorded. Needs a UE and `--traffic`, else SKIP; an echo
  peer that returns nothing is ERROR. Not yet run on Ref 2.
- `GTPU-END-MARKER-ON-PATH-SWITCH` (mobility, FAIL-class) — a UE with a PDU
  session hands over by Xn; the core must send a GTP-U End Marker on the old
  F-TEID to the source gNB, and no G-PDU on that tunnel after it (TS 23.501
  5.8.2.9.1, TS 29.281 7.3.2). A path switch the core does not acknowledge
  is BLOCKED on the Xn path switch. Needs a UE (`--supi`, `--ki/--opc`,
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

- `NGAP-PATH-SWITCH-ACK-TRANSFER` (mobility, FAIL-class) — a UE with a PDU
  session hands over by Xn; the AMF must answer the Path Switch Request with
  a PATH SWITCH REQUEST ACKNOWLEDGE (TS 38.413 8.4.4.2) whose Switched List
  carries a Path Switch Request Acknowledge Transfer for the session
  (9.2.3.9). UL NG-U UP TNL Information inside the transfer is optional from
  V15.1.0 (9.3.4.9) and never required: absent, it means the uplink endpoint
  is unchanged, so with `--traffic` the check also expects an echo of uplink
  sent after the switch, and WARNs (availability) without one when the peer
  answered before the switch. Without `--traffic` the uplink is recorded as
  not probed. A missing Switched List or transfer is FAIL (9.1.1: both are
  mandatory). No answer within 5 s, or another one, is FAIL: 8.4.4.2 and
  8.4.4.3 require the Acknowledge or the PATH SWITCH REQUEST FAILURE. The
  FAILURE itself is WARN (availability). The check owns the Xn path switch.
  Needs a UE, else SKIP. Not yet run on Ref 2.

- `NGAP-HANDOVER-COMMAND-TRANSFER` (mobility, FAIL-class) — a UE with a PDU
  session hands over by N2; for an intra-system handover the HANDOVER
  COMMAND must carry the PDU Session Resource Handover List (TS 38.413
  8.4.1.2, a shall from V15.5.0), each item with a Handover Command Transfer
  (9.2.3.2) that decodes. Nothing inside the transfer is asserted (every IE
  of 9.3.4.10 is optional), and a session missing from the list is recorded
  only. The target gNB sends the conformant Handover Request Acknowledge
  Transfer (strict-3gpp, never the `sdcore` profile), so the check measures
  what [interop/sdcore.md](../interop/sdcore.md) Finding 1 does to the
  command. A HANDOVER PREPARATION FAILURE, no answer within 8 s, or another
  answer is WARN (availability): no shall obliges the AMF to prepare the
  handover. The check owns the N2 handover preparation. Needs a UE, else
  SKIP. Not yet run on Ref 2.

- `NGAP-UE-CTX-RELEASE-ON-REQUEST` (procedural, DEVIATION-class) — a
  registered UE's gNB sends a UE CONTEXT RELEASE REQUEST, cause radio network
  "release due to NG-RAN generated reason" (never "User inactivity", which
  from V18.1.0 makes the release conditional). A UE CONTEXT RELEASE COMMAND
  within 5 s is PASS; none is a DEVIATION, never a FAIL, since TS 38.413
  8.3.2.2 says "should". A released UE is still registered, so the check
  deregisters it with an initial NAS message on a new NG connection; that
  needs the 5G-GUTI, and the detail says how the cleanup went. Needs a UE,
  else SKIP. Not yet run on Ref 2.

- `NGAP-HANDOVER-CANCEL-ACK` (mobility, WARN-class) — a UE with a PDU
  session has an N2 handover prepared up to the HANDOVER COMMAND, and the
  source gNB then sends HANDOVER CANCEL. TS 38.413 8.4.5 has no "shall" or
  "should" for the AMF: the acknowledge is the response of a class 1
  procedure (Table 8.1-1), 9.2.3.12 says what it confirms, and 8.4.5.4 has
  the source take a missing one as success. So the check is a
  characterisation: a HANDOVER CANCEL ACKNOWLEDGE within 5 s and none are
  both INFO; a dropped source association is WARN (availability). A
  handover the core does not prepare is BLOCKED on the N2 handover
  preparation. Whether the AMF released the context prepared at the target
  is recorded. A fresh NG Setup is tried afterwards; a core that no longer
  completes it did not survive: CRASH. The UE stays on its source gNB.
  Needs a UE, else SKIP. Not yet run on Ref 2.

- `NAS-T3560-AUTH-RETRANSMIT` (nas, FAIL-class) — the UE withholds its
  Authentication Response; the AMF must retransmit the Authentication
  Request on each T3560 expiry, four times, and on the fifth expiry release
  the N1 NAS signalling connection, a UE Context Release Command on N2
  (TS 24.501 5.4.1.3.7 b)). The timer is estimated from the observed gaps
  (at least the 6 s default; up to 1.5 × 24 s for the first), so a core
  configured slower still passes; the spacing is recorded, not asserted.
  An Authentication Reject after the four retransmissions is recorded and
  the check keeps waiting for the release, which alone decides (the clause
  names no reject); a reject after a request and before them is FAIL, one
  in place of the request, or no request, BLOCKED on the registration. Needs a UE, else SKIP,
  and up to 190 s (about 33 s at the default), so it
  raises its own per-test timeout. Not yet run on Ref 2;
  the source analyst predicts PASS for the retransmissions and FAIL for the
  release (the AMF removes the UE locally without a release command).
- `NAS-T3550-ACCEPT-RETRANSMIT` (nas, FAIL-class) — the UE withholds its
  Registration Complete after a Registration Accept that assigns a 5G-GUTI;
  the AMF must retransmit the Registration Accept on each T3550 expiry, four
  times, and abort on the fifth (TS 24.501 5.5.1.2.8 c), with T3550 started
  per 5.5.1.2.4). No release is required on the abort, so any release is
  recorded only. An Accept without a 5G-GUTI starts no T3550, and the check
  reports INFO; a reject or no Accept is BLOCKED on the registration. The timer is estimated from the gaps as for T3560 (up to 1.5 × 18 s
  for the first). Needs a UE and up to 145 s (about 33 s at the default).
  Not yet run on Ref 2; the source analyst predicts PASS.
- `NAS-PDU-UNKNOWN-DNN` (nas, FAIL-class) — the UE requests a PDU session
  for DNN `orbit-unknown-dnn` on the run's slice. A PDU Session
  Establishment Reject with any 5GSM cause passes (TS 24.501 6.4.1.4.1; #27
  is only "typical"), and so does the un-forwarded request returned in a DL
  NAS Transport with 5GMM cause #90/#91 (5.4.5.2.5 a) 1), required from
  V15.1.0; 5.4.5.3.2 for case e) of 5.4.5.3.1). #90/#91 with any other
  payload, another answer, or nothing within 10 s is FAIL. A core that
  accepts the DNN, or returns the request for congestion (#22/#67/#69,
  case f), allowed from V16.3.0), SKIPs. The answer is read
  through the UE's NAS security context, and the UE is deregistered after.
  An AMF-built Reject without a 5GMM cause cannot be told from an SMF's.
- `NAS-REG-REJECTED-NSSAI` (nas, FAIL-class, Rel-16 onward) — the UE
  registers requesting only a stimulus S-NSSAI the core does not serve (the
  run's SST with SD EEEEEE), with the NSSAA and ER-NSSAI bits sent as 0.
  TS 24.501 5.5.1.2.5 (condition b) 2)) then requires a Registration Reject
  with 5GMM cause #62 (from V16.2.0) whose Rejected NSSAI IE lists the
  stimulus (from V16.6.0). Another cause, a #62 Reject without the
  stimulus, or the stimulus only in the Extended rejected NSSAI IE (from
  V17.1.0) is FAIL; so is neither a Reject nor an Accept, or a release
  without one (5.5.1.2.4, 5.5.1.2.5). A #62 Reject with an IE Orbit cannot
  decode instead is ERROR. A core that allows a default S-NSSAI accepts, so
  the #62 rule did not apply: SKIP as a permitted alternative, likely on a
  core whose subscriber has a default slice. An Authentication Reject is
  BLOCKED on the registration.
  The Accept's rejected NSSAI is a may, recorded only. Not yet run on Ref 2;
  the source analyst predicts FAIL on EX-1.
  Needs a UE. Not yet run on Ref 2; the source analyst predicts FAIL (an
  AMF-built Reject returned with #91).

- `NAS-SMC-NO-NIA0-REPLAYED-CAPS` (security, FAIL-class) — three registrations
  with the Security Mode Complete withheld. A normal UE (5G-EA0/1/2,
  5G-IA0/1/2) must get a Security Mode Command that selects no NIA0 (TS 33.501
  5.5.2) and replays its capabilities exactly (6.7.2 step 1b). A UE offering
  only NIA0 and NIA3 may get NIA3, a reject, a release or no answer, never
  NIA0 or no command. A UE offering only NIA0, run last, expects a reject or
  a release. For any UE, a Security Mode Command selecting NIA0, or a
  Registration Accept with no Security Mode Command, is FAIL (TS 33.501
  5.5.2, 6.7.2 step 1b). A normal UE the core rejects, releases or leaves
  unanswered before the command is BLOCKED on the registration. A non-null algorithm the
  UE did not offer is PASS, recorded, not scored: no shall requires the
  selected algorithm to be one the UE offered at initial registration. 5.11.2
  b) says only 'dependent on', and 6.7.1.2 (from V19.4.0) covers AMF change.
  After a Registration Accept without NAS security the UE is deregistered in
  the clear, and a UE Context Release is requested if the core sends none; the
  cleanup is in the detail. Each UE is judged separately; the first FAIL
  decides, else the first BLOCKED or ERROR. The selected algorithms are
  recorded. Needs a UE, else SKIP. Not yet
  run on Ref 2; the source analyst predicts the capabilities replay PASSes,
  and the NIA0-only UE is predicted to FAIL on EX-1.
- `NAS-INTEGRITY-FAIL-DISCARD` (security, FAIL-class) — after the security
  mode control procedure the UE sends a valid PDU Session Establishment
  Request (which must be answered), a second one with a corrupted MAC, and a
  third, valid one. The AMF shall discard the second (TS 24.501 4.4.4.3,
  TS 33.501 6.4.3.3): an answer to it, a 5GMM STATUS after it, or a third
  request left unanswered is FAIL. A UE released after the bad MAC is WARN
  for security (neither a discard nor an answer); a first request left
  unanswered is BLOCKED on the PDU session; a core that selects 5G-IA0 SKIPs. The
  sessions are released and the UE deregistered at the end, best effort.
  Needs a UE and a DNN the core serves. Not yet run on a live core.
- `NAS-REPLAY-DISCARD` (security, FAIL-class) — after the security mode
  control procedure the UE sends a valid PDU Session Establishment Request
  (which must be answered), the same NAS PDU again, and a second, valid
  request. The repeat carries the NAS COUNT the AMF accepted last, which it
  shall accept only once (TS 33.501 6.4.3.2, TS 24.501 4.4.3.2): a further
  answer to the first request, a 5GMM STATUS after the repeat, or a second
  request left unanswered is FAIL. A UE released after the repeat is WARN
  for security; a first request left unanswered is BLOCKED on the PDU
  session; a core that selects 5G-IA0 SKIPs. Cleanup as for
  `NAS-INTEGRITY-FAIL-DISCARD`. Needs a UE and a DNN the core serves. Not
  yet run on a live core.
- `NAS-COUNT-NOT-ADVANCED-ON-MAC-FAIL` (security, FAIL-class) — the UE sends
  two valid PDU Session Establishment Requests, A and B (both must be
  answered), then A's bytes again, then a valid request C. The re-sent A has
  a sequence number lower than the last accepted one, so it fails the MAC at
  the AMF's estimated NAS COUNT, and the stored COUNT shall not move
  (TS 24.501 4.4.3.3). C answered is PASS; C unanswered with the association
  up is FAIL; an answer to the re-sent A is FAIL under 4.4.3.2. A or B
  unanswered, or a release before C is answered, is ERROR; 5G-IA0 SKIPs. On
  the FAIL path the UE's security context is unusable, so the cleanup's
  release and deregistration may not be accepted: the association is
  dropped, and the core may keep a half-registered UE until its timers
  expire. Mock-only so far.
- `NAS-RETRANSMIT-NEW-COUNT` (security, FAIL-class) — the stimulus of
  `NAS-T3550-ACCEPT-RETRANSMIT`: the UE withholds its Registration Complete.
  Each retransmitted Registration Accept must be protected at a NAS COUNT
  higher than the transmission before it (TS 24.501 4.4.3.1); a
  byte-identical resend or a lower COUNT is FAIL. The COUNT is the one of two
  candidates (the sequence number with the stored overflow counter, or the
  next) the MAC verifies at, so a wrap counts as an increase; a
  retransmission that verifies at neither is ERROR. No retransmission SKIPs
  (the T3550 check grades that). Needs a UE and up to 118 s (about 25 s at
  the default timer). The UE is left registered, as by the T3550 check.
  Mock-only so far.
- `NAS-BADMAC-REGISTRATION-AFTER-SMC` (security, FAIL-class) — with one PDU
  session established, the UE sends a REGISTRATION REQUEST for mobility
  registration updating with a corrupted MAC on the secured connection.
  TS 24.501 4.4.4.3 can be read as "discard" or as "authenticate first", so
  an answer is FAIL only when both readings forbid it: a Registration Accept,
  Registration Reject, Security Mode Command or 5GMM STATUS with no
  authentication first. No answer or a release is PASS; an Authentication
  Request (or an Identity Request) is SKIP, with the ambiguity in Observed.
  No PDU session is ERROR. The session is released and the UE deregistered
  best effort; after a re-authentication the core may keep the UE until its
  timers expire. Mock-only so far.

- `NAS-INITIAL-MSG-CONTAINER-WINS` (security, FAIL-class) — the initial
  REGISTRATION REQUEST carries, in the clear, a Requested NSSAI the core does
  not serve, and the SECURITY MODE COMPLETE's NAS message container carries
  the complete request with the served S-NSSAI. The AMF shall complete the
  registration on the container's request (TS 24.501 5.4.2.4; TS 33.501
  6.4.6 step 4). Two conformant reference registrations, one per S-NSSAI,
  show how this core answers each value: the stimulus answered as the served
  one is PASS; any other answer, or none, is FAIL, named as the unprotected
  value's when it is that one. INFO when the core refuses or drops the
  initial message before a SECURITY MODE COMMAND (the container is never
  consulted) or answers both references alike; WARN (security) when it
  registers the UE on the unprotected initial message with no SECURITY MODE
  COMMAND. A reference the core does not complete is BLOCKED on the
  registration. Needs a UE; up to 52 s. Each registration
  uses its own subscriber where the pool gives the check three, and each
  accepted one is deregistered.
- `NAS-INITIAL-MSG-NO-CONTAINER` (security, characterisation, WARN at
  worst) — the initial message has non-cleartext IEs in the clear and the
  SECURITY MODE COMPLETE has no NAS message container. No clause says what
  the AMF then does: an Accept, or a Reject with #62 for the Requested NSSAI
  only the unprotected message carried, acts on unprotected input and is
  WARN (security); a second SECURITY MODE COMMAND with the RINMR bit (the
  request TS 24.501 5.4.2.2 describes for other conditions) or without it,
  another Reject, an early refusal, a release and no answer are INFO. Needs
  a UE.
- `NAS-INITIAL-MSG-NONCLEARTEXT-ACCEPTED` (security, characterisation,
  WARN at worst) — the complete REGISTRATION REQUEST, non-cleartext IEs
  included, is sent as the initial message and again in the container.
  TS 24.501 4.4.6 binds the UE only: an accepted registration, a
  Registration Reject (cause and whether it came before the SECURITY MODE
  COMMAND recorded), a release and no answer are INFO; a registration with
  no SECURITY MODE COMMAND at all is WARN (security). Needs a UE.
- `NAS-SMC-IMEISV-REQUEST` (security, characterisation, WARN at worst) —
  records whether the SECURITY MODE COMMAND requests the IMEISV (a may,
  TS 24.501 5.4.2.2) and, the UE having left a requested IMEISV out of the
  SECURITY MODE COMPLETE, what the core does. An Accept without the IMEISV
  it asked for is WARN (security: the equipment identity check it asked for
  is skipped); no request (the omission is then not exercised), an
  IDENTITY REQUEST, a Reject and the rest are INFO. A registration that
  never reaches a SECURITY MODE COMMAND is BLOCKED. Needs a UE.

- `NGAP-PATH-SWITCH-SECCAP-MISMATCH` (security, FAIL-class) — a UE with a
  PDU session is handed over by Xn with a PATH SWITCH REQUEST whose UE
  Security Capabilities differ from the ones the AMF supplied in the INITIAL
  CONTEXT SETUP REQUEST (algorithm 3 inverted in both NR bitmaps). The AMF
  shall verify them against its stored copy and, on a mismatch, send the
  stored ones in the PATH SWITCH REQUEST ACKNOWLEDGE (TS 33.501 6.7.3.1). An
  Acknowledge carrying the supplied capabilities is PASS; one without the
  IE, one echoing the sent ones, and one with others are FAIL. No
  Acknowledge (a PATH SWITCH REQUEST FAILURE included) is WARN
  (availability): the shall governs the Acknowledge's content, not whether
  one is sent. A core that no longer completes NG Setup afterwards is CRASH. Needs a UE; up to 25 s.
  The UE is deregistered afterwards.

- `NAS-DEREG-ACCEPT-NOT-SWITCH-OFF` (procedural, FAIL-class) — the UE
  registers without a PDU session and sends a Deregistration Request that
  does not indicate switch-off; the AMF shall answer with a Deregistration
  Accept (TS 24.501 5.5.2.2.2). None within 15 s (one T3521), or a UE Context
  Release Command in its place, is FAIL; the check owns the deregistration,
  and a registration the core does not complete first is BLOCKED. The release after the Accept is
  described by TS 23.502 4.2.2.3.2, not required by a shall, so it is recorded
  only. A core that answers nothing is sent a switch-off deregistration
  before the association closes. Needs a UE, else SKIP. Not yet run on Ref 2.

- `NGAP-CTX-RELEASE-AFTER-SWITCH-OFF` (procedural, FAIL-class) — the UE
  registers without a PDU session and sends a switch-off Deregistration
  Request. A UE Context Release Command naming the connection's AMF and RAN
  UE NGAP IDs is PASS; one naming the UE by its AMF UE NGAP ID alone is FAIL
  (TS 38.413 8.3.3.2, both IDs "if available"). The release itself is only
  described (TS 23.502 4.2.2.3.2 step 8; nothing in TS 24.501 5.5.2.2.2), so
  no command for the connection within 5 s violates no requirement: WARN
  (availability), with the likely cause (the AMF leaves the N2 UE context)
  in the detail. Needs a UE, else SKIP. Not yet run on Ref 2.

- `NAS-INITIAL-REGISTRATION` (nas, FAIL-class) — the run's own UE
  registers, without a PDU session, and is deregistered. The check owns the
  registration: a check whose own registration the core does not complete
  is BLOCKED and names it. TS 24.501 5.5.1.2.4 and 5.5.1.2.5 give an initial
  registration request its answer: a Registration Accept is PASS; neither
  an Accept nor a Reject within 10 s, or a release without one, is FAIL. A
  Registration Reject or an Authentication Reject for the run's subscriber
  violates nothing and is WARN (availability); so is a registration that
  ended another way. Needs a UE; up to 20 s.

- `NAS-REGISTRATION-AFTER-RELEASE` (nas, FAIL-class) — the UE registers,
  deregisters with a switch-off and, at the core's UE Context Release
  Command, the gNB sends the UE Context Release Complete and at once a new
  initial Registration Request for the same SUPI, on the same association
  under a new RAN UE NGAP ID. Four attempts, with 0, 5, 10 and 20 ms between
  the two messages. The switch-off deregistration was complete when the AMF
  received it (TS 24.501 5.5.2.2.2), so the request is an ordinary initial
  registration, which the AMF shall answer with an Accept or a Reject
  (5.5.1.2.4, 5.5.1.2.5). Any NAS message for the new registration within
  10 s, such as the Authentication Request, is an answer: every attempt
  answered is PASS, one with no NAS message FAIL (5.5.1.2.8 read in six
  versions: only case e) 2), a second identical request, lets the AMF stay
  silent). A core that sends no release for a switch-off is INFO; a
  registration of the SUPI that does not complete is BLOCKED. The UE is deregistered at the end; the
  detail says what an unanswered registration left. Needs a UE (one
  subscriber); up to 124 s.

- `NAS-PDU-RELEASE-UE-REQUESTED` (procedural, FAIL-class) — the UE requests
  the release of its PDU session. The SMF shall perform the release (TS
  24.501 6.4.3.3), starting with a PDU Session Release Command (6.3.3.2),
  which may ride in the NGAP command or in a Downlink NAS Transport; with the
  user plane active its answer shall include the N2 release (TS 23.502
  4.3.4.2 step 3, from V15.5.0), seen as a PDU Session Resource Release
  Command listing the session (TS 38.413 8.2.2.2); and the UPF shall drop the
  session's packets (TS 23.502 4.3.4.2 step 2a). Both commands and no
  downlink G-PDU on the released tunnel from 1 s after them is PASS. Neither
  command within 16 s (one T3582), one without the other, or downlink after
  the grace is FAIL. A PDU Session Release Reject SKIPs. With `--traffic` the
  UE keeps sending uplink on the released tunnel as the probe; without it the
  tunnel is only watched. Needs a UE, else SKIP. Not yet run on Ref 2.

**Verdict note:** every missed **shall** is FAIL, including one that leaves the
core running; `DEVIATION` is reserved for a missed **should**. A known failure
that a lab tolerates goes on that core's expected-failures list, which keeps it
FAIL in the report but out of the gate. See
[expected-failures.md](expected-failures.md).

**Legend, check classes:** a `FAIL-class` check cites a shall; a
`DEVIATION-class` check cites only shoulds; an `INFO-class` check is
informational. An informational check cites descriptive text only (a response
the specification describes without requiring, or a case it is silent on). It
reports `INFO` when the core did what the text describes and `INFO-DIFF` when
it did anything else, with both in Expected and Observed, so a table of runs
shows where cores differ. Neither verdict gates, counts as a pass or a failure,
or can be put on an expected-failures list; `ERROR` and `SKIP` mean what they
mean for every check. In every class a core that does not survive a check's
stimulus (a fresh NG Setup afterwards no longer completes) is `FAIL` and
gates: a crash is never demoted.

> **Tooling note:** the testbed has no tshark, so wire disambiguation currently
> uses a byte-level PPID scan that reads uplink cleanly but misses downlink
> responses; response identity is confirmed via ORBIT's free5gc decode of the
> received bytes. For checks where a FAIL is possible (e.g. the GTP-U "shall"),
> a proper SCTP/NGAP pcap decoder (or tshark) would strengthen disambiguation.

## Other interfaces (matrices to grow)

Separate sections/files as coverage extends beyond NGAP:

- **XnAP** — TS 38.423 (Xn handover already exercised on the happy path).
- **NAS-5GS** — TS 24.501 (its own procedures + error-handling clause; security ties to TS 33.501). ✅ T3560 Authentication Request retransmission and release (`NAS-T3560-AUTH-RETRANSMIT`); ✅ T3550 Registration Accept retransmission (`NAS-T3550-ACCEPT-RETRANSMIT`); ✅ unknown DNN on PDU session establishment (`NAS-PDU-UNKNOWN-DNN`); ✅ Registration Reject #62 with the rejected S-NSSAI (`NAS-REG-REJECTED-NSSAI`). ✅ Deregistration Accept for a deregistration that is not switch-off (`NAS-DEREG-ACCEPT-NOT-SWITCH-OFF`). ✅ UE-requested PDU session release (`NAS-PDU-RELEASE-UE-REQUESTED`). ✅ Service Request from CM-IDLE re-establishing a PDU session's user plane (`NAS-SERVICE-REQUEST-REACTIVATES`). ✅ An initial registration right after the same UE's switch-off deregistration and release (`NAS-REGISTRATION-AFTER-RELEASE`).
- **GTP-U** — TS 29.281 (§7 error handling). ✅ unknown-TEID → Error Indication (`GTPU-UNKNOWN-TEID-ERRIND`, FAIL on SD-Core); ✅ End Marker on Xn path switch (`GTPU-END-MARKER-ON-PATH-SWITCH`); ✅ Echo Request → Echo Response with the sequence number copied (`GTPU-ECHO-RESPONSE`). ✅ PDU Session Container on downlink G-PDUs (`N3-DL-QFI-CONTAINER`, §5.2.2.7).
- **PFCP / N4** — TS 29.244 (only if ORBIT ever drives N4 directly; today the core owns it).
