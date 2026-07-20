# SD-Core (Aether / omec-project) interop notes

Tracks conformance/interop issues ORBIT has found in the **Aether SD-Core**
(omec-project) two-node testbed (ATB-01), and the opt-in quirks that work
around them. ORBIT's codecs stay strict and 3GPP/X.691-conformant by default;
quirks live only in the `sdcore` [core profile](../../internal/coreprofile)
and never bend the codec. See `docs/DESIGN.md` §5(i).

Deployed versions (read from the running SMF binary, `go version -m`):
`omec-project/ngap/v2 v2.1.0`, `omec-project/nas/v2 v2.0.0`,
`omec-project/smf` build `2026-06-02`.

The N2 handover path is the source of these — it is **unexercised upstream**
(gnbsim, omec's reference RAN sim, never drove N2 or Xn handover), so ORBIT is
the first tool to exercise it end to end.

---

## Finding 1 — SMF cannot decode a conformant `HandoverRequestAcknowledgeTransfer` (FIXED via quirk)

**Symptom.** After a fully-completed N2 handover (control plane green: AMF drives
`HandoverRequired → HandoverRequest → HandoverRequestAcknowledge →
HandoverCommand → HandoverNotify → UE Context Release`), the user-plane
downlink never switches to the target. The UPF keeps sending downlink to the
source gNB. SMF log:

```
ERROR producer/n1n2_data_handler.go:366
  handle HandoverRequestAcknowledgeTransfer failed: align Bit is not zero
```

**Root cause — a missing `optional` in omec's generated ASN.1 type.** In
`omec-project/ngap/v2 v2.1.0`, `HandoverRequestAcknowledgeTransfer` declares:

```go
DLForwardingUPTNLInformation  *UPTransportLayerInformation `aper:"valueLB:0,valueUB:1"`   // omec: NO `optional`
```

3GPP **TS 38.413 §9.3.4.11** defines `dLForwardingUP-TNLInformation` as
**OPTIONAL**. free5gc has it right (`...,optional`); omec dropped the tag, so
its codec treats a spec-OPTIONAL field as **mandatory**. Its decoder therefore
expects a value where a conformant encoder (correctly) omitted one, desyncs,
and fails at the next byte-alignment with *"align Bit is not zero"*.

**Conformance verdict — ORBIT is correct, SD-Core is not.** Verified three
independent ways that our encoding is the canonical X.691 APER:

1. **pycrate** (independent spec-derived 3GPP NGAP ASN.1) decodes our bytes
   `0007c0ac11320d000000c80001`, re-encodes to the identical bytes, and its
   from-scratch canonical encode of the same value matches.
2. **Byte-diff**: the *only* difference between free5gc's and omec's tags is
   the missing `optional`; encoding a struct with omec's (buggy) tags via
   free5gc's own aper reproduces omec's exact bytes — so the aper *libraries*
   agree, the *type definition* is wrong.
3. **omec's own v2.1.0 decoder** rejects the canonical bytes with the same
   error, and accepts only the (non-conformant) form with the field present.

**The quirk — `HandoverAckForwardingMandatory`.** Under the `sdcore` profile,
ORBIT emits the transfer with `dLForwardingUP-TNLInformation` present (set to
the same target tunnel), matching omec's schema. Implemented by encoding a
local struct with omec's tags via ORBIT's *own conformant* aper — no omec
dependency; verified byte-identical to omec's v2.1.0 encoder and decodable by
it. Golden test: `internal/gnb.TestHandoverAckTransferSDCoreQuirk`. The bytes
are non-conformant and a strict core would reject them — hence opt-in.

**Verified live (2026-07-04).** With `ORBIT_CORE_PROFILE=sdcore`, the
`align Bit is not zero` error disappears, the SMF decodes the transfer, and the
downlink FAR's `OuterHeaderCreation` switches from the source address to the
**target** (`172.17.50.13`) — which it never did before. Finding 1 is fixed.

**Upstream.** Report to `omec-project/ngap`: add `optional` to
`HandoverRequestAcknowledgeTransfer.DLForwardingUPTNLInformation` (and audit the
generator for other OPTIONAL fields with the tag dropped). When fixed, retire
this quirk.

---

## Finding 2 — N2 handover: SMF mis-decodes the TEID and never pushes the switch (ROOT-CAUSED; fix filed)

Once Finding 1 is worked around, a **second, independent** SD-Core bug blocks
data continuity across an N2 handover. Root-caused from the omec SMF source, its
logs, and an N4 capture — **two** SMF bugs, both entirely SMF-side (ORBIT's
transfer decodes to the correct tunnel, TEID `0x100`):

1. **GTP-TEID decoded as a LEB128 varint.** `context/ngap_handler.go`
   `HandleHandoverRequestAcknowledgeTransfer` reads the target downlink GTP-TEID
   with `binary.ReadUvarint` instead of a fixed 4-octet `binary.BigEndian.Uint32`
   (which every other TEID read in that file uses). ORBIT sends TEID `0x100`
   (bytes `00 00 01 00`); `ReadUvarint` stops at the first `0x00` → **0**. The
   downlink FAR is left with `OuterHeaderCreation {addr: target-gNB, TEID: 0,
   State: RULE_UPDATE}` (confirmed in the SMF log).
2. **The switch is never pushed to the UPF.** `producer/n1n2_data_handler.go`
   `HandleUpdateHoState` takes no `pfcpAction`/`pfcpParam` and leaves the
   SMContext in `SmStateModify`, so the updated (RULE_UPDATE) FAR is never put in
   a PFCP Session Modification. The N4 capture across the whole handover shows
   **zero** `SessionModificationRequest` — only heartbeats.

So the UPF keeps forwarding downlink to the source gNB and the flow does not
survive.

**Status: fix filed** — [`bengrewell/smf#1`](https://github.com/bengrewell/smf/pull/1)
fixes both (TEID → `BigEndian.Uint32`; `HoState_COMPLETED` collects the downlink
FARs, requests the PFCP modify, and moves to `SmStatePfcpModify`, mirroring the
`UpCnxState` `DEACTIVATED` path). To be proposed upstream to `omec-project/smf`
after review/soak — the "upstream-first" step ADR-0002 requires. Does not block
mobility *signalling*, which is fully proven; **Xn** already carries data across
handover today.

---

## Finding 3 — UPF sends no GTP-U Error Indication on an unknown TEID (DEVIATION)

TS 29.281 §7.3.1: when a GTP-U node receives a G-PDU for a TEID it has no
context for, it "shall discard the G-PDU [and], if the TEID … is different from
… 'all zeros', shall also return a GTP error indication" (message type 26, with
TEID Data I echoing the offending TEID).

Sending a well-formed 5G N3 G-PDU with a non-zero unknown TEID (`0x7FABCDEF`) to
SD-Core's BESS-UPF produces **no Error Indication**. Wire-confirmed: the G-PDU
leaves the RAN node to the UPF N3 (`172.17.50.12:2152 → 172.17.50.241:2152`,
one packet) and nothing returns; the data path between these two is otherwise
proven, so the G-PDU reaches the UPF and is silently dropped.

**Severity: benign.** Discarding the packet is safe; the Error Indication is a
courtesy that lets the sender tear down a stale tunnel faster, and it is
commonly unimplemented in production UPFs. So the conformance suite scores this
**DEVIATION**, not FAIL — a documented §7.3.1 gap that does not fail the CI gate.
Check: `internal/conformance` `GTPU-UNKNOWN-TEID-ERRIND`.

---

## Finding 4 — No AMF handover event on the Kafka stream; gNB "Disconnected" only on SCTP teardown (OBSERVATION)

Found while driving Xn + N2 handovers to study SD-Core's event stream (the AMF
and SMF publish structured JSON events to the in-cluster Kafka topics
`sdcore-data-source-amf` / `-smf`; `metricfunc` consumes them). Two related gaps,
both observations rather than conformance failures:

