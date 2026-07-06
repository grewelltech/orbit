# ORBIT

**Open Radio Benchmark and Integration Testbed.**

ORBIT is a Go-native 5G SA RAN + UE simulator and test harness for
**benchmarking, conformance-checking, and integration-testing a real 5G core**.
It speaks the actual control and user planes — NGAP over SCTP (N2), NAS-5GS (N1),
and GTP-U (N3) — with the radio replaced by a software model instead of an RF
PHY. So you can drive genuine signaling and user data against the core you're
testing, and scale to a large fleet of simulated devices, without any radios.

## What you can do with it

- **Attach UEs and carry data.** A simulated UE completes Registration →
  5G-AKA → Security Mode → PDU session with a real allocated IP, and sends
  bidirectional user data over N3.
- **Scale out.** Many UEs multiplexed over one N2 association per gNB, across
  multiple gNBs, with a bounded-concurrency attach scheduler.
- **Move UEs around.** Synthesized RSRP/RSRQ over a trajectory drives A3/A4/A5
  measurement events, which trigger real **N2 and Xn handover** against the
  core — no radio required.
- **Load- and performance-test.** Rate-controlled attach storms with
  per-procedure latency percentiles and an SLO gate, plus per-UE traffic and
  throughput / latency / jitter (traffic generation via
  [loom](https://github.com/bgrewell/loom), embedded as a library).
- **Check conformance.** A spec-cited harness that asserts the core rejects
  malformed input gracefully, with a machine-readable pass/fail CI gate.
- **Automate all of it.** Everything is a Connect (gRPC / gRPC-Web / JSON-REST)
  API call; the CLI is just a client. Structured logs, Prometheus metrics, OTLP
  traces, and a live state-event stream come built in.

## What to expect

ORBIT reports two different numbers and keeps them separate, so you always know
whether a limit is yours or the core's:

- **Sim capability** — what ORBIT's own engine sustains (measured against an
  in-process mock): **~1,350 attach/s**, sub-100 ms registration P99.
- **Integration capability** — bounded by the *core under test*. Against an
  Aether SD-Core testbed the attach ceiling is **~10 attach/s** — the core
  serializes attaches, and turning up ORBIT's concurrency doesn't move it.

The point isn't a big headline number; it's honest attribution — ORBIT is built
so the ceiling you hit is the core's, not the tool's.

## Install

```sh
make build      # -> bin/orbit
```

You'll need a reachable 5G core (an AMF's N2/SCTP endpoint) and subscriber
credentials provisioned in it (`Ki`/`OPc`, PLMN, slice, DNN).

## Quickstart

Start the API server, register a UE with a data session, and use the user plane:

```sh
./bin/orbit serve &                                  # API on 127.0.0.1:8412

./bin/orbit ue watch --supi 208930100007500 &        # live state stream
./bin/orbit ue register \
    --amf 172.17.50.11:38412 \
    --supi 208930100007500 --ki <hex> --opc <hex> \
    --mcc 208 --mnc 93 --sst 1 --sd 010203 \
    --gnb-id 1 --pdu-session --gnb-n3 <ip-reachable-from-upf>
# -> UE 208930100007500 registered=true; PDU session: UE IP 192.168.100.x

./bin/orbit ue ping    --supi 208930100007500 --dst 8.8.8.8
./bin/orbit ue latency --supi 208930100007500 --target 8.8.8.8       # RTT / jitter / loss
./bin/orbit ue traffic --supi 208930100007500 --target 8.8.8.8:9999 --rate 20Mbps
```

Hand a UE over, run a rate-controlled load storm with an SLO gate, or run the
conformance suite:

```sh
./bin/orbit ue xn-handover --supi 208930100007500 --amf 172.17.50.11:38412 \
    --gnb-id 2 --bind 172.17.50.13:0 --gnb-n3 172.17.50.13      # or: ue handover (N2)

./bin/orbit load --amf 172.17.50.11:38412 --base-imsi 208930100007500 --count 100 \
    --ki <hex> --opc <hex> --mcc 208 --mnc 93 --rate 20 --slo-min-success 0.99

./bin/orbit conformance --amf 172.17.50.11:38412 --json
```

> **One thing to know up front:** the user plane and handover need the gNB's N3
> address reachable *from the UPF*, so run those from a host on the core's access
> network and pass `--gnb-n3 <that-ip>`. Full details, every command, and its
> flags are in **[docs/USAGE.md](docs/USAGE.md)**.

## Documentation

- **[docs/USAGE.md](docs/USAGE.md)** — the practical guide: topology, every
  command with its flags, and common workflows.
- **[docs/DESIGN.md](docs/DESIGN.md)** — architecture, design decisions, and
  rationale.
- **[docs/interop/sdcore.md](docs/interop/sdcore.md)** — interop issues ORBIT
  has surfaced in SD-Core (with root cause), and the opt-in `--core-profile`
  workarounds. ORBIT stays strictly 3GPP/X.691-conformant by default; any
  core-specific quirk is opt-in and documented, never baked into the codecs.

## Contributing

Package layout for orientation:

| Path | What |
|---|---|
| `proto/`, `gen/` | API schema (Protobuf) and generated Connect/Go code (`make gen`). |
| `internal/{sctp,ngap,nas,gtpu,datapath}` | Transport/codec adapters and the N3 data path. |
| `internal/gnb`, `internal/ue` | gNB and UE roles (procedures, PDU sessions, handover, SUCI, 5G-AKA). |
| `internal/engine` | Attach FSM, session manager, fleet, mobility, load driver. |
| `internal/{meas,load,loomgtp}` | Measurement synthesis, load engine + KPIs, and the loom traffic bridge. |
| `internal/conformance` | Spec-cited conformance / regression harness. |
| `internal/{server,cli,observability}` | API façade, CLI, and observability. |

```sh
make test          # unit tests: headless, no core required
make integration   # integration tests: needs a live core (ORBIT_AMF_N2 points at the AMF)
```

## License

[Apache-2.0](LICENSE).
