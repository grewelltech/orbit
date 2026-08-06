# ORBIT

**Open Radio Benchmark and Integration Testbed.**

ORBIT is a 5G SA RAN + UE simulator and test harness. It stands in for the radio
network and the devices — the **gNBs** (5G base stations) and the **UEs**
(phones) — so you can **benchmark, conformance-check, and integration-test a real
5G core** with no radio hardware. It speaks the real 5G protocols on the wire
(NGAP/N2, NAS/N1, GTP-U/N3); only the radio itself is replaced by software. So
the core you're testing sees genuine signaling and user traffic, and you can
scale from a single device to a large fleet.

## What you can do with it

- **Attach UEs and carry data.** A simulated UE completes Registration →
  5G-AKA → Security Mode → PDU session with a real allocated IP, and sends
  bidirectional user data over N3.
- **Scale out.** Many UEs multiplexed over one N2 association per gNB, across
  multiple gNBs, with a bounded-concurrency attach scheduler.
- **Move UEs between cells.** A built-in mobility/signal model triggers real
  **N2 and Xn handovers** against the core — no radio required.
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

ORBIT is a CLI (`orbit`) that talks to a small local API server. The easiest
setup installs both and runs the server as a background service — one script
does install, upgrade, and uninstall (needs `sudo`):

```sh
curl -fsSL https://raw.githubusercontent.com/grewelltech/orbit/main/scripts/orbit.sh | sudo bash -s -- install
# or, from a checkout of this repo:
sudo bash scripts/orbit.sh install
```

Then:

```sh
systemctl status orbit                    # the server, running on 127.0.0.1:8412
journalctl -u orbit -f                    # follow its logs
sudo bash scripts/orbit.sh upgrade        # update to a newer build
sudo bash scripts/orbit.sh uninstall      # remove it
```

Settings (listen address, core profile, log level) live in `/etc/orbit/orbit.env`.

**Prefer to build it yourself?** `make build` produces `bin/orbit`, and
`orbit serve` runs the server in the foreground (no service needed).