**(a) The AMF publishes no event at handover completion.** Neither an Xn path
switch (`HandlePathSwitchRequest`) nor an N2 handover (`HandleHandoverNotify`)
calls `AmfUe.PublishUeCtxtInfo()`, so the UE-context stream carries **no event
when a UE moves cells**. Every other UE state transition (Authentication →
SecurityMode → ContextSetup → Registered → Deregistered) publishes; handover is
the one lifecycle change that is silent. The AMF *does* update its internal
context (the new `RanUe`/`GnbId` is attached), so the move only becomes visible
**lazily** — on the UE's *next* published event, which then carries the new
`gnbid`. In a capture of one Xn + one N2 handover, the only event showing the
target gNB was the final `Deregistered`. Consequence for anything consuming the
stream: real-time handover tracking is impossible without an AMF change (add a
`PublishUeCtxtInfo()` call at both handover-completion points). SMF side is more
useful in real time — a handover drives extra `smf_pdu_sess_modify_req` events
(observed: **1** for Xn, **3** for N2), so the *type* is inferable there, but the
SMF events carry no gNB id for from/to attribution.

**(b) gNB `Disconnected` events fire only on SCTP teardown, not on stale-context
eviction.** `AmfRan.Remove()` does publish `NfStatusDisconnected`, but it is
only reached from the SCTP notification path (`SCTP_COMM_LOST` /
`SCTP_SHUTDOWN` in `ngap/dispatcher.go`). So: a gNB whose association is cleanly
torn down publishes Disconnected; but a gNB that goes away **without** an SCTP
close — or, notably, the **stale association from a reused gNB ID on a new source
address** (Finding 1's workaround territory) — is never `Remove()`d and so
**never publishes Disconnected**. The result is that `gnb_session_profile` /
NF-status views accumulate **ghost gNBs** that show `Connected` forever. This
matches what the core-side telemetry consumers see (an aether-ops issue tracks
the same ghost-context accumulation across gNB/UE/NF). ORBIT keeps its own gNB
associations up across a handover, so it does not itself emit spurious
Disconnected events; the gap is that the *absence* of a disconnect is
indistinguishable from a still-live gNB.

**Update (2026-07-14) — event signatures measured, and aether-ops now has a
heuristic detector that still misses handovers.** Re-exercised on a fresh
two-node LXD SD-Core (omec `5gc-smf:rel-4.1.0`) with controlled single-UE
handovers, watching the aether-ops event stream:

- An **Xn** handover publishes exactly **one** `smf_pdu_sess_modify_req` and
  nothing else attributable to the move — no AMF event, and no target
  `gnb-connect` (the target gNB is already N2-associated). Data survives.
- An **N2** handover publishes **three** `smf_pdu_sess_modify_req` plus an AMF
  `gnb-connect` (NF-status of the target gNB). No AMF handover event; data does
  not survive (Finding 2).
- Neither the `pdu-session-modify` nor the `gnb-connect` events carry a **UE
  identity or a from/to gNB** — the modify detail is only
  `{msg_type, source_nf_id}`. The stream shows *that* a session was modified,
  not *which UE* moved *where*; in a busy stream it is indistinguishable from a
  normal session modify (every PDU-session setup emits modifies too).
- aether-ops has since added a derived `serving-gnb-changed` detector
  (`source: derived, detector: heuristic`). In testing it fired only once, for a
  **registration** transition (lagging one step), and **missed all four**
  exercised handovers (2×N2, 2×Xn). It cannot attribute a mid-session cell
  change to a UE because no per-UE event carries the new gNB between
  registration and deregistration.
- The only reliable per-UE gNB attribution remains the terminal
  `deregistration` event (`{gnb, guti, ...}`), which correctly reported the UE's
  final cell.

Two ways to close it: (a) the AMF-side `PublishUeCtxtInfo()` change at both
handover-completion points (the real fix — a proper per-UE handover event); or,
without touching SD-Core, (b) have the consumer **poll** the AMF UE-context
state (SBI / the backing Mongo) and diff each UE's serving gNB, since the AMF
*does* hold the updated `GnbId` internally even though it publishes nothing —
poll-granularity, but works for both Xn and N2.

**Severity: observation.** Neither is a wire-protocol conformance issue — they
are gaps in SD-Core's *own* event/telemetry stream, relevant to tools that
consume it (dashboards, event feeds) rather than to the signaling ORBIT tests.
Recorded here so the handover/event-stream behavior isn't rediscovered.

## The compatibility model (why this isn't "tuning for SD-Core")

- **Codecs stay conformant.** ORBIT's default profile is `strict-3gpp` (zero
  quirks); a conformant core needs no profile and gets byte-exact 3GPP.
- **Quirks are opt-in, named, and documented** with the exact defect, core,
  version, and upstream report. Selected by `ORBIT_CORE_PROFILE` (currently the
  integration tests; a CLI flag later).
- **Quirks are observable** — `coreprofile.Profile.Active()` lists them, so the
  set a core needs is a **conformance scorecard** (feeds Phase 6), not hidden
  tuning.
- **Upstream-first** — every quirk maps to a filed bug and is deleted when the
  core is fixed.

## Reproduction

- Root-cause + conformance harnesses: `scratchpad/apertest` (Go — free5gc vs
  omec v1/v2 encode/decode) and `scratchpad/conform.py` (pycrate).
- Live: `internal/engine.TestLiveHandoverDataContinuity` with `-tags=integration`,
  `ORBIT_CORE_PROFILE=sdcore`, distinct routed source IPs, fresh gNB IDs
  (`ORBIT_SRC_GNB`/`ORBIT_TGT_GNB`) — the AMF does not re-key a reused gNB ID
  cleanly (a third, minor SD-Core robustness gap).
