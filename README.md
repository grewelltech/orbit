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
make build              # -> bin/orbit
```

You'll need a reachable 5G core (an AMF's N2/SCTP endpoint) and subscriber
credentials provisioned in it (`Ki`/`OPc`, PLMN, slice, DNN).

## Run the server

The CLI talks to a local API server. Install it as a service so it's always
running — one script does install, upgrade, and uninstall:

```sh
sudo bash scripts/orbit.sh install    # or: curl -fsSL …/scripts/orbit.sh | sudo bash -s -- install
systemctl status orbit                # listening on 127.0.0.1:8412
journalctl -u orbit -f                # logs
# later: sudo bash scripts/orbit.sh upgrade   |   sudo bash scripts/orbit.sh uninstall
```

Config (listen address, `--core-profile`, log level) lives in
`/etc/orbit/orbit.env`. For a throwaway run you can also just foreground it:
`orbit serve`.

## Quickstart — run a scenario (preferred)

The clearest way to drive ORBIT is a declarative YAML scenario: declare the
core, gNBs, and UEs once, then a list of steps. Point the example at your
testbed, export your credentials, and run it:

```sh
export ORBIT_KI=... ORBIT_OPC=...
orbit run examples/attach-and-handover-xn.yaml
```

```
▶ attach-and-handover (1 UEs, 6 steps)
✓ [1] register — 1 UE(s) registered
✓ [2] ping — …: 3/3 replies from 8.8.8.8 (5.0 ms)
✓ [3] latency — …: 20/20 replies, rtt 5.20 ms, jitter 0.33 ms
✓ [4] handover — … → gnb-2 (Xn): HANDED_OVER
✓ [5] ping — …: 3/3 replies from 8.8.8.8          # the user plane follows the handover
✓ [6] deregister — 1 UE(s) deregistered
```

That attaches a UE, proves the data path, hands it over via **Xn**, and proves
the flow survives — verified end to end on SD-Core. There's also an
[N2 example](examples/attach-and-handover-n2.yaml); note N2 handover completes on
the control plane but SD-Core does not carry the downlink across it (an upstream
SMF bug — [docs/interop/sdcore.md](docs/interop/sdcore.md)), so **Xn is the path
for mobility with data continuity**. Full scenario reference in
[docs/USAGE.md](docs/USAGE.md).

## Or drive it with individual commands

Every scenario step is also a command:

```sh
orbit ue register --amf 172.17.50.11:38412 --supi 208930100007500 \
    --ki <hex> --opc <hex> --mcc 208 --mnc 93 --sst 1 --sd 010203 \
    --gnb-id 1 --pdu-session --gnb-n3 <ip-reachable-from-upf>
orbit ue ping    --supi 208930100007500 --dst 8.8.8.8
orbit ue latency --supi 208930100007500 --target 8.8.8.8
orbit ue traffic --supi 208930100007500 --target 8.8.8.8:9999 --rate 20Mbps
orbit ue xn-handover --supi 208930100007500 --amf 172.17.50.11:38412 \
    --gnb-id 2 --bind 172.17.50.13:0 --gnb-n3 172.17.50.13   # or: ue handover (N2)

orbit load --amf 172.17.50.11:38412 --base-imsi 208930100007500 --count 100 \
    --ki <hex> --opc <hex> --mcc 208 --mnc 93 --rate 20 --slo-min-success 0.99
orbit conformance --amf 172.17.50.11:38412 --json
```

> **One thing to know up front:** the user plane and handover need the gNB's N3
> address reachable *from the UPF*, so run those from a host on the core's access
> network. Full details, every command, and its flags are in
> **[docs/USAGE.md](docs/USAGE.md)**.

## Documentation

- **[docs/USAGE.md](docs/USAGE.md)** — the practical guide: topology, every
  command with its flags, and common workflows.
- **[docs/DESIGN.md](docs/DESIGN.md)** — architecture, design decisions, and
  rationale.
- **[docs/adr/](docs/adr/)** — Architecture Decision Records: the *why* behind
  significant, hard-to-reverse choices.
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
