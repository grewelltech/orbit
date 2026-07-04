# ORBIT — Master Design Plan

**Open Radio Benchmark and Integration Testbed** — a from-scratch, Go-native,
PHY-less 5G SA gNB + UE engine for benchmarking, conformance, and integration
testing of a real 5G core (Aether SD-Core).
Module: `github.com/bgrewell/orbit` · Status: **in design** · Grounds every claim in a 3GPP TS, a repo/URL, or the research findings.

---

## 1. Vision

ORBIT is a native Go engine that speaks the real 5G SA control and user planes to a live core — NGAP over SCTP on N2, NAS-5GS tunneled inside it, and GTP-U on N3 — with the radio replaced by a software model instead of an RF PHY. It owns the protocol engine end to end in pure Go, so we can do the things UERANSIM (the most-deployed no-PHY simulator) structurally cannot:

- **Signaling-level emulated mobility** — synthesized measurement reports driving real N2/Xn handover signaling against the AMF.
- **Core conformance / regression probing** — deliberately malformed NGAP/NAS with structured pass/fail and spec citations.
- **Rate-controlled performance/load** — attach storms, per-procedure P99.9 latency, per-UE throughput.
- **API-first** — gRPC/Connect + REST + a CLI that only ever calls the API.
- **First-class observability** — structured `slog` correlated to OpenTelemetry traces, Prometheus metrics, live state streaming.
- **CI/CD-native** headless operation from day one.

**Two honest scope corrections up front (these shape the whole plan):**

1. **The mobility pillar depends on a core that may not support handover.** Findings converge from three directions: gnbsim has *never* exercised N2 or Xn handover (both "pending"), SD-Core 1.3 frames multi-gNB as "groundwork, not done," and SD-Core 1.2 had a handover context-persistence bug. So "emulated mobility against a *live* core" is a **hypothesis gated on Discovery Spike D-1**, not a given. The plan carries an explicit fallback (§6, Milestone Gate M-1): a pre-staged free5gc core as an alternative real target, and degraded scope (mock-AMF signaling validation) if no real core completes the path. Mobility signaling *synthesis* is a certainty; mobility *against SD-Core specifically* is not.

2. **"VIAVI/Spirent-class" is an architectural style, not a numbers claim against SD-Core.** Commercial rigs do ~1.25M UEs / hundreds of Gbps. Our Stage-1 default is 1–2 Gbps/gNB, we target 10k UEs of *sim* capacity, and **SD-Core's own validated ceiling is ~5,000 UEs @ 10 attach/s**. The performance ceiling in any integration test is the core under test, not the sim. We therefore report two distinct numbers: **sim capability** (measured against a mock AMF/UPF) and **integration capability** (bounded by the live core, baseline 5k UEs / 10 aps).

