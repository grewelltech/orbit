# ORBIT usage

How to drive ORBIT against a live 5G core. New here? Start with
[QUICKSTART.md](QUICKSTART.md) — this page is the reference. For the
architecture and design rationale see [DESIGN.md](DESIGN.md); for known
core-side issues and the `--core-profile` quirks see
[interop/sdcore.md](interop/sdcore.md).

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
  (`--bind <ip>`) — the AMF distinguishes gNBs by association address — and a
  **fresh gNB ID per run**: the AMF does not cleanly re-key a reused gNB ID from
  a new address (a stale association makes it drop the Handover Request). See
  [interop/sdcore.md](interop/sdcore.md).

### Choosing the N2 source address (`--bind`)

Every command that opens an N2 association — `cell ngsetup`, `ue register`,
`ue handover` / `xn-handover`, `load`, `runs start-load` and `conformance` —
takes `--bind <ip[:port]>`: the local address the gNB's SCTP association is
bound to. A bare IP means an ephemeral port (`10.0.0.21` is `10.0.0.21:0`); an
explicit port is kept as given; IPv6 is written bare (`fd00::21`, zoned
`fe80::1%eth0`) or bracketed with a port (`[fd00::21]:0`); a hostname is
resolved at dial time. Unset, the association is bound to the single address
the host routes to the AMF from (with an ephemeral port), so the INIT
advertises one address and the AMF sees exactly that; the `n2-source=` line
reports it. Use `--bind` when the gNB should use a different address (a RAN
host with several N2 addresses) or a fixed port. A host with no route to the
AMF fails at dial with an error that names `--bind`; ORBIT never falls back to
advertising every host address.

**Port rule:** a fixed (non-zero) port is legal only when exactly one
association will use that `ip:port`; port 0 is always legal because every dial
gets a fresh ephemeral port. So a `--bind` shared by several `load` gNBs must
be a bare IP or `ip:0`, two gNBs may not name the same fixed `ip:port`, and
`conformance --bind` must be port 0 because each check dials afresh and the
liveness probe opens a second association from the same address. These are
rejected before anything is dialled.

`--bind` is the **N2** source; the user plane has its own flags (`--gnb-n3` on
the UE and load commands, `--n3-bind` on `conformance`). Each command prints
the N2 source it actually used as `n2-source=<ip:port>`, with the real port,
so a capture or an AMF-side log can be matched to the run. `conformance --json`
records the configured bind as `N2Source` in `ip:0` form: every check dials its
own association on its own ephemeral port, so there is no single port to show.

`load` and `runs start-load` take a list: none (route-selected source), one (shared by
every gNB, which leaves the AMF telling them apart by gNB ID only — the command
says so), or exactly `--gnb-count` addresses, one per gNB in order. Any other
count is an error. Addresses are never auto-incremented from a base; list the
ones the host owns, as fleet scenarios do with `topology.gnbs.source_ips`.

```sh
orbit cell ngsetup --amf <host:port> --mcc 001 --mnc 01 --bind 172.17.50.21
orbit ue register  ... --bind 172.17.50.21 --gnb-n3 172.17.50.21
orbit load         ... --gnb-count 3 --bind 172.17.50.21,172.17.50.22,172.17.50.23
orbit conformance  --amf <host:port> --bind 172.17.50.21 --json
```

`cell ngsetup --local` is the old name for the same flag; it still works but is
deprecated, and `--bind` wins if both are given.

### Network identity (`--mcc --mnc --tac --sst --sd --dnn`)

Every command that dials a core takes the same identity flags with the same
meaning, help and defaults, and prints the identity it used on one line:

```
network: plmn=001/01 tac=1 snssai=1/010203 dnn=internet
```

- **PLMN is required and never guessed.** `--mcc` (3 digits) and `--mnc` (2 or
  3 digits, leading zeros kept) come from the flags or, when both are absent,
  from `$ORBIT_MCC`/`$ORBIT_MNC`, exactly as the keys fall back to
  `$ORBIT_KI`/`$ORBIT_OPC`. Neither present is one error naming both sources.
  A wrong PLMN produces an NG Setup Failure that reads like a core finding,
  which is why the tool carries no default.
