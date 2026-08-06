# ORBIT quickstart

From nothing to a UE registered and a scenario running. This is the on-ramp;
[USAGE.md](USAGE.md) is the reference, and [DESIGN.md](DESIGN.md) is the why.

If you have no core to point at, skip to [the LXD
testbed](#appendix-no-core-yet) — it builds one.

---

## 1. What you need from the core operator

ORBIT talks to a real core, so it needs the same six things any gNB would, plus
credentials for the subscribers it will simulate.

| | Flag | Example |
|---|---|---|
| AMF N2 endpoint (SCTP) | `--amf` | `10.102.0.10:38412` |
| PLMN | `--mcc` / `--mnc` | `001` / `01` |
| TAC | `--tac` | `1` |
| Slice (S-NSSAI) | `--sst` / `--sd` | `1` / `010203` |
| DNN | `--dnn` | `internet` |
| Test SUPI range | `--supi` / `--base-imsi` | `001010100007500`… |
| Subscriber keys | `--ki` / `--opc` | 32 hex digits each |

Keys come from `$ORBIT_KI` / `$ORBIT_OPC` when the flags are omitted, which is
how to keep them out of shell history and scenario files:

```sh
export ORBIT_KI=... ORBIT_OPC=...
```

**The SUPIs must already be provisioned in the core.** An unprovisioned one
fails with `AMF rejected registration`, which reads like a broken core rather
than an exhausted subscriber list — so if attach fails, check the range first.

## 2. Where each command has to run

This trips people up more than anything else, so it is worth reading once.

- **Control plane** — `cell ngsetup`, `ue register`, handovers — works from any
  host that can reach the AMF's N2 endpoint.
- **User plane** — `ue ping`, `latency`, `traffic`, `app` — needs the gNB's N3
  address to be reachable **from the UPF**, because that is where the UPF sends
  downlink. Register with `--gnb-n3 <ip>` set to an address on the UPF's access
  network, and run those commands from the host that owns it.
- **Handover** additionally needs a **distinct source IP per gNB**, since the
  AMF keys a gNB's association on its address — `--bind` on `orbit ue handover`,
  or `topology.gnbs.source_ips` in a fleet scenario.

See [interop/sdcore.md](interop/sdcore.md) for the core-side quirks behind
these rules.

## 3. Build

```sh
make build          # → bin/orbit
make ui             # embeds the dashboard; skip it and `serve` serves a placeholder
```

Cross-compiling for a separate RAN node:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o orbit ./cmd/orbit
```

## 4. Start the server

Most commands are clients of an ORBIT API server, which also serves the
dashboard on the same port.

```sh
orbit serve --listen 127.0.0.1:8412
```

Then either run `orbit …` on that host, or point at it with `--server`:

```sh
orbit --server http://<host>:8412 ue list
alias orbit='orbit --server http://<host>:8412'   # if you are driving it remotely
```

> **Warning:** the API is unauthenticated and carries subscriber credentials.
> Bind it to loopback unless the network is private to your lab.

## 5. Smoke test, in order

Each step proves one layer. Do them in order — if step 2 fails, step 3 cannot
work.

**Is the core there?** One NGAP exchange over SCTP:

```sh
orbit cell ngsetup --amf 10.102.0.10:38412 \
    --mcc 001 --mnc 01 --tac 1 --sst 1 --sd 010203 --gnb-id 1
# NG Setup accepted by AMF "AMF"
```

**Attach one UE.** Real 5G-AKA, NAS security, registration, then a PDU session:

```sh
orbit ue register --amf 10.102.0.10:38412 --supi 001010100007500 \
    --mcc 001 --mnc 01 --sst 1 --sd 010203 \
    --gnb-id 2 --pdu-session --gnb-n3 10.103.0.20
# UE 001010100007500 registered=true (AMF-UE-NGAP-ID …)
#   PDU session: UE IP 192.168.100.x via UPF 10.103.0.100
```

`--gnb-n3` is **not optional** if you want to send traffic: without it you get
`registered without a gNB N3 address; data path disabled`.

**Does data move?** This is genuine GTP-U through the UPF, not a loopback:

```sh
orbit ue ping    --supi 001010100007500 --dst 8.8.8.8 --count 3
orbit ue latency --supi 001010100007500 --target 8.8.8.8 --probes 10
orbit ue stats   --supi 001010100007500
```

**Real application traffic** against an N6 far end (see §7):

```sh
orbit ue app http  --supi 001010100007500 --peer 10.106.0.30:9551 --duration 10s
orbit ue app voip  --supi 001010100007500 --peer 10.106.0.30:9551 --duration 10s
orbit ue app video --supi 001010100007500 --peer 10.106.0.30:9551 --duration 10s
```

## 6. Runs — and why the dashboard is empty until you start one

The commands above are ad-hoc: they act immediately and create no *run*. The
dashboard watches **runs**, so it shows nothing until one exists. `orbit runs
list` returning an empty table is the tell.

**A load run** — a rate-controlled attach storm:

```sh
orbit runs start-load --amf 10.102.0.10:38412 \
    --mcc 001 --mnc 01 --sst 1 --sd 010203 \
    --base-imsi 001010100007500 --count 100 --rate 10 --gnb-id 10
```

For **CI**, `orbit load` runs the same storm in the foreground and gates on an
SLO, exiting non-zero on breach — which is what makes it usable as a build
step. The gate lives here rather than on `runs start-load`, because a run that
outlives its client has nobody to return an exit status to:

```sh
orbit load --amf 10.102.0.10:38412 \
    --mcc 001 --mnc 01 --sst 1 --sd 010203 \
    --base-imsi 001010100007500 --count 100 --rate 10 --gnb-id 11 \
    --slo-min-success 0.99 --slo-reg-p99 500ms --slo-attach-p99 1s
```

**A fleet run** — a population with continuous behaviours, from a scenario
file:

```sh
orbit runs start-fleet examples/fleet-testbed.yaml
```

**Watch it**, from the terminal or the browser:

```sh
orbit runs watch            # live one-line summary
orbit runs events           # attach milestones, failures, handovers
orbit runs get <run-id>     # full snapshot: cohorts, flows, latencies
```

…or open `http://<host>:8412/`.

## 7. The far end for application traffic

`ue app` and fleet app cohorts need a loom agent on the **N6** side to talk to.
ORBIT ships one:

```sh
orbit responder --bind 10.106.0.30:9551
```

Run it on a host in the data network, and pass that address as `--peer`. Stock
`loomd` works too and is interchangeable.

## 8. Writing a scenario

The shipped examples are annotated and are the fastest way in:

| File | What it shows |
|---|---|
| `examples/fleet-testbed.yaml` | a small fleet with an http cohort and a latency probe |
| `examples/fleet-testbed-mobility.yaml` | a gNB grid with mobility and handover |
| `examples/fleet-testbed-heavy.yaml` | a mixed http/video/voip population |
| `examples/fleet-testbed-staged.yaml` | cohorts joining over time (`start_after`) |
| `examples/fleet-testbed-ramp.yaml` | load escalating through the run |

A minimal one:

```yaml
kind: fleet
name: first-fleet

core:
  amf: 10.102.0.10:38412
  plmn: { mcc: "001", mnc: "01" }
  tac: 1
  slice: { sst: 1, sd: "010203" }
  dnn: internet

# Used by the local `orbit run <file>` path, which expands ${ENV} itself.
# `orbit runs start-fleet` ignores this block and takes credentials from
# --ki/--opc or $ORBIT_KI/$ORBIT_OPC instead: the server never expands a
# client's ${ENV}, or it would leak its own environment back.
credentials:
  ki: ${ORBIT_KI}
  opc: ${ORBIT_OPC}

topology:
  gnbs:
    count: 1
    id_base: 100
    source_ips: [10.102.0.20]     # N2 source, one per gNB
    n3_ips:     [10.103.0.20]     # N3 data plane; omit if the same interface

fleet:
  count: 20
  supi_base: "001010100007500"
  attach_rate: 5/s
  pdu_session: true

behaviors:
  traffic:
    mix:
      - { app: http, name: web, share: 1.0, peer: 10.106.0.30:9551,
          params: { object_size: 256KB, think: "200ms" } }

run:
  duration: 120s
```

## 9. Things that will bite you

- **Use a fresh `--gnb-id` per run.** The AMF does not cleanly re-key a reused
  gNB ID from a new association.
- **Stay inside the provisioned SUPI range**, or attach fails with what looks
  like a core problem.
- **One active run per kind.** Starting a second fleet run is refused; stop the
  first with `orbit runs stop <run-id>`.
- **Ad-hoc `ue` commands never appear on the dashboard.** They create no run.
- **`make ui` before `make build`** if you want the dashboard embedded.

---

## Appendix: no core yet?

`testing/lxd-testbed/` builds one — three VMs on separated N2/N3/N6 networks
with Aether SD-Core deployed, subscribers provisioned, and ORBIT and the
responder installed:

```sh
cd testing/lxd-testbed
./testbed.sh all            # ~20 minutes from nothing
```

It prints the coordinates to use, and its
[README](../testing/lxd-testbed/README.md) documents the layout. Everything
above works against it unchanged.