**How it beats UERANSIM (feature-complete, not a re-implementation).** UERANSIM is the most-deployed no-PHY sim but is C++/AGPL-3.0 with a hard commercial-use warning ([UERANSIM README, License](https://github.com/aligungr/UERANSIM)), caps at 512 UEs per `nr-ue` process (`src/ue.cpp:290-291`), has **zero** handover (`hoAttach=NOT_SUPPORTED`, `src/ue/nas/mm/register.cpp:89`; deprioritized indefinitely, discussion #492), no ciphered SUCI, and no API — only YAML + `nr-cli`. ORBIT is Apache-2.0-compatible Go, targets 10k+ UEs of sim capacity, implements both handover types, all three SUCI schemes, multi-PDU-session, conformance mode, and an API. We **study** UERANSIM/PacketRusher/gnbsim/my5G-RANTester for behavioral guidance but **clean-room** the state machines from the specs — copying UERANSIM's AGPL C++ would taint the whole engine (§9).

---

## 2. Cellular primer for a tool-builder who is a cellular novice

You are building a client that impersonates a base station (**gNB**) and a fleet of phones (**UEs**) talking to a real 5G core. Strip away the radio and this is just a stack of network protocols over IP. Here is the whole surface you actually touch.

### 2.1 The interfaces (the "N-numbers")

Think of the core as a set of microservices ("Network Functions"): **AMF** (mobility/registration front door), **SMF** (session manager), **UPF** (the user-plane packet router), plus **AUSF/UDM** (auth + subscriber database). The gNB only ever talks to two of them directly:

| Interface | Between | Protocol | Transport | Spec | What it carries |
|---|---|---|---|---|---|
| **N2** | gNB ↔ AMF | **NGAP** | SCTP, port 38412, **PPID = 60 (0x3C)**, APER-encoded | TS 38.413 / 38.412 | All control signaling: "a UE showed up", "set up its session", handover |
| **N1** | UE ↔ AMF (logical) | **NAS-5GS** | *tunneled inside NGAP* | TS 24.501 | The UE's own conversation with the core — registration, auth, session requests |
| **N3** | gNB ↔ UPF | **GTP-U** | UDP, port 2152 | TS 29.281 | The actual user data packets, tunnel-encapsulated |
| **Xn** | gNB ↔ gNB | **XnAP** | SCTP | TS 38.423 | Direct gNB-to-gNB handover prep (bypasses the core) |

> **Wire trap — NGAP PPID is the integer 60 (0x0000003C), not `0x3c000000`.** `0x3c000000` (= 1,006,632,960) is the *big-endian byte-order rendering* seen in packet captures. If the SCTP `sndinfo.ppid` is set to that value the AMF drops the association. Set PPID = 60; assert it in the Phase-0 NG Setup smoke test. Which value application code passes depends on the SCTP library version: the pinned `bgrewell/sctp` fork byte-swaps in `SCTPWrite` (pass 60), while free5gc's `ngap.PPID = 0x3c000000` constant targets an older, non-swapping lib — never mix the two conventions. **Phase-0 finding (pcap, 2026-07-02):** our uplink carries the conventional network-order 60 (wire `00 00 00 3c`) and the AMF accepts it; the ATB-01 SD-Core (omec) AMF *replies* with the byte-reversed `3c 00 00 00` (= 60 swapped once too many — the `0x3c000000` constant run back through a swapping lib). **This is not a strict protocol violation:** RFC 4960 §3.3.1 says the PPID "is NOT touched by an SCTP implementation; therefore its byte order is NOT necessarily big endian. The upper layer is responsible for any byte order conversions" — an explicit exception to network byte order, and SCTP never demuxes on it. It is a byte-order *divergence* from the universal NGAP convention (send 60 in network order), harmless here but a candidate Phase-6 finding since a strict peer validating `PPID == 60` in network order would reject the AMF. ORBIT's receive path accepts both encodings (`internal/sctp.PPIDNGAPSwapped`) and records which was seen.

**N1 is the key mental model.** NAS is a message *from the phone to the core* whose guts the gNB cannot read — the gNB just relays an opaque NAS blob. In NGAP terms, the UE's Registration Request rides inside an "Initial UE Message" (TS 38.413 §8.6.1); replies come back inside "DL NAS Transport" (§8.6.2). So a "UE" and a "gNB" in our engine are two logical roles: the UE produces/consumes NAS; the gNB wraps/unwraps it in NGAP and owns the SCTP socket.

### 2.2 The protocol layers (minus RF)

- **NGAP (N2)** — ASN.1 messages with a strict typed structure. Every information element (IE) has an `id` and a **criticality** (`reject`/`ignore`/`notify`): if a mandatory `reject`-criticality IE is missing, the receiver must return an Error Indication or Unsuccessful Outcome (TS 38.413 §10). This criticality machinery is exactly what a *conformance* mode exploits. The minimum happy-path procedure set for one UE to attach and get a session: **NG Setup** (§8.7.1) → **Initial UE Message** (§8.6.1) → **DL/UL NAS Transport** (§8.6.2-3) → **Initial Context Setup** (§8.3.1) → **PDU Session Resource Setup** (§8.2.1).

- **NAS-5GS (N1)** — two sub-layers: **5GMM** (mobility: Registration, Authentication, Security Mode, Deregistration, Service Request) and **5GSM** (sessions: PDU Session Establishment/Modification/Release). PDU-session messages ride inside a 5GMM "NAS Transport" as an "N1SM container" (TS 24.501 §5, §8). The UE runs a state machine: `5GMM-DEREGISTERED → REGISTERED-INITIATED → REGISTERED → …` (TS 24.501 §5.1.3/§5.4.1).

- **Authentication & keys** (the part that rejects you silently if wrong). The core challenges the UE; the UE proves it holds a secret key **K** on its (simulated) SIM. Two methods: **5G-AKA** (Milenage functions f1–f5, TS 35.206 → RES/CK/IK) and **EAP-AKA'** (RFC 5448, EAP carried in NAS). From CK/IK a **key hierarchy** is derived with KDFs ("function codes" 0x69–0x6E per TS 33.501 Annex A): `K → CK,IK → KAUSF → KSEAF → KAMF → {KNASint, KNASenc} → KgNB → {KRRCint, KRRCenc, KUPint, KUPenc}`. Get any FC or input wrong and the AMF rejects with an opaque cause.

- **Identity privacy (SUCI).** The phone must not send its permanent ID (SUPI/IMSI) in the clear. It sends a **SUCI**: scheme 0 = null (clear), 1 = Profile A (ECIES over X25519), 2 = Profile B (ECIES over P-256) (TS 33.501 Annex C, TS 23.003 §28.7). The *UE side* (encrypt) must be built by us — free5gc only ships the *network side* (decrypt). **Note: SD-Core's default accepts null-scheme SUCI, so ciphered SUCI is a differentiator, not a critical-path attach requirement (see D-8).**

- **Ciphering/integrity.** Once authenticated, NAS is integrity-protected and optionally ciphered with one of four algorithm pairs: NIA0/NEA0 (null), 128-NIA1/NEA1 (SNOW 3G), 128-NIA2/NEA2 (AES), 128-NIA3/NEA3 (ZUC) (TS 33.501 §5.9).

- **RRC (TS 38.331) — this is where "no PHY" lives.** In a real network RRC configures the radio and carries measurement config. In our engine the UE and gNB are **in the same process**, so RRC becomes an *internal Go interface* — a struct passed on a channel, not a wire encoding — until conformance or inter-vendor testing forces real bytes.

- **GTP-U (N3).** Once a session is up, user IP packets are wrapped in a GTP-U header + a **PDU Session Container** extension header (type `0x85`, TS 38.415) that stamps the **QFI** (QoS Flow ID) — mandatory in 5G and must be the *first* extension header (TS 29.281 §5.2.1, added in v15.3.0). **Header size: carrying any extension header requires the E flag, which forces the 4-byte optional field (seq / N-PDU / next-ext-type). So the GTP-U header is 12 bytes (8 mandatory + 4 optional) before the PDU Session Container, not 8.** Each session has **two unidirectional tunnels**, each keyed by a **TEID** picked by the *receiver*: the gNB allocates the AN-TEID (downlink), the UPF allocates the CN-TEID (uplink), exchanged over NGAP.

### 2.3 What "no PHY" actually means

Everything above is IP signaling and can be emulated faithfully. The **only** thing you cannot do is transmit radio: the UE cannot *measure* a neighbor cell's signal (there is no signal), and it cannot perform **RACH** (the random-access preamble to sync to a cell). So:

- **Measurements are synthesized.** A `MeasurementReport` (TS 38.331 §5.5.5) is just numbers — serving/neighbor RSRP/RSRQ on a 0–127 scale. Our mobility engine *invents* these numbers to force a handover trigger event (e.g. event **A3**: neighbor becomes offset-better than serving, `Mn + Ocn − Hys > Ms + Ocs + Off`, TS 38.331 §5.5.4.4) at a moment we choose.
- **RACH becomes a no-op.** After the UE "receives" the handover command (`RRCReconfiguration` with `reconfigurationWithSync`), instead of transmitting a preamble it flips a state variable to the target gNB and emits `RRCReconfigurationComplete` (TS 38.331 §5.3.5.3). The step the spec calls "physical" is a single line of Go.

### 2.4 What real handover involves (and what we emulate)

Two flavors. Both end with the same goal: the UPF re-points the downlink tunnel at the new gNB.

**Xn handover** (gNBs talk directly, AMF barely involved) — TS 38.423 §8.3, TS 23.502 §4.9.1:
1. UE→srcGNB `MeasurementReport` → 2. srcGNB→tgtGNB `XnAP HandoverRequest` → 3. tgtGNB→srcGNB `HandoverRequestAcknowledge` → 4. srcGNB→UE `RRCReconfiguration(withSync)` → 5. srcGNB→tgtGNB `SN Status Transfer` → 6. *[RACH — skipped]* → 7. UE→tgtGNB `RRCReconfigurationComplete` → 8. **tgtGNB→AMF `NGAP PathSwitchRequest`** → 9. AMF→tgtGNB `PathSwitchRequestAcknowledge` → 10. tgtGNB→srcGNB `UEContextRelease`. Only steps 8–9 touch the core.

**N2 handover** (routes through the AMF throughout) — TS 38.413 §8.4: srcGNB→AMF `HandoverRequired` → AMF→tgtGNB `HandoverRequest` → tgtGNB→AMF `HandoverRequestAcknowledge` → AMF→srcGNB `HandoverCommand` → RRCReconfiguration → *[RACH skipped]* → `RRCReconfigurationComplete` → tgtGNB→AMF `HandoverNotify`; AMF then drives SMF→UPF (PFCP/N4) to switch the path. After the switch the source gNB sends a GTP-U **End Marker** (type `0xFE`, TS 29.281 §7.3) on the old tunnel so the UPF doesn't reorder packets.

Because the AMF only sees NGAP (not RRC/XnAP), we can validate the *NGAP portion* of a full handover against the real core while keeping RRC and XnAP as **in-process stubs** — the highest-leverage insight in the plan (§5, decision (c)). **Caveat (see D-3):** the RRC container is opaque to the AMF, but it is wrapped in an NGAP **Source-to-Target Transparent Container** (TS 38.413) that *is* NGAP ASN.1 and must be well-formed for our own target-side decode. Opacity to RRC bytes does not remove the obligation to encode the NGAP container correctly.

---

## 3. Target architecture

Five layers. The invariant: **CLI → API → Engine**. Nothing bypasses the API; the engine never imports the CLI.

```
                        orbit (single Go binary; roles selectable)
┌──────────────────────────────────────────────────────────────────────────────┐
│  CLI  (spf13/cobra)  ── talks ONLY to the API, never the engine directly       │
└───────────────┬────────────────────────────────────────────────────────────────┘
                │ gRPC / Connect / REST  (connectrpc/connect-go, one net/http mux)
┌───────────────▼────────────────────────────────────────────────────────────────┐
│  API SERVER  — Protobuf services: Scenario, UEFleet, Cell, Mobility,           │
│  Conformance, Load, StateStream(server-streaming), Metrics                      │
├─────────────────────────────────────────────────────────────────────────────────┤
│  OBSERVABILITY  slog→otelslog (trace-id in every log) │ prometheus/client_golang │
│                 OTLP traces (attach/HO/detach spans)  │ StateStream event bus    │
├─────────────────────────────────────────────────────────────────────────────────┤
│  ENGINE                                                                          │
│                                                                                  │
│   ┌── Scenario Executor (reuses internal/scenario) ── Load/Ramp scheduler        │
│   │        (golang.org/x/time/rate token bucket, SetLimit ramp curves)           │
│   │                                                                              │
│   ├── UE actor  (goroutine model — SHAPE GATED ON D-6, run before commit)        │
│   │      ├ NAS-MM FSM · RRC/CM tracker · data-path handle                        │
│   │      ├ 5G-AKA / EAP-AKA' + key hierarchy   ├ SUCI (null; Profile A/B opt)    │
│   │      └ per-UE 5GSM session state (per-session DNN/S-NSSAI)                    │
│   │                                                                              │
│   ├── gNB session mgr  (one SCTP assoc/gNB, muxes UE NAS over NGAP streams)      │
│   │      ├ NGAP procedure FSM (NG Setup, Init Ctx, PDU Res Setup, HO)            │
│   │      └ distinct bind IP per gNB  (fixes PacketRusher issue #138)             │
│   │                                                                              │
│   ├── Mobility engine  (synth RSRP/RSRQ → A3/A4/A5 → HO orchestration)           │
│   │      RRC + XnAP as in-process channels (stub); NGAP path is real             │
│   │                                                                              │
│   ├── Conformance harness  (ConformanceTest registry; inject malformed IEs)      │
│   │                                                                              │
│   └── DataPath interface  ─┬─ Mode A: per-UE TUN (functional, small scale)       │
│          │                 └─ Mode B: shared-TUN demux / native traffic gen      │
│          │                            (scale + load; TEID-keyed, sharded map)    │
│          │  encap tiers:  Stage1 userspace GTP-U (default, CI-safe)              │
│          │                Stage2 gtp5g kernel module (opt-in perf)               │
│          │                Stage3 XDP/eBPF (optional, cilium/ebpf)                │
│          └ per-UE / per-QFI stats (hdrhistogram-go), End Marker, QFI stamping    │
├─────────────────────────────────────────────────────────────────────────────────┤
│  PROTOCOL SUBSTRATE (reused, wrapped in internal/{ngap,nas,sctp,gtpu} adapters)  │
│   free5gc/{ngap,nas,aper,openapi}  (default codec line)                          │
│   omec-project/{ngap,nas}  (EVALUATED for the conformance decode path — D-11)    │
│   ishidawataru/sctp (forked+pinned) · wmnsk/{go-gtp,go-pfcp}                     │
│   songgao/water (forked+pinned)                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
        │ N2: NGAP/SCTP:38412 (PPID 60)  │ N3: GTP-U/UDP:2152      │ (opt) SBI/NRF
        ▼                                ▼                          ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│  LIVE 5G CORE  — primary: Aether SD-Core (omec: AMF·SMF·UPF·AUSF·UDM)           │
│                  fallback for mobility: free5gc (pre-staged, see M-1)           │
└──────────────────────────────────────────────────────────────────────────────┘
```

**Two data-path *modes*, orthogonal to the three encap *tiers*.** The tiered `DataPath` interface addresses *encapsulation performance* (userspace → kernel module → XDP). It does **not** address *interface count*: 10,000 per-UE TUN devices is an OS wall (netlink churn, interface tables, routing), independent of encap. So the data path has two explicit modes:
- **Mode A — per-UE TUN** (`songgao/water`): one TUN per UE, lets arbitrary host apps source traffic through a UE. Used for functional work at *small* scale.
- **Mode B — shared-TUN demux / native traffic generator**: a single TUN (or no TUN) with a TEID-keyed, sharded map routing packets, plus the built-in per-UE traffic generator (§6 Phase 5). This is the path to 10k UEs; per-UE TUN is **not** promised at 10k.

**Engine ↔ API ↔ CLI split.** The engine is a library with no I/O opinion beyond the sockets it must own. The API server is a thin Protobuf façade over engine methods, served by **connect-go** (one `net/http` handler serving gRPC + gRPC-Web + Connect/JSON simultaneously — no grpc-gateway; grpc-go clients interoperate unchanged). The **CLI (cobra)** constructs a Connect client and calls the API, so every capability is machine-reachable and the CLI can never drift from the API. Real-time visibility is a **server-streaming RPC** (`StateStream`) pushing per-UE FSM transitions, serving-cell changes, session IPs, and traffic rates to any subscriber (CLI `watch`, a Grafana panel, the aether-gui frontend).

**Engine backend and scenario model.** The engine is a library with its own scenario/config/traffic schema (BUILD, §4) — no external orchestrator is reused. An `EngineBackend` interface keeps the protocol engine swappable: the native Go engine is the default, and a higher-fidelity backend (e.g. a future OCUDU/srsRAN integration) can slot in behind the unchanged scenario contract. Scenario definitions load and run directly against the engine; single-slice attach works from config today, and slicing / multi-session is a first-class schema task (§5g).

---

## 4. Reuse strategy

Reuse-first: reuse the entire codec + infrastructure substrate; build only the differentiating layers. Licenses verified from findings; two are flagged for pre-build confirmation.

| Component | Reuse (name · license) or **BUILD** | Rationale |
|---|---|---|
| NGAP codec (TS 38.413) — default | **`free5gc/ngap`** · Apache-2.0 (v1.1.3) | Full `ngapType` IE set + APER convert; fuzz-tested. **Not itself SD-Core-validated — the omec fork is (see next row).** Codec only, no FSM imposed. |
| NGAP/NAS codec — conformance path | **EVALUATE `omec-project/{ngap,nas}`** · Apache-2.0 | These forks are what gnbsim/SD-Core actually field-use. For Phase 6 conformance we must decode *SD-Core-emitted* messages with byte-identical types → omec has the edge. **D-11 decides**; adapters (`internal/{ngap,nas}`) let the conformance path use omec while the happy path uses free5gc if needed. |
| NAS codec (TS 24.501) | **`free5gc/nas`** · Apache-2.0 (v1.2.3) | All 5GMM/5GSM messages + `security/` (key deriv, NAS integrity/cipher); EAP-Message IE present; SUCI scheme constants. |
| APER (ASN.1 PER) | **`free5gc/aper`** · Apache-2.0 (v1.1.1) | Transitive under ngap; only Go APER for 5G. *Watch: open padding-quirk issues → test every new msg vs live core.* |
| SBI/OpenAPI models | **`free5gc/openapi`** · Apache-2.0 | Conditional — only if we query NRF for AMF discovery or probe SMF/UDM SBI. Import per-NF sub-packages. |
| SCTP transport | **`ishidawataru/sctp`** (fork+pin) · Apache-2.0/BSD-3 | De-facto standard (gnbsim/PacketRusher/free5gc). No tags, ~23 open issues → **fork under our org, pin a commit** from day one — done: `bgrewell/sctp` @ 19ddcbc. Linux-only (raw syscalls; pure Go, no cgo — verified from fork source). |
| GTP-U (N3) Stage 1 | **`wmnsk/go-gtp`** (fork/extend) · MIT (verified from repo LICENSE, 2026-07-02) | Pure-Go, no kernel module → CI-safe. *Experimental, ~25% coverage, no `0x85`/QFI → build the 12-byte header + PDU Session Container + End Marker on top (2–3 wk spike, Phase 1b).* |
| TUN device (Mode A) | **`songgao/water`** (**fork+pin**) · BSD-3 | Kernel-module-free per-UE data plane. **Long dormant → same fork+pin discipline as sctp/go-gtp.** |
| GTP-U Stage 2 (perf) | **`free5gc/gtp5g` + `go-gtp5gnl`** · GPL-2.0 (module) / Apache-2.0 (Go ctl) | Line-rate when >2 Gbps or high UE density needed. Kernel ≥5.4, root, Secure Boot off → opt-in only. GPL confined to the kernel module. |
| GTP-U Stage 3 (opt) | **`cilium/ebpf`** · Apache-2.0; study **eUPF** | XDP fast path. Only if Stage 2 misses targets; needs C/eBPF + kernel ≥5.15. |
| PFCP (N4) | **`wmnsk/go-pfcp`** · MIT | Only for conformance/replay of N4, not the data path itself. |
| Packet capture/analysis | **`gopacket/gopacket`** · BSD-3 | Observability side-car (decode N3, per-packet metrics, pcap). Not the hot path. |
| API server | **`connectrpc/connect-go`** · Apache-2.0 (v1.20.0) | gRPC+gRPC-Web+Connect from one handler; server-streaming for state push. |
| CLI | **`spf13/cobra`** · Apache-2.0 (+ optional `protoc-gen-cobra`) | Standard; each command calls the Connect client. |
| Metrics | **`prometheus/client_golang`** · Apache-2.0 (v1.23) | System KPIs (success rate, active UEs, byte counters). |
| Latency percentiles | **`HdrHistogram/hdrhistogram-go`** · MIT (v1.2.0) | Accurate P99.9; one histogram/goroutine + `Merge()`; windowed rolling stats. |
| Rate control | **`golang.org/x/time/rate`** · BSD-3 | Token-bucket UE spawn; `SetLimit()` on a ticker = linear/step/exp ramp. |
| Tracing/logs | **`go.opentelemetry.io/otel` + `contrib/bridges/otelslog`** · Apache-2.0 | Spans per lifecycle op; `slog.InfoContext` auto-carries trace-id. |
| CI test output | **`gotestyourself/gotestsum`** · Apache-2.0 | JUnit XML + exit codes for GitLab/GitHub CI. |
| **NGAP/NAS procedure FSMs** | **BUILD** | Codecs give message *types*; sequencing, timers, retransmit, IE population are ours (clean-room from specs). |
| **5G-AKA + Milenage + key hierarchy** | **REUSE primitives + BUILD orchestration** (`free5gc/util/{milenage,ueauth}` · Apache-2.0) | **Corrected from "BUILD ~400 LoC" once the reuse surface was confirmed:** Milenage f1–f5 (`util/milenage`, verified against the 3GPP conformance test set) and the TS 33.501 Annex A KDF (`util/ueauth`, all FC constants) are standard non-differentiating crypto — reuse them. ORBIT builds only the UE-side *orchestration* (RAND/AUTN → RES* + KNASint/KNASenc) in `internal/ue/auth`. Still no AGPL UERANSIM. |
| **EAP-AKA' (RFC 5448)** | **BUILD** (Phase 7, gated) | No Go reference. AT_RAND/AUTN/MAC/RES parsing, CK'/IK' (FC=0x20). Transport IE already in `free5gc/nas`. |
| **UE-side SUCI conceal** | **BUILD** — null scheme done (Phase 1a, `internal/ue`); Profile A/B Phase 3+ on stdlib `curve25519`/`crypto/elliptic` | free5gc only decrypts. Null-scheme encoder built and round-tripped through free5gc's decoder (§5f). Profile A/B is *non-deterministic (ephemeral key) → verify via fixed-key test vectors + decrypt-and-compare, not bit-for-bit.* |
| **XnAP (TS 38.423)** | **BUILD** (stub first; real codec Phase 4) | No Go library — largest pure-build unknown. In-process stub until multi-process Xn is required. |
| **RRC abstraction (TS 38.331)** | **BUILD** (in-memory struct; wire codec only if forced) | UE↔gNB share a process → RRC is a Go channel, not bytes. |
| **Mobility / measurement-synthesis engine** | **BUILD** (~200 LoC event logic + trajectory model) | The ORBIT differentiator; nothing open-source does programmable A3/A4/A5 synthesis. |
| **Conformance harness** | **BUILD** (`ConformanceTest` registry) | No open-source pass/fail-with-spec-citation framework exists. |
| **Load orchestrator + stats + SLO engine** | **BUILD** | Ramp scheduler, per-procedure histograms, SLO assertions, CP/UP decoupling. |
| **gRPC/REST API + CLI + StateStream + scenario schema** | **BUILD** | No reference sim has any of this — the core product. |

---

## 5. Pivotal decisions

### (a) Reuse Go NGAP/NAS/APER codecs vs hand-roll — **REUSE. Decided.**
Every serious Go 5G project (gnbsim, PacketRusher, my5G-RANTester) sits on these Apache-2.0 codec families. Hand-rolling APER + the NGAP type system is 6–12 months with high interop risk. The libraries are *codecs, not state machines* — no architectural constraints, so we keep full control of sequencing and IE population. **Correction to the draft's rationale:** it is the **`omec-project` fork** that is field-validated against Aether SD-Core, *not* `free5gc/*` directly. **Guardrails:** default to `free5gc/*` for breadth on the happy path; **evaluate `omec-project/{ngap,nas}` for the Phase-6 conformance decode path** (D-11) where byte-identical decoding of SD-Core-emitted messages matters most. Wrap both behind `internal/{ngap,nas}` adapters so the choice is per-path and swappable. Pin exact versions in `go.mod` + Dependabot (free5gc modules are marked "not latest in module" — API can shift). Build a **golden-bytes fixture** for deterministic NAS/NGAP output (scope limited per §5f).

### (b) Engine backend pluggability — **own scenario schema, backend behind an interface. Decided.**
ORBIT defines its own scenario/config/traffic schema (BUILD, §4) rather than reusing an external orchestrator. An `EngineBackend` interface keeps the protocol engine swappable so a higher-fidelity backend (e.g. a future OCUDU/srsRAN integration) can slot in later behind the unchanged scenario contract; the native Go engine is the default. The scenario schema is single-slice by default; the slicing / multi-session extension is a first-class Phase-2 task (§5g), not an afterthought. **Exit criterion:** single-slice scenario definitions load and run on the native engine.

### (c) RRC & XnAP: wire-encode now, or in-process stub? — **STUB in-process first. Decided; highest-leverage call, single-point-gated.**
Because the AMF passes RRC containers **opaquely** and never touches XnAP, the NGAP portion of a full handover against the *real* core can be validated with RRC and XnAP as **Go structs on channels**. Full TS 38.331 UPER / TS 38.423 APER codecs become mandatory only for inter-vendor interop or running two gNBs as *separate processes*. **This decision rests entirely on D-3, and D-3's framing is refined:** it must confirm (1) the AMF is opaque to *RRC bytes*, **and** (2) `free5gc/ngap` (or the omec fork) correctly encodes the NGAP-level Source-to-Target Transparent Container that *wraps* the RRC blob, because our own target-side decode depends on it. Budget a contingency (partial NGAP-container encoding work) if (2) surprises.

### (d) Data-path performance approach — **Tiered `DataPath` encap interface + two interface *modes*. Default Stage-1 userspace, Mode A for functional, Mode B for scale. Decided.**
Encap tiers: Stage 1 (pure-Go userspace GTP-U over `water` + `wmnsk/go-gtp`) is the default — rootless/container/CI-friendly, ~1–2 Gbps/gNB. Stage 2 (gtp5g kernel module) is gated on a *demonstrated* need for >2 Gbps aggregate or high UE density; Stage 3 (XDP/eBPF) is optional beyond that. **Orthogonally**, interface Mode A (per-UE TUN) is for functional/arbitrary-app work at small scale; Mode B (shared-TUN demux + native traffic generator, TEID-keyed sharded map) is the only path promised at 10k UEs. Upper layers never touch the data path directly. *Do not adopt PacketRusher's "5 GB/s per UE" as a budget — plan 2–5 Gbps/gNB.*

### (e) Handover scope for the first mobility milestone — **N2-based ONLY in the mobility phase; Xn is its own later phase. Decided.**
N2 handover reuses the existing NGAP stack and exercises the real AMF (higher conformance value). Xn's *completion* is NGAP (`PathSwitchRequest`), but its *preparation* needs XnAP — no Go library exists (D-9 unresolved). **Correction to the draft:** Phase 3 is **N2 only**; Xn becomes **Phase 4**, gated on D-2 and D-9. This keeps the mobility milestone decoupled from an untooled protocol.

### (f) Golden-bytes / crypto verification method — **Deterministic-only bit compare; SUCI via test vectors + decrypt-and-compare. Corrected.**
The draft's "golden-bytes vs UERANSIM" and "property-test SUCI vs PacketRusher" are partly unworkable: UERANSIM has *no ciphered SUCI* (no reference to compare), and SUCI ECIES uses an **ephemeral key** so output is non-deterministic — a Registration Request *containing* a ciphered SUCI cannot be byte-compared at all. Therefore:
- **Bit-for-bit golden bytes** apply **only** to deterministic NAS/NGAP messages (e.g. registration with null SUCI, NG Setup, Initial Context Setup) captured from UERANSIM/PacketRusher as behavioral oracles.
- **SUCI Profile A/B** is verified by (1) **fixed-ephemeral-key test vectors** for reproducibility and (2) **decrypt-and-compare** against the free5gc *network-side* decoder (round-trip: conceal → free5gc de-conceal → original SUPI). PacketRusher is a *behavioral* oracle only, never a byte oracle.

### (g) Slicing / multi-session schema — **Explicit Phase-2 schema extension; "run unchanged" means single-slice. Decided.**
Repo reality: `internal/config` models exactly **one global `Core.Slice` (SST/SD) + `Core.DNN`**, consumed via templates — there is **no per-UE or per-session S-NSSAI/DNN** in the scenario schema. Single-slice attach works from config today (Phase 1 fine). But "multi-PDU-session per UE" and "slice-aware multi-session" require **per-UE Requested NSSAI + per-session DNN/S-NSSAI** that neither the schema nor the single-slice config expresses. This is a **first-class Phase-2 schema task**, not an afterthought. The "scenarios load and run unchanged" goal is therefore scoped to **single-slice** scenarios; multi-slice is the schema extension. **Status — DONE (2026-07-04), verified live against ATB-01:** `UEConfig` carries per-UE `RequestedNSSAI` (sent in Registration Request, §9.11.3.37) and a list of `PDUSessions`, each with its own DNN + S-NSSAI; the attach FSM establishes them all with distinct per-session downlink TEIDs. A UE established **two PDU sessions (distinct IPs 192.168.100.88/.100, DL-TEIDs 1/2, DNN internet)** on the live core; single-session backward compat unaffected. Multi-*slice* (distinct S-NSSAIs) is expressible in the schema but needs a multi-slice-configured core to exercise (Phase-7 acceptance); ATB-01 is single-slice.

### (h) API streaming model — **server-streaming for state push; injection as unary. Recommended.**
Connect's HTTP/1.1 protocol supports unary + server-streaming, covering live state push. If the CLI later needs to *inject* synthetic measurement reports, model injection as **separate unary RPCs** (keeps Connect/curl-friendliness) rather than bidi, unless a strong need for bidi appears. Decide before freezing the `.proto`.

---

## 6. Phased plan

Sequencing principle: **a minimal gNB+UE that registers ONE UE and passes data end-to-end against a live core comes first and is verified there** — then scale, then mobility, then performance, then conformance. The keystone is split so crypto and the GTP-U spike do not sit inside the single go/no-go. Goals advanced tagged `[mob|conf|perf|obs|api|cicd]`.

**CI taxonomy (applies to every phase).** Two pipelines, stated explicitly because "CI/CD-native" was conflating them:
- **unit-CI** — headless, mockable, no core: build, unit tests, golden-byte fixtures, lint, `gotestsum` JUnit. Runs on any standard runner.
- **integration-CI** — requires a **live core + SCTP + `NET_ADMIN` lab runners** (and for perf, a pinned-kernel host). Gated on lab-core availability; not "standard CI." Perf SLO gates (Phase 5) and conformance suites (Phase 6) live here and inherit its infra/cost/availability constraints.

### Phase 0 — Foundations & Sprint-0 constraints `[obs api cicd]`
**Goal:** repo scaffolding, hard constraints proven, substrate wired.
**Tasks:** fork+pin `ishidawataru/sctp` **and `songgao/water`**; verify `wmnsk/go-gtp` license before committing to it; verify `modprobe sctp` and `NET_ADMIN` on the atb-01/lab host and CI runners; stand up `internal/{ngap,nas,sctp,gtpu}` adapter packages over the free5gc libs; `slog`→`otelslog`→OTLP + `prometheus/client_golang` skeleton; initial `.proto` service stubs + connect-go server + cobra CLI shell; `gotestsum` JUnit in unit-CI; scaffold integration-CI (lab-core reachability check). Confirm the deployed **SD-Core version** and its default auth method (D-8, expected 5G-AKA / null SUCI). **Set SCTP PPID = 60 and assert it in the NG Setup smoke test.**
**Verification:** unit-CI green headless; `orbit serve` starts, CLI reaches it over Connect; an SCTP association opens to the AMF's N2 port and **NG Setup** is exchanged (no UE yet), PPID asserted = 60.
**Exit:** NG Setup succeeds against a live core AMF; substrate compiles and is observable; SCTP Linux-only constraint documented (pure Go, no cgo — corrected from the draft's "CGO" claim by reading the fork source).
**Advances:** obs, api, cicd (foundation for all).

### Phase 1a — Control-plane single-UE attach (keystone, part 1) `[api obs cicd]`
**Goal:** ONE UE reaches `5GMM-REGISTERED` and completes PDU-session *signaling* — no user-plane bytes yet.
**Tasks:** NGAP procedure FSM (NG Setup → Initial UE Message → DL/UL NAS Transport → Initial Context Setup → PDU Session Resource Setup); NAS-MM FSM (Registration → **5G-AKA** challenge/response → Security Mode → Registration Accept); one 5GSM PDU Session Establishment (signaling to `PDU SESSION ACTIVE`); **5G-AKA + Milenage + full key hierarchy** + NAS integrity/ciphering at **NEA0/NIA2** (D-8 confirmed the ATB-01 AMF offers ciphering NEA0 and integrity NIA1/NIA2 — NIA0 null integrity is **not** offered, so NIA2 AES-CMAC integrity is mandatory, not optional); **null-scheme SUCI only** (SD-Core default accepts it — D-8); expose `Register/Deregister/Status` unary RPCs + `StateStream`. Golden-bytes fixture for the **deterministic** registration/NGAP messages (§5f).
**Verification:** against a **live core** — UE reaches `5GMM-REGISTERED`, obtains a PDU session context + IP allocation (control-plane confirmation, `PDUSessionResourceSetupResponse`); deterministic registration bytes match the golden fixture; `StateStream` shows transitions live.
**Exit:** single UE reaches REGISTERED with an established session context via the API. **This is the go/no-go for the engine's control plane.**
**Advances:** api, obs, cicd.
**Status — DONE (2026-07-03), verified live against ATB-01 SD-Core (omec rel-3.1.0):** a UE registers (null-SUCI Registration → 5G-AKA → Security Mode NIA2/NEA0 → Initial Context Setup → Registration Complete) and establishes a PDU session with a real IP from the core's pool (192.168.100.0/24), driven through the Connect `UEService` (`Register`/`Deregister`/`Status`/`List`/`StateStream`) and the `orbit ue` CLI. UPF N3 endpoint (address+TEID) captured for Phase 1b. Reuse-first paid off: free5gc `util/{milenage,ueauth}` + `nas/security` supply the crypto; ORBIT builds the UE/gNB orchestration.

### Phase 1b — User-plane data path (keystone, part 2) `[perf obs cicd]`
**Goal:** the Phase-1a session carries bidirectional user data.
**Tasks:** Stage-1 userspace GTP-U with **Mode A** per-UE TUN; build the **12-byte GTP-U header + `0x85` PDU Session Container + QFI stamping** on top of `wmnsk/go-gtp` (the 2–3 wk spike, isolated here — not in the go/no-go); drive one flow through the tunnel; per-QFI byte counters.
**Verification:** against the **live core** — an ICMP echo + a small native flow traverses N3 to the UPF and back (gnbsim's own smoke test is ICMP-over-N3). QFI correctly stamped as the first extension header; End-Marker path scaffolded (exercised in Phase 3).
**Exit:** single UE, single session, **bidirectional data verified end-to-end on the real core**, driven entirely through the API.
**Advances:** perf (data-path foundation), obs, cicd.
**Status — DONE (2026-07-03), verified live against ATB-01:** Stage-1 userspace GTP-U (`internal/gtpu` codec + `internal/datapath` tunnel) carries an ICMP echo from the UE (192.168.100.x) to 8.8.8.8 and back through N3; `orbit ue ping` reports replies + RTT over the Connect API. On-wire capture confirms the 12-byte header + `0x85` PDU Session Container with QFI stamped first, both directions. **Infra note:** the UPF N3 (172.17.50.241) is on an isolated Multus access-net unreachable from grewell01, so the data path runs from the RAN node (172.17.50.12) — cross-compiled binary, `GNBN3Addr` reported to the UPF. N2 control plane still runs fine from grewell01.

### Phase 2 — Scale-out & multi-cell `[perf obs api cicd]`
**Goal:** many UEs across multiple gNBs; single-slice scenario definitions load and run; the actor architecture is chosen on evidence.
**Tasks (ordered):**
1. **Run D-6 FIRST** (goroutine-per-UE vs worker-pool) against a **mock AMF**, *before* committing the actor model — this decides the architecture rather than validating one already built.
2. Implement the chosen UE-actor model (NAS-MM + RRC/CM tracker + data-path handle per UE); one SCTP association per gNB multiplexing UE NAS over NGAP streams; **distinct bind IP per gNB** (fixes PacketRusher #138 up front); shard the TEID→conn map (256 shards) to avoid `sync.Map` contention.
3. Define the scenario schema and wire the `EngineBackend` behind `internal/scenario`; single-slice scenario definitions load and run on the native engine.
4. **Slicing schema extension (§5g):** add per-UE Requested NSSAI + per-session DNN/S-NSSAI to the scenario schema; enable **multi-PDU-session per UE** (up to 15, TS 23.501 §5.8.2.2). Keep a single global slice/DNN as the default so single-slice scenarios still parse.
5. Introduce **Mode B** (shared-TUN demux) so scale does not require 10k TUN devices.
**Verification:** single-slice multi-cell / scale scenario definitions load and run on the engine; register N UEs across ≥2 gNBs on the live core (checkpoint: the SD-Core-validated **5,000 UEs @ 10 attach/s**); **sim-capacity** (goroutine/RSS scaling to 10k) measured separately against the mock AMF; per-UE memory on the order of ~2 MB or better.
**Exit:** stable multi-cell, multi-session fleet on the real core; single-slice scenario definitions load and run; actor model chosen on D-6 evidence; Mode B available for scale.
**Advances:** perf, obs, api, cicd.
**Status — actor model + multi-gNB DONE (2026-07-03), verified live against ATB-01:** `gnb.Session` muxes many UEs over one N2 association (NG Setup once; downlink demuxed by RAN-UE-NGAP-ID; UEs spread across the association's *negotiated* SCTP streams — the AMF granted fewer than requested, so the count is read from `SCTP_STATUS`). `engine.Fleet` dials one association per gNB (distinct bind address supported) and registers UEs across them with the D-6 bounded-concurrency pool. **100 UEs across 2 gNBs reach REGISTERED on the live core at ~10 attach/s** — which *is* the SD-Core baseline: the core is the ceiling, not the sim (mock-AMF sim capacity ~2100/s, §1). **Confirmed core-bound by a concurrency sweep:** raising the attach worker pool 8→16→32→64 leaves throughput flat at ~9–10/s (and slightly worse at 32/64 from contention), with 0 failures — the core serializes attaches, it does not drop them. This is the first real integration benchmark and vindicates D-6: bounding our own concurrency is what lets us attribute the ceiling to the core rather than the tool. Remaining Phase-2 items: slicing schema (§5g, task) and Mode B shared-TUN demux + sharded TEID map (data-path scale).

### Milestone Gate M-1 — Mobility feasibility (before Phase 3) `[mob]`
**This is a first-class gate on the mobility *value proposition*, not just the FSM build.**
- **Run D-1 (N2) and D-4 (UPF SN/End-Marker behavior)** against the **primary** live core (SD-Core).
- **Pre-stage a free5gc core** (the same codec family is validated against it) as an alternative real target, and run D-1/D-4 there too.
- **Decision:**
  - If a real core (SD-Core *or* free5gc) completes `HandoverCommand` + N4/N3 path switch → proceed to Phase 3 with that core as the mobility target.
  - If **no** real core completes it → mobility scope **degrades to signaling validation against a mock AMF** (synthesis + NGAP FSM correctness, no real path switch). This is stated openly as the fallback, and the "against a live core" claim is qualified in all external messaging.
**Exit:** a named mobility test target (SD-Core, free5gc, or mock-AMF-degraded) with D-1/D-4 evidence. **D-1 PASSED against SD-Core (2026-07-04): the AMF drives N2 handover — the mobility target is SD-Core itself; no free5gc fallback needed.** Remaining for the full loop: distinct routed source IPs per gNB (run from the RAN node) so the AMF can deliver Handover Request to the target, and D-4 (UPF SN/End-Marker). Phase 3 is GO with SD-Core.

### Phase 3 — Emulated mobility: N2 handover `[mob obs api]`
**Goal:** real N2 handover behavior from the core with zero RF (or the M-1 degraded fallback).
**Tasks:** measurement-synthesis engine (per-UE per-cell RSRP/RSRQ over a trajectory model → A3/A4/A5 evaluation, ~200 LoC); RRC + XnAP as **in-process stubs**; **N2 handover NGAP FSM** (HandoverRequired → HandoverRequest → Ack → HandoverCommand → HandoverNotify + UL/DL RAN Status Transfer, with the correctly-encoded NGAP Source-to-Target Transparent Container per D-3); GTP-U tunnel switch (new AN-TEID at target, `PDUSessionResourceToBeSwitchedDLList`, release old tunnel) with **End Marker** on the old path; API to trigger handover + observe on `StateStream`; extend scenario schema with handover triggers. **Optionally introduce SUCI Profile A/B here** (off the keystone; verified per §5f). **Xn is explicitly out of scope for this phase (§5e).**
**Verification:** **D-3 must pass before building the full FSM.** Then a UE with a live native flow hands over **N2** between two gNBs with session continuity and measurable (near-zero, End-Marker-bounded) loss across the switch — on the M-1 target core. In the degraded fallback, verify NGAP FSM correctness + synthesis against the mock AMF.
**Exit:** N2 handover with data continuity verified on the M-1 target (or NGAP-FSM correctness in fallback); mobility driven and observed via API.
**Advances:** mob, obs, api.

### Phase 4 — Xn handover `[mob obs api]`
**Goal:** Xn-based handover (completion path proven, prep phase in-process until a codec exists).
**Tasks:** **gated on D-2 (does the core complete Xn `PathSwitchRequest`?) and D-9 (`free5gc/aper` can round-trip XnAP types)**. Build the **NGAP `PathSwitchRequest`/`Ack` completion**; keep XnAP *preparation* as an in-process stub unless D-9 shows a viable codec path, in which case begin a real XnAP codec (largest pure-build unknown). GTP-U path switch reused from Phase 3.
**Verification:** minimal `PathSwitchRequest` gets a correct `Ack` + N4/N3 completion on the target core; a UE hands over Xn with continuity (or, if D-2 negative, Xn is documented as core-unsupported and deferred).
**Exit:** Xn `PathSwitch` verified on the target core, or an evidence-backed deferral.
**Advances:** mob, obs, api.

### Phase 5 — Performance / load testing `[perf obs api cicd]`
**Goal:** rate-controlled load with per-procedure KPIs, reported honestly against both sim-capacity and core-limited baselines.
**Tasks:** ramp scheduler (`x/time/rate` + `SetLimit()` curves: linear/step/exp/Poisson); per-goroutine `hdrhistogram-go` merged at 1s/10s/60s windows for P50/95/99/99.9 per procedure (registration, PDU-session setup, service request, handover); Prometheus exposition; **SLO assertion engine** (fail-fast/report-continue on P99 or success-rate thresholds → integration-CI gate); **CP/UP decoupling** (CP-only AMF/SMF stress vs UP-only UPF hammer vs combined); **native per-UE traffic generator** (rate/size/burst/bidi, Mode B — no external iperf3 dependency); handover-under-load mode; soak mode with resource-trend hooks. Gate **Stage-2 gtp5g** here only if Stage-1 throughput is the measured bottleneck (D-5).
**Verification:** two distinct measurements — (1) **sim capability** against a mock AMF/UPF (the number that reflects *our* engine); (2) **integration** against the live core, where the **5,000-UE / 10-aps SD-Core baseline is the ceiling** and is stated as such. Export P99.9 histograms + Prometheus time-series to Grafana; SLO gate fails the build (integration-CI) on a synthetic threshold breach. Attribute failures core-vs-sim via time-aligned AMF/SMF/UPF metrics + logs.
**Exit:** rate-controlled attach storm + throughput/latency/jitter reporting per-UE and aggregate; sim-vs-core numbers reported separately; SLO gate usable in integration-CI.
**Advances:** perf, obs, api, cicd.

### Phase 6 — Core conformance / regression `[conf obs api cicd]`
**Goal:** structured pass/fail probing with spec citations — framed as **graceful-rejection / regression assertion**, not novel bug-finding.
**Tasks:** **run D-11 first** (choose free5gc vs omec types for decoding SD-Core-emitted messages). `ConformanceTest{ID(); SpecRef(); Run(ctx,Engine) Result}` + `TestRegistry` by category (procedural, negative-IE, security, GTP-U, timing); `Result{verdict, specRef, observed, expected, pcapFrame}`; inject deliberately malformed NGAP (strip mandatory `reject`-criticality IEs) on the highest-value set — **RANConfigurationUpdate, LocationReport, PathSwitchRequest, NG Reset** — and assert **Error Indication rather than crash**; NAS state-machine-violation + replay tests (TS 33.501); GTP-U unknown-TEID → Error Indication (TS 29.281 §7.3.1); concurrent-procedure timing tests (N2 HO + NAS SMC) in a separate **flaky-allowed** category; machine-readable JSON/SARIF + pcap correlation.
**Framing corrections (from findings):** omec AMF issues **#672/#673 were fixed in v2.2.1**; against the deployed omec AMF (v3.1.0) these tests should **PASS** — sell them as **regression guards**, not reproductions. **CVE-2026-42082 is a free5gc/amf v4.2.1 bug**, not an omec/SD-Core bug — **do not cite it against SD-Core** without first confirming the omec fork is affected; keep the timing-test category but describe it as class-of-defect coverage, and note it **depends on working handover (Phase 3/4)**, hence on M-1.
**Verification:** the suite runs headless in integration-CI against the core, emits per-test PASS/FAIL with TS citations; malformed-message tests confirm graceful rejection; **sim-bug vs core-bug disambiguation via pcap+decode is a hard requirement** before asserting any result; responsible-disclosure path in place before running against shared cores.
**Exit:** named, cited conformance/regression suite integrated as an integration-CI job with structured output.
**Advances:** conf, obs, api, cicd.

### Phase 7 — Hardening & optional depth `[all]`
Each item carries its own acceptance criteria (no ungated grab-bag):
- **NSSAI/slice-aware multi-session** — *acceptance:* a scenario with ≥2 S-NSSAIs and per-session DNNs attaches and establishes concurrent sessions on distinct slices against a multi-slice-configured core; StateStream shows per-session S-NSSAI. (Builds on the §5g schema.)
- **EAP-AKA' (RFC 5448)** — deferral defensible (D-8: SD-Core defaults 5G-AKA; no Go reference). *Acceptance:* against an EAP-AKA'-configured core or stub AUSF, UE completes EAP-AKA' to REGISTERED. Gated on a real need + a validation target.
- **NAS user-plane ciphering (NEA1/NEA3 SNOW3G/ZUC)** — gated on **D-7** (does `free5gc/nas security/` export SNOW3G/ZUC?); *acceptance:* NIA1/3 + NEA1/3 round-trip against the core.
- **GUTI re-registration; PDU Session Modification** — *acceptance:* documented procedures pass against the core.
- **Real XnAP/RRC wire codecs** — only if multi-process/inter-vendor required; gated on D-3/D-9.
- **Stage-3 XDP data path** — only if Stage-2 misses load targets (D-5).

---

## 7. Discovery spikes / open questions

| ID | Question | Phase | Why it matters | Method |
|---|---|---|---|---|
| **D-1** | Does a live core (SD-Core; else free5gc) actually complete **N2** handover? | **M-1, before P3** | gnbsim never exercised it; omec *declares* support (v3.1.0) but SD-Core 1.2 had a context-persistence bug fixed only in 1.3. **This gates the mobility value prop, not just the FSM.** | Minimal `HandoverRequired` at the live AMF; confirm `HandoverCommand` + SMF/UPF N4 modification + N3 switch. Run against SD-Core **and** the pre-staged free5gc. **RESOLVED 2026-07-04 — POSITIVE.** Sent a well-formed `HandoverRequired` (source gNB → AMF) with the NGAP source-to-target transparent container (placeholder RRC blob) for an attached UE. AMF logs: `handle HandoverRequired` → prepared it → `send Handover Request` toward the target gNB. **SD-Core (omec rel-3.1.0) actively drives N2 handover** — refuting the pessimistic hypothesis. The only failure was `Ran addr is nil` delivering Handover Request to the target when both gNBs dialed from **one source IP** (the PacketRusher #138 collision, §5b) — needs distinct *routed* source IPs per gNB (the RAN node's access-net aliases qualify; a fresh grewell01 alias doesn't route back). Also validates **D-3(a)**: the AMF accepted the opaque RRC container, so the in-process RRC stub is sound. |
| **D-2** | Does the core complete **Xn** `PathSwitchRequest`? | before P4 | Xn also "pending" in gnbsim; SD-Core 1.3 frames multi-gNB as groundwork. | Minimal PathSwitchRequest; confirm Ack + N4/N3 completion. |
| **D-3** | (a) Is the AMF opaque to **RRC bytes**? (b) Does the codec encode the NGAP **Source-to-Target Transparent Container** correctly? | before P3 | (a) validates the in-process stub (§5c); (b) is still owed even if (a) holds — our target-side decode needs a valid NGAP container. | (a) handover with a *garbage RRC* container inside a *valid NGAP* container; observe AMF completion. (b) round-trip the NGAP transparent container through the codec. |
| **D-4** | What GTP-U **SN values** does the UPF accept post-handover? Does it honor End Markers? | **M-1 / P3** | In a PHY-less sim SN values are synthetic; a strict UPF could drop packets → continuity illusory. | iperf3/native flow through a handover; measure loss with/without End Marker and varied synthetic SNs. |
| **D-5** | Which **encap tier** do we need; where does Stage-1 userspace GTP-U saturate? | P2→P5 | Decides Stage-2 (gtp5g, kernel-module CI friction) adoption. | Benchmark userspace GTP-U at 100/500/1000 concurrent UEs on atb-01; find the crossover. |
| **D-6** | Goroutine-per-UE vs worker-pool at 10k+? | **early P2, before building the actor model** | ~10k UEs × ~3 goroutines × ~8 KB ≈ 240 MB before state; bursty attach storms stress the scheduler. **Must precede the architecture commit.** | Attach 10k vs a **mock AMF**; measure RSS + p99 attach latency. **Resolved 2026-07-03** (`internal/mockamf` harness, 10k in-process attaches): throughput is equal (~2.1–2.4k/s — the work, not the model, is the ceiling), but a **bounded worker pool wins decisively on tail latency and memory**: p99 attach **414 ms vs 2.6 s** (6×), p99.9 **552 ms vs 3.0 s**, peak goroutines **744 vs 8191** (11×), peak RSS **93 MB vs 214 MB** (2.3×). Unbounded goroutine-per-UE turns a 10k attach storm into a scheduler pile-up. **Decision:** bound concurrent *attaches* (worker pool / rate-limited spawn, ties into the Phase-5 ramp scheduler); an attached UE at rest can remain a lightweight goroutine — the cost is in the concurrent burst, not steady state. |
| **D-7** | Does `free5gc/nas security/` export callable **SNOW3G / ZUC** (NIA1/3, NEA1/3)? | P7 | If not, implement from TS 35.215 / ETSI TS 135.222. | Grep/exercise the package before relying on it. |
| **D-8** | Default core **auth method** (5G-AKA vs EAP-AKA') and null-SUCI acceptance? | P0 | Confirms 5G-AKA + null SUCI is the critical path (keeps Profile A/B off the keystone). | Inspect deployed SD-Core config; attach with null SUCI. **Resolved 2026-07-02:** ATB-01 runs omec AMF rel-3.1.0; auth is **5G-AKA** (Milenage: key/opc/seqNum provisioned, no per-sub override); NAS security is `cipheringOrder:[NEA0]`, `integrityOrder:[NIA1,NIA2]` — **NIA0 is NOT offered, so Phase 1a must implement real NIA2 integrity** (null ciphering is fine, null integrity is not). Null-SUCI acceptance to confirm on first attach. |
| **D-9** | Can the APER toolchain cleanly encode/decode **XnAP** (TS 38.423)? | before P4 | Largest pure-build unknown; APER like 38.413 but untested with this toolchain. | Generate a few XnAP types, round-trip them. |
| **D-10** | SCTP **multi-stream / multi-homing** correctness under the NGAP pattern in the forked lib? | P0/P2 | NGAP uses stream 0 for non-UE-assoc + per-UE streams; wrong routing = *sim* bugs masquerading as core bugs. | Validate stream routing independently before load. |
| **D-11** | For the conformance decode path, **`free5gc` vs `omec-project` types** — which byte-identically matches SD-Core output? | before P6 | The omec fork is what SD-Core field-uses; conformance needs exact decode of core-emitted messages. | Capture live SD-Core NGAP; decode with both; diff type coverage + padding handling. |

---

## 8. Risks and how phasing de-risks them

| Risk | Impact | De-risking (phase) |
|---|---|---|
| ~~**Live core cannot do handover**~~ — **REFUTED 2026-07-04 (D-1):** SD-Core (omec rel-3.1.0) actively drives N2 handover (received `HandoverRequired`, sent `Handover Request`). | ~~Mobility collapses to mock-AMF.~~ Risk retired for SD-Core. | The mobility value prop is live: Phase 3 targets SD-Core, no free5gc fallback needed. Residual: distinct routed source IPs per gNB (RAN node) for target delivery; D-4 for UPF SN/End-Marker. |
| **Keystone over-scope hides crypto + a 2–3-wk spike in the go/no-go.** | The single most important gate becomes multi-month and crypto-fragile. | **Split into 1a (control plane, null SUCI, NEA0/NIA2) and 1b (user plane + GTP-U spike)**; **SUCI Profile A/B moved out to Phase 3** (off the critical path — SD-Core accepts null SUCI, D-8). |
| **Golden-bytes invalid for SUCI** (UERANSIM has none; ECIES ephemeral key ⇒ non-deterministic). | Wasted effort / false confidence. | **§5f:** bit-for-bit only on deterministic NAS/NGAP; SUCI via fixed-key vectors + decrypt-and-compare against free5gc's network-side decoder; PacketRusher behavioral-only. |
| **Wrong library "SD-Core-validated" premise** — omec fork is field-proven, not free5gc. | Conformance path decodes core output with the wrong types. | **Evaluate omec-project/{ngap,nas} for the conformance path (D-11)**; adapters allow per-path choice; don't claim free5gc is SD-Core-validated. |
| **Scenario schema underspecified for scale** — single-slice/global-DNN assumptions block multi-session before it is noticed. | Slicing/multi-session silently blocked. | **§5b/§5g:** ORBIT owns the scenario schema; the slicing extension (per-UE NSSAI, per-session DNN/S-NSSAI, fleet expansion) is a first-class Phase-2 task, not an afterthought. |
| **Per-UE TUN vs 10k UEs contradiction.** | 10k TUN devices hit an OS wall unrelated to encap tier. | **Two data-path modes (§5d/§3):** Mode A (per-UE TUN, small scale) vs Mode B (shared-TUN demux + native traffic gen, scale). Per-UE TUN not promised at 10k. |
| **D-6 placed after the actor model it decides.** | Building the wrong architecture, then reworking. | **D-6 runs first in Phase 2 against a mock AMF**, before the actor commit. |
| **Slicing needs unflagged schema work; "run unchanged" overclaimed.** | Multi-session/slicing silently blocked; scope claim too broad. | **§5g:** per-UE NSSAI + per-session DNN/S-NSSAI is an explicit Phase-2 task; "load and run unchanged" scoped to single-slice scenarios; slicing gets Phase-7 acceptance criteria. |
| **"CI/CD-native" conflates unit-CI with live-core integration.** | SLO/conformance gates assumed to run on standard CI; they need lab infra. | **CI taxonomy (§6):** unit-CI (mockable) vs integration-CI (lab core + SCTP + NET_ADMIN + pinned kernel); infra/cost/availability stated. |
| **"VIAVI/Spirent-class" oversold; unreachable vs SD-Core.** | Credibility gap; core is the ceiling. | **§1 correction:** report sim-capacity (mock AMF) separately from integration numbers; **5k UEs / 10 aps is the stated SD-Core baseline**; soften the claim to an architectural style. |
| **Stale/incorrect conformance-bug framing** — omec #672/#673 patched (v2.2.1); CVE-2026-42082 is free5gc not SD-Core. | Selling regression tests as bug-finding; miscited CVE. | **P6 reframed** as graceful-rejection/regression guards; verify the CVE applies to the omec fork before citing; note handover (M-1) dependency of timing tests. |
| **NGAP PPID wire trap** (`0x3c000000` vs 60). | AMF drops the association. | **PPID = 60 set and asserted in the Phase-0 smoke test.** |
| **GTP-U header-size error** (8 vs 12 bytes with an extension header). | Malformed N3 packets; UPF drops. | **§2.2 corrected:** E-flag forces the 4-byte optional field → 12-byte header before the PDU Session Container; enforced in Phase-1b. |
| **D-3 single point of failure + incomplete framing.** | Stub strategy (§5c) collapses; hidden NGAP-container encoding debt. | **D-3 split** into AMF-opacity + NGAP-transparent-container encode checks; contingency budgeted if either surprises. |
| **AGPL contagion from UERANSIM.** | Legal / must open-source or license. | **Clean-room from specs**; UERANSIM as reading reference only; 5G-AKA/Milenage from TS 35.206. Enforced from P0. |
| **APER interop quirks** — valid-looking Go the AMF silently drops. | Silent, hard-to-debug failures. | **Test every new NGAP message vs the live core early** — Phase-1a exit + deterministic golden-bytes fixture. |
| **SCTP lib fragility** (no tags, ~23 issues, Linux-only). | Build breakage / cross-platform loss. | **Fork+pin at P0** (done: `bgrewell/sctp` @ 19ddcbc); document Linux-only constraint; D-10 validates streams/multi-homing before scale. |
| **Kernel-module (gtp5g) CI friction.** | CI unreliability. | Default stays userspace; gtp5g opt-in behind a build flag; **pin kernel (Ubuntu 22.04 HWE, 5.15) in perf integration-CI only.** |
| **Crypto correctness** (AKA, NAS cipher, SUCI). | Opaque "core rejects everything" failures. | Reuse `free5gc/nas security/` for NAS crypto; SUCI via §5f; concentrated in 1a (AKA) and P3 (SUCI), off the keystone. |
| **Dormant-dependency risk** (`songgao/water`, `wmnsk/go-gtp` license ambiguity). | Silent breakage / license surprise. | **Fork+pin `water` like sctp/go-gtp** (done: `bgrewell/water` @ 2b4b6d7); **go-gtp license verified MIT at P0 (2026-07-02).** |
| **Observability backpressures a hot path** — synchronous logging on the data/control plane slows the thing being measured. | Distorted KPIs; the tool becomes the bottleneck. | **Invariant (enforced in `internal/observability`):** per-packet/per-UE volume uses metrics (lock-free atomics) + batched-drop OTLP, never logs; hot-path logs are Debug and short-circuited by the level gate; any high-frequency log sink is fronted by a bounded drop-on-full handler (lands with the data plane, P1b). |
| **Latency-measurement accuracy** — NTP jitter (1–10 ms) can dominate. | Misleading KPIs. | Choose a time-sync strategy (PTP/PHC or documented NTP error bars) at P5; report measurement uncertainty with every KPI. |
| **free5gc library API drift** (marked "not latest in module"). | Silent breakage on update. | Pin exact versions + Dependabot; adapter packages isolate blast radius. |
| **Credential handling in the API** — real IMSI/Ki/OPc over gRPC. | Secret leakage. | TLS-by-default; `slog` redaction of `Key`/`OPc`; throw-away test subscriber ranges in CI; API not externally exposed. Designed in at P0. |
| **OCUDU/srsRAN transition** could later offer a higher-fidelity backend. | Build-vs-reuse could shift. | The `EngineBackend` interface (§5b) keeps backends pluggable behind the unchanged scenario contract. |

---

## Addendum: loom as the traffic / performance engine

Decided: rather than build the "native per-UE traffic generator" (§6 Phase 5) and
lean on iperf3, ORBIT **embeds loom as a Go library** — the maintainer's own
distributed traffic generator (`github.com/bgrewell/loom`, source at
`~/repos/bgrewell/loom`), in active development, so any API/behavior ORBIT needs
is driven there rather than worked around. Reuse win, consistent with §4.

loom provides: scenario-driven flows (a timeline of `from`/`to`/`start`/`stop`/`repeat`),
datapaths `tcp`/`udp`/**`afxdp`** (kernel-bypass), application-emulation *shapes*
(`https-browse`/`voip-call`/`ssh-session`/`prometheus-sender`), rate control and
stop conditions, and structured telemetry (throughput/latency/jitter). Its own
`loomd` agent + `loomctl` controller design is also a useful precedent for ORBIT's
engine↔API↔CLI split.

Where it lands:
- **Reuse table (§4)** — loom moves into the reuse column (embed as library,
  `github.com/bgrewell/loom`); the "native traffic generator" BUILD row is struck.
- **Phase 1b / Data-path Mode A** — verification flows call loom (tcp/udp sourced
  from the UE's tunnel IP) instead of iperf3 (replaces iperf3 in D-4/D-5).
- **Phase 5 (performance/load)** — loom *is* the per-UE traffic generator and the
  throughput/latency/jitter surface. ORBIT maps per-UE traffic profiles → loom
  flows sourced from each UE's data interface, and feeds loom telemetry into the
  per-procedure KPI/SLO engine.
- **Raw N6 / UPF max-throughput** — loom's **AF_XDP** datapath (NIC-to-NIC) hammers
  the UPF's data-network side at line rate — a different axis from per-UE-over-GTP
  (which uses tcp/udp bound to the UE IP).

Open items:
- **Embedding surface** — confirm (or grow, in loom) a stable Go API for in-process
  flow control + telemetry callbacks, so ORBIT drives loom as a library, not a
  subprocess.
- **Per-UE binding spike** (new, alongside D-5) — can loom source-bind many
  concurrent flows to many local UE tunnel IPs from one process, or must we isolate
  per UE netns? Resolve before Phase-5 scale.