- **Defaults for the rest:** `--tac 1`, `--sst 1`, `--sd ""` (no slice
  differentiator), `--dnn internet` (where a PDU session is involved). SD-Core
  wants `--sd 010203`; pass it explicitly, as every example here does. The
  values are whatever your core is configured with.
- **Handover inherits.** `ue handover` / `xn-handover` take the same flags but
  with no defaults and no environment fallback: anything left unset is copied
  from the UE's serving gNB (`$ORBIT_MCC`/`$ORBIT_MNC` are ignored here, since
  the target is in the serving PLMN by definition), so a same-network handover
  needs only `--gnb-id`, `--bind` and `--gnb-n3`. A field given explicitly
  overrides the inherited one.
- **Scenario files** need `core.plmn.mcc` and `core.plmn.mnc` (quote them, so
  `"01"` is read as the string it is; `${ORBIT_MCC}`/`${ORBIT_MNC}` expand);
  `core.tac`, `core.slice.sst` and `core.dnn` take the defaults above when
  omitted.
- `conformance --json` records the identity as `Network{MCC, MNC, TAC, SST,
  SD}` next to `N2Source`, so a verdict says which network it was taken under.

```sh
export ORBIT_MCC=001 ORBIT_MNC=01
orbit cell ngsetup --amf <host:port> --sd 010203                 # PLMN from the environment
orbit ue register  --amf <host:port> --supi <imsi> --sd 010203 --gnb-id 1
orbit ue xn-handover --supi <imsi> --amf <host:port> --gnb-id 2 --bind <ip> --gnb-n3 <ip>  # inherits
```

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
- `--server <url>` on every client command selects the endpoint. When the flag
  is omitted it comes from `$ORBIT_SERVER` the same way keys come from
  `$ORBIT_KI` / `$ORBIT_OPC`; when that is unset too, `http://127.0.0.1:8412`.
  An explicit flag always wins. The value must be an absolute `http://` or
  `https://` URL (a bare `host:port` is rejected, naming where it came from).
  When the environment chose the server, the command says so on stderr before
  its first request: `server: http://10.127.0.21:8412 (from $ORBIT_SERVER)`.
  `serve`, `load`, `conformance`, `responder`, `version` and `run` with a
  `kind: fleet` scenario never read it (fleet mode drives the engine
  in-process; the step form of `run` is an API client and does), and
  `serve --listen` keeps its loopback default.

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
build, or set `VERSION=` to download a release. When `--listen` is not
loopback, export `ORBIT_SERVER=http://<listen>` in the shell you run client
commands from, so they find the service without `--server` on every call.

## UE lifecycle

```sh
# Attach one UE; add --pdu-session (+ --gnb-n3) for a data session.
# --ki/--opc may be omitted when $ORBIT_KI/$ORBIT_OPC are exported (every
# command that takes credentials falls back to them).
orbit ue register --amf <host:port> --supi <imsi> --ki <hex> --opc <hex> \
    --mcc 001 --mnc 01 --sst 1 --sd 010203 --gnb-id 1 \
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

All UEs on one gNB share a single N3 socket (one bind per gNB N3 address);
each UE stamps its own TEID/QFI on uplink and the downlink is demultiplexed
back to it by TEID, so any number of UEs on the same gNB can run data
commands — and hold app sessions — concurrently.

## Application traffic (VoIP / MOS)

`orbit ue app voip` places a real RTP/RTCP call from a registered UE through
its GTP-U data path to a **stock loomd agent** (loom ≥ v0.10) on the N6
network, and scores it with the ITU-T G.107 E-model:

```sh
# On the N6 box (once), either:
loomd --token $TOKEN                                   # stock loom agent
orbit responder --bind 0.0.0.0:9551 --token $TOKEN     # same agent, orbit binary

orbit ue app voip --supi <imsi> --peer <n6-host>:9551 \
    --codec g711 --ptime 20ms --jb 40 --duration 60s [--json]
