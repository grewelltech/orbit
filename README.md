# ORBIT

**Open Radio Benchmark and Integration Testbed.**

ORBIT is a from-scratch, Go-native 5G SA RAN + UE simulator and test harness for
**benchmarking, conformance-checking, and integration-testing a real 5G core**.
It speaks the actual control and user planes — NGAP over SCTP (N2), NAS-5GS (N1),
and GTP-U (N3) — with the radio replaced by a software model instead of an RF
PHY. That lets it drive genuine signaling and user data against the core under
test while scaling to a large fleet of simulated devices.

## What it does

- **Attach & user plane** — a simulated UE completes Registration → 5G-AKA →
  Security Mode → PDU session with a real allocated IP, and carries
  bidirectional user data over N3 (GTP-U).
- **Scale-out** — many UEs multiplexed over one N2 association per gNB, across
  multiple gNBs, with a bounded-concurrency attach scheduler.
- **Emulated mobility** — synthesized RSRP/RSRQ over a trajectory model drives
  A3/A4/A5 measurement events (TS 38.331), which trigger **real N2 *and* Xn
  handover** against the core — no radio required.
- **Load / performance** — rate-controlled attach storms with per-procedure
  latency KPIs (P50/P99/P99.9) and an SLO gate, plus per-UE traffic and
  throughput / latency / jitter via [loom](https://github.com/bgrewell/loom),
  embedded as a library.
- **Conformance / regression** — a structured, spec-cited harness that asserts
  the core rejects malformed input gracefully (crash-safety regression guards),
  with a machine-readable CI gate.
- **API-first** — a Connect (gRPC + gRPC-Web + JSON/REST) API with a CLI that
  only ever calls the API, plus structured logs, Prometheus metrics, OTLP
  traces, and live state streaming.

## Status

**Phases 0–6 complete** and live-verified against an Aether SD-Core testbed:
control-plane attach, N3 data path, multi-gNB scale-out and slicing, N2 and Xn
handover, the full load/perf suite, and the conformance harness. The grounded
design, phased plan, discovery spikes, and risks are in
**[docs/DESIGN.md](docs/DESIGN.md)**; day-to-day usage is in
**[docs/USAGE.md](docs/USAGE.md)**.

## Honest scope

ORBIT reports two different numbers and never conflates them:

- **Sim capability** — what ORBIT's own engine sustains, measured against an
  in-process mock AMF: **~1,350 attach/s** with sub-100 ms registration P99.
- **Integration capability** — bounded by the *core under test*. Against the
  SD-Core testbed the attach ceiling is **~10 attach/s** (the core serializes
  attaches; raising ORBIT's concurrency doesn't move it), consistent with
  SD-Core's stated ~5,000-UE / 10-attach-per-second envelope.

"VIAVI/Spirent-class" describes an *architectural style* here, not a throughput
claim — the core is the ceiling, and ORBIT is built to attribute limits to the
core rather than to itself.

## What it has found

Driving a real core end to end surfaces real interop issues. Two examples,
documented with root cause in **[docs/interop/sdcore.md](docs/interop/sdcore.md)**:

- **N2 handover has no user-plane continuity on SD-Core** — the SMF can't decode
  a spec-conformant `HandoverRequestAcknowledgeTransfer` (an omec type-generation
  bug: a spec-OPTIONAL field marked mandatory), so the downlink never switches.
- **Xn handover *does* complete with data continuity** on the same core — its
  transfer type is tagged correctly. ORBIT demonstrates both, and pinpoints the
  difference.

ORBIT stays strictly 3GPP/X.691-conformant by default; core-specific workarounds
live behind opt-in, documented `--core-profile` quirks, never in the codecs.

## Build

```sh
make build      # -> bin/orbit
```

## Quickstart

Run the API server, register a UE (with a data session), and carry traffic:

```sh
./bin/orbit serve &                                  # API on 127.0.0.1:8412

./bin/orbit ue watch --supi 208930100007500 &        # live FSM/mobility stream
./bin/orbit ue register \
    --amf 172.17.50.11:38412 \
    --supi 208930100007500 --ki <hex> --opc <hex> \
    --mcc 208 --mnc 93 --sst 1 --sd 010203 \
    --gnb-id 1 --pdu-session --gnb-n3 <ip-reachable-from-upf>
# -> UE 208930100007500 registered=true; PDU session: UE IP 192.168.100.x

./bin/orbit ue ping    --supi 208930100007500 --dst 8.8.8.8
./bin/orbit ue latency --supi 208930100007500 --target 8.8.8.8      # RTT/jitter/loss (loom)
./bin/orbit ue traffic --supi 208930100007500 --target 8.8.8.8:9999 --rate 20Mbps
```

Handover, a rate-controlled load storm with an SLO gate, and the conformance
gate:

```sh
./bin/orbit ue xn-handover --supi 208930100007500 --amf 172.17.50.11:38412 \
    --gnb-id 2 --bind 172.17.50.13:0 --gnb-n3 172.17.50.13     # or: ue handover (N2)

./bin/orbit load --amf 172.17.50.11:38412 --base-imsi 208930100007500 --count 100 \
    --ki <hex> --opc <hex> --mcc 208 --mnc 93 --rate 20 --slo-min-success 0.99

./bin/orbit conformance --amf 172.17.50.11:38412 --json
```

The CLI is a Connect client of the API (`--server` selects the endpoint), except
the direct-drive `load` / `conformance` benchmarks. **The user plane and handover
must run where the UPF's N3 is reachable** (on the ATB-01 testbed, the RAN node),
and handover needs a distinct source IP and a fresh gNB ID per gNB — see
[docs/USAGE.md](docs/USAGE.md).

## Layout

| Path | What |
|---|---|
| `proto/`, `gen/` | API schema (Protobuf) and generated Connect/Go code (`make gen`). |
| `internal/sctp`, `internal/ngap`, `internal/nas`, `internal/gtpu`, `internal/datapath` | Thin adapters over the pinned transport/codec substrate + the N3 data path. |
| `internal/gnb` | gNB role — NG Setup, UE-associated procedures, PDU sessions, N2/Xn handover. |
| `internal/ue`, `internal/ue/auth` | UE identity, SUCI, NAS-5GS builders, 5G-AKA / key derivation. |
| `internal/engine` | Attach FSM, session manager, fleet, mobility, load driver. |
| `internal/meas` | Measurement synthesis (RSRP model, A3/A4/A5 events). |
| `internal/load`, `internal/loomgtp` | Load engine + SLO/KPIs, and the loom traffic bridge over GTP-U. |
| `internal/conformance` | Spec-cited conformance / regression harness. |
| `internal/mockamf` | In-process mock AMF for headless sim-capacity measurement. |
| `internal/server`, `internal/cli` | Connect API façade and the cobra CLI. |
| `internal/observability` | slog (trace-correlated, credential-redacting), OTLP tracing, Prometheus. |

## Testing

```sh
make test          # unit-CI: headless, no core required
make integration   # integration-CI: needs a live core (ORBIT_AMF_N2 to point at the AMF)
```

## License

[Apache-2.0](LICENSE).