Either way, you'll need a reachable 5G core (an AMF's N2/SCTP endpoint) and
subscriber credentials provisioned in it — `Ki`/`OPc`, PLMN, slice, and DNN.
(Don't have these? They come from whoever runs the core under test.)

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
orbit ue stats   --supi 208930100007500   # per-QFI UL/DL counters
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

## Run a fleet test on a single-node SD-Core

Just brought SD-Core up on one box and want to point a crowd of UEs at it? Here's
the whole loop. Run ORBIT on the same node (or any host that can reach the core's
N2 and N3).

**1 — Install ORBIT and aim it at the core.**

```sh
curl -fsSL https://raw.githubusercontent.com/grewelltech/orbit/main/scripts/orbit.sh | sudo bash -s -- install
echo 'ORBIT_ARGS="--listen 127.0.0.1:8412 --core-profile sdcore"' | sudo tee /etc/orbit/orbit.env
sudo systemctl restart orbit
systemctl status orbit          # active (running) on 127.0.0.1:8412
```

The `sdcore` profile turns on the documented N2 handover-transfer workaround.

**2 — Grab two things from your SD-Core install:**

- the **AMF N2 address** — the SCTP endpoint the gNB dials, `<amf-ip>:38412`
  (your AMF service / NodePort);
- the **subscribers you provisioned** — `Ki`/`OPc`, PLMN (`mcc`/`mnc`), slice
  (`sst`/`sd`), DNN, and a block of IMSIs.

**3 — Give each gNB an address the UPF can reach.** The one networking rule:
a gNB's N3 `source_ip` must be an IP the UPF sends downlink back to — on a single
node, an address on the UPF's *access* network (not loopback). Mobility moves UEs
between gNBs, so use at least two; add a second local IP if needed:

```sh
sudo ip addr add <gnb2-ip>/<prefix> dev <access-iface>
```

**4 — Write `fleet.yaml`.** ORBIT generates the gNBs and the UE population from it:

```yaml
kind: fleet
name: single-node-smoke
core:
  amf: <amf-ip>:38412
  plmn: { mcc: "208", mnc: "93" }     # your core's values
  tac: 1
  slice: { sst: 1, sd: "010203" }
  dnn: internet
credentials: { ki: ${ORBIT_KI}, opc: ${ORBIT_OPC} }   # read from the env, kept out of the file
topology:
  gnbs:
    count: 2
    id_base: 1                        # bump this on each re-run (see notes)
    source_ips: [<gnb1-ip>, <gnb2-ip>]
fleet:
  count: 20
  supi_base: "208930100007500"        # first IMSI; UEs count up from here
  attach_rate: 5/s                     # SD-Core serialises attaches — keep it modest
  pdu_session: true
behaviors:
  mobility: { model: random_walk, speed: 3m/s, handover: xn }
  traffic:  { mix: [ { profile: video, share: 1.0, rate: 5Mbps } ] }
run: { duration: 60s }
```

**5 — Launch it:**

```sh
export ORBIT_KI=<hex> ORBIT_OPC=<hex>
orbit run fleet.yaml
```

```
▶ single-node-smoke
  core:     <amf-ip>:38412  PLMN 208/93
  topology: 2 gNBs (ids 1–2), grid, source IPs <gnb1-ip>, <gnb2-ip>
  fleet:    20 UEs (208930100007500…), even across gNBs, attach 5/s, PDU sessions
  mobility: random_walk @ 3m/s, xn handover
  traffic:  100% video (5Mbps)
  run:      60s

attaching 20 UEs across 2 gNBs, then running behaviours for 60s…

attach:       20/20 in 4.1s
handovers:    10 ok, 0 failed
traffic:      10 flow(s) (per UE, shared N3 socket per gNB), 375.0 MB total
deregistered: 20
```

That attaches 20 UEs, hands half of them between the two gNBs over **Xn** (data
intact), runs a video flow on the rest, then deregisters — all against the real
core.

**Single-node notes:**

- **N3 reachability is the thing to get right.** 0 replies in traffic almost
  always means a `source_ip` the UPF can't route back to — not an ORBIT bug.
- **Use a fresh `id_base` per run.** SD-Core doesn't cleanly re-key a reused gNB
  ID from a new association, so bump it (1 → 3 → …) if you run again.
- Fleet handover uses **Xn**, which carries data across the handover on SD-Core;
  plain **N2** currently doesn't — see
  [docs/interop/sdcore.md](docs/interop/sdcore.md).
- Start small and scale `count` / `attach_rate` up once it's clean.

## Run it split: RAN and core on different machines

The same loop works when ORBIT and the core run on separate machines — the
usual shape past a smoke test (a `core` node running SD-Core, a `ran` node
running ORBIT). Everything above carries over; what changes is where things
run and which addresses you use. Verified end to end on a two-node SD-Core
(RKE2, bess-UPF) with the core node on two networks — a management network
carrying the AMF's N2 endpoint, and an access network carrying the UPF's N3
interface — and the RAN node joined to both:

```
core node:  N2 net 10.32.2.10  ← AMF 10.32.2.10:38412 (SCTP, service external IP)
            access net 10.32.3.10, UPF N3 at 10.32.3.241 (macvlan)
ran  node:  N2 net 10.32.2.20
            access net 10.32.3.20 + gNB IPs 10.32.3.120, 10.32.3.121
```

**1 — Install ORBIT on the RAN node** (step 1 above, unchanged).

**2 — Put the gNB addresses on the access network.** The single-node rule
becomes the whole game here: each gNB's `source_ip` is used for **both** its
N2 association (the SCTP source the AMF sees) and its N3 traffic (GTP-U with
the UPF). So every gNB IP must be an address the RAN node owns **on the
core's access network** — the UPF sends downlink GTP-U straight to it. One
IP per gNB, added on the access-network interface:

```sh
sudo ip addr add 10.32.3.120/24 dev <access-iface>
sudo ip addr add 10.32.3.121/24 dev <access-iface>
```

(Persist them in netplan/systemd-networkd if the testbed reboots.) The N2 leg
needs no extra setup: SCTP from an access-net source to an AMF on the
management net is plain IP routing, and works as long as the core node sits
on the access network too (it routes the return path directly; note strict
per-interface `rp_filter` on the core node would drop this asymmetry —
Linux defaults are fine).

**3 — Point `fleet.yaml` at the remote core.** Same file as step 4 above with
the remote addresses in: `core.amf: 10.32.2.10:38412`,
`source_ips: [10.32.3.120, 10.32.3.121]`. Nothing else changes.

**4 — Prove the path with one UE before the fleet** (from the RAN node):

```sh
orbit ue register --amf 10.32.2.10:38412 --supi <imsi> --ki $ORBIT_KI --opc $ORBIT_OPC \
    --mcc 208 --mnc 93 --sst 1 --sd 010203 --gnb-id 90 --pdu-session --gnb-n3 10.32.3.120
orbit ue ping --supi <imsi> --dst 8.8.8.8    # 3/3 replies = N2, N3, and routing all good
orbit ue deregister --supi <imsi>
```

Then `orbit run fleet.yaml` exactly as before.

**Split-deployment notes:**

- **Attach succeeds but 0 replies** → the UPF can't reach your `source_ip`.
  Watch the access network from the core node (`tcpdump -i <access-iface>
  udp port 2152`): uplink G-PDUs arriving with no downlink coming back means
  the UPF-side session state or return route is the problem, not ORBIT.
- **Data path dies while the control plane stays green** (attach still works,
  pings that used to work return nothing) — on SD-Core's bess-UPF check for
  `bessd` container restarts: `measure_flow: true` in the UPF config
  segfaults bessd under per-flow stats reads, and the rebuilt datapath comes
  back empty while PFCP keeps ACKing. Set `measure_flow: false` and restart
  `upf-0` — Finding 5 in [docs/interop/sdcore.md](docs/interop/sdcore.md).
- The UE pool (e.g. `192.168.100.0/24`) lives behind the UPF; you never route
  it on the RAN node. ORBIT carries UE traffic inside GTP-U on the gNB IPs.

## Documentation

- **[docs/QUICKSTART.md](docs/QUICKSTART.md)** — the on-ramp: what you need from
  the core operator, a smoke test in order, and a first scenario.
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