```

`orbit responder` hosts loom's agent inside the ORBIT binary, so a run needs
only one binary (ADR-0007). It is interchangeable with stock `loomd` — same
control plane, same app engines — and the two are chosen freely per
deployment. `--bind` is **required and has no default**: the responder is a
remotely-aimable traffic generator, so its reachability is always stated
explicitly. Binding a routable address without `--token` warns loudly and
leaves the control plane open to anyone who can reach it.

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
One call per UE at a time; UEs on the same gNB call concurrently over the
gNB's shared N3 socket, and a call survives that UE's handover (TEID rebind,
with the UPF's End Marker on the old path reported as a correlation event).

## Application traffic (HTTP / video QoE)

`orbit ue app http` and `orbit ue app video` run real TCP applications from a
registered UE: loom's HTTP/1.1 (+ optional TLS 1.3 / HTTP/2) client, and its
ABR video player, ride a per-gNB userspace TCP/IP stack over the UE's GTP-U
tunnel — real SYN and TLS ClientHello bytes travel as inner IP packets — to
the HTTPOrigin a **stock loomd agent** (loom ≥ v0.11) serves on the N6
network. The video far end *is* the http origin (loom's player is
client-only): one `loomd` serves voip, http, and video sessions alike.

```sh
# Web objects in a think-time loop; TTFB/goodput/error interval lines:
orbit ue app http  --supi <imsi> --peer <n6-host>:9551 \
    --object-size 100KB --think 200ms..2s --duration 60s \
    [--url-path /object/65536] [--objects N] [--tls [--h2] --tls-insecure] \
    [--port-min 40200 --port-max 40210] [--json]

# A generated HLS ladder under the ABR buffer model; buffer/stall/startup lines:
orbit ue app video --supi <imsi> --peer <n6-host>:9551 \
    --ladder 240p:400k,480p:1200k,720p:2500k --duration 180s \
    [--seg-duration 4s] [--segments 150] [--abr throughput|buffer] \
    [--start-threshold 2s] [--buffer-target 12s] [--rebuffer-target 4s] \
    [--tls ...] [--port-min ...] [--json]
```

`--peer`/`--token`/`--peer-data-ip` and the server-level defaults work exactly
as for voip. Parameters travel verbatim to **both** ends (each engine reads
only the keys it documents), so `--ladder`/`--seg-duration`/`--segments`
configure the origin's generated manifest *and* pin the player's expectation —
one grammar, both sides. The firewall matrix extends accordingly: the origin's
data port (advertised back as `media port` in the report) must be reachable
from the UPF's N6 subnet; pin it with `--port-min`/`--port-max` like the RTP
range.

The `http` CLI streams one line per interval from both ends — requests,
errors, TTFB p95, goodput, plus TCP-connect/TLS-handshake means when new
connections were opened (the origin end serves counts and goodput only; it
measures no client-side latencies) — and the report carries the whole-run
percentiles. The `video` CLI streams buffer level, interval bitrate, stall
count/time, and startup delay; the report summarizes startup, stalls with
their timestamped events, rebuffer ratio, and average bitrate.

**TLS, honestly:** the origin generates a self-signed certificate per flow,
and loom's control plane does not expose it remotely (`CertificatePEM` is an
in-process accessor; the Configure response carries only the data port), so
orbit cannot pin a remote origin's certificate for you — and neither can you:
every session Configures a *new* flow with a *new* certificate, so no PEM
obtained out of band ever matches, and a pinned session would pass setup and
then burn its whole duration on 100%-error samples. orbit therefore **refuses
`--tls-ca`** (and a `tls_ca` param in fleet cohorts) up front. Use plain
HTTP/1.1, or `--tls --tls-insecure` (an explicit lab-only opt-in; verification
is off, which is the only working TLS path to a stock loomd origin today).
The flag returns when loom can hand out the per-flow origin certificate over
the control plane.

**Handover semantics (stated, not hidden):** an *intra*-gNB handover swaps
TEIDs below the TCP stack — established connections survive and see only
delay/loss. An *inter*-gNB handover moves the UE's address between gNB stacks,
which **aborts** connections live during the move (the `TCP_CONNS_RESET`
correlation event names the count); the http client's next request reconnects
via the target gNB, and the video player retries the failed segment — visible
as a TTFB spike or a stall, which is the realism the tool exists to measure.
Handover correlation covers the TCP apps too: annotations join the handover
with buffer drain → stall → ABR downshift (video) or error-burst/TTFB-spike
evidence (http), the latter labeled *coincident* — temporal overlap, not
causal proof.

## Mobility (handover)

The UE must already be registered (ideally with a session). Use a distinct
`--bind`/`--gnb-n3` and a fresh `--gnb-id` for the target.

```sh
# N2 (AMF-mediated) handover:
orbit ue handover    --supi <imsi> --amf <host:port> \
    --gnb-id 2 --bind 172.17.50.13 --gnb-n3 172.17.50.13

