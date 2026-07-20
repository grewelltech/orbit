# ORBIT usage

How to drive ORBIT against a live 5G core. For the architecture and design
rationale see [DESIGN.md](DESIGN.md); for known core-side issues and the
`--core-profile` quirks see [interop/sdcore.md](interop/sdcore.md).

## Prerequisites

- A reachable 5G SA core (an AMF's N2/SCTP endpoint), and subscriber
  credentials provisioned in the core: `Ki` and `OPc` (32 hex digits each) for
  the SUPIs you use, plus the core's PLMN (`--mcc`/`--mnc`), TAC, and slice
  (`--sst`/`--sd`) and DNN.
- For the **user plane and handover**, a host on the core's N3 access network
  (see "Where things must run" below).
- `make build` → `bin/orbit`.

## Where things must run (topology)

- **Control plane** (attach, status, handover signalling) works from any host
  that can reach the AMF's N2 endpoint.
- **User plane** (`ping`, `traffic`, `latency`) and the **handover downlink**
  need the gNB's N3 address to be reachable *from the UPF*. Register with
  `--gnb-n3 <ip>` set to an address on the UPF's access network; the UPF returns
  downlink there. On a split testbed (e.g. ATB-01) that address lives on the RAN
  node, not the control-plane host, so run those commands from there.
- **Handover** additionally needs each gNB on a **distinct routed source IP**
  (`--bind <ip>:0`) — the AMF distinguishes gNBs by association address — and a
  **fresh gNB ID per run**: the AMF does not cleanly re-key a reused gNB ID from
  a new address (a stale association makes it drop the Handover Request). See
  [interop/sdcore.md](interop/sdcore.md).

## The server

Most commands are Connect API clients; start the server first:

```sh
orbit serve --listen 127.0.0.1:8412 [--log-level info] [--core-profile strict-3gpp]
```

- `/metrics` (Prometheus) and `/healthz` are served on the same listener.
- `--core-profile` selects core-compatibility quirks. Default `strict-3gpp`
  (byte-exact 3GPP, zero quirks). Use `sdcore` to enable the documented N2
  handover-transfer workaround for SD-Core; the set of quirks a core needs is a
  conformance signal, not silent tuning.
- `--server <url>` on every client command selects the endpoint (default
  `http://127.0.0.1:8412`).

### Run as a service

To avoid backgrounding `orbit serve`, install it as a systemd service. One
script does install, upgrade, and uninstall (needs root):

```sh
sudo bash scripts/orbit.sh install       # from a checkout, or straight from GitHub:
curl -fsSL https://raw.githubusercontent.com/grewelltech/orbit/main/scripts/orbit.sh | sudo bash -s -- install

sudo bash scripts/orbit.sh upgrade       # rebuild/replace the binary + restart
sudo bash scripts/orbit.sh uninstall     # add --purge to also remove config
sudo bash scripts/orbit.sh status
```

It installs the `orbit` binary to `/usr/local/bin`, a unit at
`/etc/systemd/system/orbit.service`, and config at `/etc/orbit/orbit.env` — set
`ORBIT_ARGS` there (e.g. `--listen 0.0.0.0:8412 --core-profile sdcore`), then
`systemctl restart orbit`. Manage it with `systemctl {status,restart,stop}
orbit` and follow logs with `journalctl -u orbit -f`. (`make install-service` /
`upgrade-service` / `uninstall-service` wrap the script.) Install needs Go to
build, or set `VERSION=` to download a release.

## UE lifecycle

```sh
# Attach one UE; add --pdu-session (+ --gnb-n3) for a data session.
orbit ue register --amf <host:port> --supi <imsi> --ki <hex> --opc <hex> \
    --mcc 208 --mnc 93 --sst 1 --sd 010203 --gnb-id 1 \
    --pdu-session --dnn internet --gnb-n3 <ip-reachable-from-upf>

orbit ue status     --supi <imsi>
orbit ue list
orbit ue watch      [--supi <imsi>]      # live StateStream: FSM + mobility events
orbit ue deregister --supi <imsi>        # UE-originated switch-off deregistration
```

`watch` streams lifecycle states (`REGISTERING`, `AUTHENTICATED`,
`SECURITY_ESTABLISHED`, `REGISTERED`, `SESSION_ACTIVE`, `DEREGISTERED`) and
mobility states (`HANDOVER_STARTED`, `PATH_SWITCH_COMPLETE`, `HANDED_OVER`,
`HANDOVER_FAILED`). Mobility events are also logged by `orbit serve` at info
level with timestamps and gNB/UE identifiers.

## User-plane data

Run these from a host on the UPF access network (see topology above).

```sh
orbit ue ping    --supi <imsi> --dst 8.8.8.8 --count 3            # ICMP over N3
orbit ue latency --supi <imsi> --target 8.8.8.8 --probes 20       # RTT/jitter/loss (loom)
orbit ue traffic --supi <imsi> --target 8.8.8.8:9999 \            # throughput (loom)
    --rate 20Mbps --packet-size 1200 --duration-ms 5000
orbit ue stats   --supi <imsi>                                    # per-QFI UL/DL counters
```

`traffic` and `latency` embed loom over the UE's GTP-U tunnel: `traffic`
reports bytes/packets and achieved Mbps; `latency` reports sent/received/lost,
loss %, and min/mean/max RTT plus jitter. `--rate` empty means unlimited.
`stats` reads the tunnel's per-QFI uplink/downlink packet and byte counters
(cumulative since the data path opened; empty until the first
ping/traffic/latency run touches it).

## Application traffic (VoIP / MOS)

`orbit ue app voip` places a real RTP/RTCP call from a registered UE through
its GTP-U data path to a **stock loomd agent** (loom ≥ v0.10) on the N6
network, and scores it with the ITU-T G.107 E-model:

```sh
# On the N6 box (once): loomd --token $TOKEN
orbit ue app voip --supi <imsi> --peer <n6-host>:9551 \
    --codec g711 --ptime 20ms --jb 40 --duration 60s [--json]
```

`--peer` is loomd's control address on the **management** network; if the N6
media address differs, add `--peer-data-ip`. A server-level default can be
set once at startup with `orbit serve --loom-agent <n6-host>:9551
[--loom-token $TOKEN]`, after which `--peer`/`--token` may be omitted
per call (per-call values override the defaults). The CLI streams one line per
interval from *both* ends of the call — MOS/R, jitter, loss, jitter-buffer
discard, RTT, and one-way delay labeled with its method and error bar
(`owd 0.61±0.05ms (timesync)`) — with correlation events (handover phases,
GTP-U End Markers) printed inline as they arrive, then a both-end report with
media-gap summaries and the annotated event timeline. The exit code is
non-zero when the call fails, e.g. a media handshake timeout because RTP
cannot reach the N6 box (the firewall must allow loomd's control port from
the management network and the RTP port range from the UPF's N6 subnet).

## Mobility (handover)

The UE must already be registered (ideally with a session). Use a distinct
`--bind`/`--gnb-n3` and a fresh `--gnb-id` for the target.

```sh
# N2 (AMF-mediated) handover:
orbit ue handover    --supi <imsi> --amf <host:port> \
    --gnb-id 2 --bind 172.17.50.13:0 --gnb-n3 172.17.50.13

# Xn handover (NGAP PathSwitch; Xn prep stubbed in-process):
orbit ue xn-handover --supi <imsi> --amf <host:port> \
    --gnb-id 2 --bind 172.17.50.13:0 --gnb-n3 172.17.50.13
```

Both move the UE's session to the target gNB and emit mobility states on
`watch`; Xn additionally emits `PATH_SWITCH_COMPLETE` when the AMF's
PathSwitchRequestAcknowledge arrives (the downlink cutover point — N2 has no
RAN-visible equivalent). On SD-Core, **Xn** completes with user-plane continuity; **N2**
completes the control-plane handover but the downlink does not follow — an open
upstream SMF bug (not the decode issue the `sdcore` profile fixes). The
[N2 example](../examples/attach-and-handover-n2.yaml) lays out all three N2
issues; see also [interop/sdcore.md](interop/sdcore.md).

## Load / performance

`orbit load` is a direct-drive benchmark (it does not go through the API
server). It reports **sim** vs **integration** capability separately — run it
against a mock for the former and the real core for the latter.

```sh
# Rate-controlled attach storm with an SLO gate (non-zero exit on breach):
orbit load --amf <host:port> --base-imsi 208930100007500 --count 100 \
    --ki <hex> --opc <hex> --mcc 208 --mnc 93 \
    --rate 20 --concurrency 64 \
    --slo-min-success 0.99 --slo-reg-p99 3s

# Linear ramp instead of a fixed rate ("find the knee"):
orbit load ... --ramp 5:80:30          # 5→80 attach/s over 30s

# Multi-gNB muxing and per-UE data sessions:
orbit load ... --gnb-count 4 --pdu-session --gnb-n3 <ip>

# Soak: sustain for a duration with a resource trend:
orbit load ... --duration 10m --sample-interval 15s
```

Output is per-procedure latency (P50/P99/P99.9), achieved rate, success/failure
counts, and (for soak) a goroutine/RSS trend. The `--slo-*` flags turn a run
into a CI gate.

## Conformance / regression

`orbit conformance` runs the spec-cited regression suite in-process against a
live core and exits non-zero on any failure — an integration-CI gate.

```sh
orbit conformance --amf <host:port> [--json] [--category negative-ie] \
    [--gnb-base 0x400] [--per-test-timeout 15s]
```

Checks are framed as graceful-rejection / crash-safety regression guards, each
with a 3GPP citation; `--json` emits machine-readable results with per-check
verdict, expected/observed, and the spec reference.

## Scenarios (`orbit run`)

Instead of long, repetitive command lines, describe a whole test declaratively
in YAML and run it: `orbit run scenario.yaml`. The runner is an ordinary API
client (it drives the same operations as the `ue` commands), so it needs a
running server (`orbit serve`).

A scenario declares the **core**, **gNBs**, and **UEs** once, then an ordered
**steps** list references them. Secrets use `${ENV}` references so they stay out
of the file:

```yaml
name: attach-and-handover
core:
  amf: 172.17.50.11:38412
  plmn: { mcc: "208", mnc: "93" }
  tac: 1
  slice: { sst: 1, sd: "010203" }
  dnn: internet
credentials:
  ki:  ${ORBIT_KI}          # export ORBIT_KI / ORBIT_OPC before running
  opc: ${ORBIT_OPC}
gnbs:
  - { id: 1, name: gnb-1, n3: 172.17.50.12 }
  - { id: 2, name: gnb-2, n3: 172.17.50.13, bind: 172.17.50.13:0 }
ues:
  - { supi: "208930100007500", gnb: gnb-1, pdu_session: true }
  - range: { base: "208930100007501", count: 3 }   # 501, 502, 503
    gnb: gnb-1
steps:
  - register: all                                            # or a single SUPI
  - ping:     { ue: "208930100007500", dst: 8.8.8.8 }
  - latency:  { ue: "208930100007500", target: 8.8.8.8, probes: 20 }
  - traffic:  { ue: "208930100007500", target: 8.8.8.8:9999, rate: 20Mbps, duration: 5s }
  - handover: { ue: "208930100007500", to: gnb-2, type: xn }  # or type: n2
  - deregister: all
```

Steps run in order and stop at the first failure (a step fails on an RPC error,
or on a natural assertion like a ping with zero replies). `wait: 5s` pauses
between steps. A ready-to-edit example is in
[`examples/attach-and-handover-xn.yaml`](../examples/attach-and-handover-xn.yaml)
(and an N2 variant,
[`attach-and-handover-n2.yaml`](../examples/attach-and-handover-n2.yaml)).
The same topology rules apply — data-plane and handover steps must run where the
UPF's N3 is reachable, with a fresh gNB ID per run.

### Fleet mode (preview)

A `kind: fleet` scenario ([`examples/fleet.yaml`](../examples/fleet.yaml))
*generates* a topology of gNBs and a UE population running continuous behaviours
(mobility + a traffic mix) for a duration, rather than listing explicit steps —
for scale / soak / mass-mobility runs (design in
[docs/adr/0004-fleet-population-mode.md](adr/0004-fleet-population-mode.md)).
`source_ips` are addresses the host owns, one per gNB. `orbit run` on a fleet
file prints the plan, **attaches the population** across the gNBs, then runs
**mobility** — each mobile UE moves along a track across the gNB grid and hands
over (Xn) when a neighbour cell becomes stronger by the a3-offset (RSRP/A3
model), not on a timer — and **traffic** (one loom flow per non-mobile UE over
its gNB's shared N3 socket) for `run.duration`, then reports (attach / handovers
/ traffic / deregister).

## The API

The server exposes a Connect API (gRPC, gRPC-Web, and JSON/REST on one port).
The CLI is a thin client of it; the same operations are reachable directly, e.g.
over JSON:

```sh
curl -s http://127.0.0.1:8412/orbit.v1.UEService/List \
    -H 'Content-Type: application/json' -d '{}'
```

Services: `UEService` (Register, Deregister, Status, List, Ping, Traffic,
Latency, Handover, XnHandover, StateStream, DataStats, StartApp, AppStream,
StopApp), `CellService` (RunNGSetup),
`SystemService` (GetInfo). Schema in [`proto/orbit/v1`](../proto/orbit/v1);
regenerate the Go/Connect bindings with `make gen`.

## Testing

```sh
make test          # unit-CI: headless, no core required
make integration   # integration-CI: needs a live core; ORBIT_AMF_N2 overrides the AMF
```

Integration tests are `//go:build integration`-tagged. The user-plane and
handover integration tests read `ORBIT_TEST_KI`/`ORBIT_TEST_OPC` and expect to
run from a host on the UPF access network with distinct routed source IPs.