# Xn handover (NGAP PathSwitch; Xn prep stubbed in-process):
orbit ue xn-handover --supi <imsi> --amf <host:port> \
    --gnb-id 2 --bind 172.17.50.13 --gnb-n3 172.17.50.13
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
    --ki <hex> --opc <hex> --mcc 001 --mnc 01 --sd 010203 \
    --rate 20 --concurrency 64 \
    --slo-min-success 0.99 --slo-reg-p99 3s

# Linear ramp instead of a fixed rate ("find the knee"):
orbit load ... --ramp 5:80:30          # 5→80 attach/s over 30s
orbit load ... --rate 175 --arrival poisson   # exponential inter-arrivals

`--arrival` selects the arrival process for the offered rate. The default
`constant` meters arrivals evenly, which is the easy case for a core: a uniform
175/s never presents the transient bursts a real 175/s does. `poisson` draws
exponentially distributed inter-arrival times about the same mean, which is how
independent subscriber attaches actually arrive. Near saturation the difference
is material — measured against SD-Core at an offered 175/s, registration P50 was
190 ms under `constant` and 244 ms under `poisson`, the same load presented
honestly. Set `ORBIT_LOAD_SEED` to make the arrival sequence reproducible.

# Multi-gNB muxing and per-UE data sessions:
orbit load ... --gnb-count 4 --pdu-session --gnb-n3 <ip>

# One N2 source address per gNB (or one shared address; see "Choosing the N2
# source address" above):
orbit load ... --gnb-count 2 --bind 172.17.50.21,172.17.50.22

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
orbit conformance --amf <host:port> [--bind <ip[:port]>] [--json] \
    [--category negative-ie] [--gnb-base 0x400] [--per-test-timeout 15s]
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
  plmn: { mcc: "001", mnc: "01" }      # quoted, so "01" stays "01"
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

#### App cohorts (real application traffic at fleet scale)

Traffic mix entries with `app:` instead of `profile:` declare **application
cohorts**: their share of the fleet runs loom's real protocol engines
(`voip` — RTP/RTCP with G.107 MOS; `http` — HTTP/TLS over the per-gNB gVisor
netstack; `video` — the ABR player against the http origin) through their
GTP-U data paths to a stock `loomd` on the N6 network, exactly like
`orbit ue app …` but for a population:

```yaml
behaviors:
  traffic:
    mix:
      - { profile: web, share: 0.7 }                       # synthetic flows
      - { app: voip, name: voip-calls, share: 0.2, peer: 172.17.60.10:9551,
          params: { codec: pcmu, port_min: "40000", port_max: "40100" } }
      - { app: http, name: web-real, share: 0.1, peer: 172.17.60.10:9551,
          params: { object_size: 64KB, think: 200ms, tls: "true", tls_insecure: "true" } }
```

Cohorts need `fleet.pdu_session: true`; `peer` is the loomd control address
(`token`, `peer_data_ip`, and `params` match the `orbit ue app` grammar; the
firewall matrix above applies unchanged). The far-end plumbing is shared, not
multiplied: **one control connection and one TimeSync loop per distinct
loomd** across the whole fleet, **one shared http origin per cohort** (the
origin serves any number of clients), and **one voip answerer per member** —
the answerer latches a single source, so a voip cohort must fit inside its
`port_min..port_max` range (an oversized cohort is refused up front with the
required range). TCP cohorts share each gNB's single gVisor netstack; voip
rides the lightweight dgram path, so fleet voice never pays the gVisor cost.
RTCP reporting is randomized per RFC 3550 §6.3/A.7 inside loom, so thousands
of concurrent calls do not synchronise into report storms.

Cohort results are **distributions** (p5/p50/p95 across members): MOS for
voip, TTFB and goodput for http, stall time and rebuffer ratio for video —
printed in the fleet report and, with `orbit run --metrics-listen host:port`,
served live on `/metrics` as `orbit_fleet_app_*` gauges labeled
`{cohort, q}` (cohort names, never SUPIs — bounded cardinality).

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
