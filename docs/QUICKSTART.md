# ORBIT quickstart

From nothing to a UE registered and a scenario running. This is the on-ramp;
[USAGE.md](USAGE.md) is the reference, and [DESIGN.md](DESIGN.md) is the why.

If you have no core to point at, skip to [the LXD
testbed](#appendix-no-core-yet) — it builds one.

---

## 1. What you need from the core operator

ORBIT talks to a real core, so it needs the same things any gNB would, plus
credentials for the subscribers it will simulate. You write them down once, as
a **target** (§5), and every command and scenario takes them from there with
`-t <name>` ([targets.md](targets.md) is the reference):

| | Target field | Example |
|---|---|---|
| AMF N2 endpoint (SCTP) | `core.amf` | `<amf-host>:38412` |
| PLMN | `core.plmn` | `001` / `01` |
| TAC | `core.tac` | `1` |
| Slice (S-NSSAI) | `core.slices` | `1` / `010203` |
| DNN | `core.dnn` | `internet` |
| Test SUPI range | `subscribers[].first_supi`, `count` | `001010100007500`, 100 |
| Subscriber keys | `subscribers[].ki` / `opc` | 32 hex digits each |
| Your N2 / N3 addresses | `ran.n2_source_ips` / `ran.n3_ips` | one per simulated gNB |
| The N6 far end (§7) | `app.responders` | `<n6-host>:9551` |

Keep the keys out of the file: `orbit target init` writes `${ORBIT_KI}` and
`${ORBIT_OPC}` in their place, and they expand from your shell when the target
loads.

```sh
export ORBIT_KI=... ORBIT_OPC=...
# export ORBIT_SERVER=http://<host>:8412   # only when serve is not on loopback
```

TAC, SST and DNN default to `1`, `1` and `internet`; SD has no default, so give
`--sd 010203` for SD-Core. ORBIT has no default PLMN: a wrong one fails NG
Setup in a way that looks like a core problem. Every command prints the
identity it used as `network: plmn=001/01 tac=1 snssai=1/010203`.

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
  or one address per gNB in the target's `ran.n2_source_ips`.

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

## 5. Describe the core, then smoke-test it

**Write the target.** `init` runs one NG Setup through the server, so from the
host runs execute on, and records the N2 source address it used:

```sh
orbit target init lab1 --amf <amf-host>:38412 --mcc 001 --mnc 01 --sd 010203 \
    --first-supi 001010100007500 --count 100 --gnb-n3 <your-n3-ip> \
    --responder n6=<n6-host>:9551,<n6-host>
# NG Setup accepted by AMF "AMF" from <your-n2-ip>:… (gNB ID 1, the first of the default range)
# wrote target lab1 to ~/.config/orbit/targets/lab1.yaml
```

**Probe it.** NG Setup, one registration, one PDU session, through the server;
it stops at the first failure and names what to check:

```sh
orbit target probe lab1
# ✓ NG Setup     ok  accepted by AMF "AMF" from …
# ✓ register     ok  001010100007500 registered in …
# ✓ PDU session  ok  UE IP 192.168.100.x via UPF …
```

**Run against it.** Every command takes the target with `-t`, and a flag you
type still overrides one field:

```sh
orbit conformance -t lab1
orbit ue register -t lab1 --pdu-session
orbit ue ping    --supi 001010100007500 --dst 8.8.8.8 --count 3
orbit ue latency --supi 001010100007500 --target 8.8.8.8 --probes 10
orbit ue stats   --supi 001010100007500
```

`--gnb-n3` (in the target, `ran.n3_ips`) is **not optional** if you want to send
traffic: without it you get `registered without a gNB N3 address; data path
disabled`. `--explain` on any of these prints where each value came from.

**Real application traffic** against the N6 far end (see §7):

```sh
orbit ue app http  -t lab1 --supi 001010100007500 --duration 10s
orbit ue app voip  -t lab1 --supi 001010100007500 --duration 10s
orbit ue app video -t lab1 --supi 001010100007500 --duration 10s
```

## 6. Runs — and why the dashboard is empty until you start one

The commands above are ad-hoc: they act immediately and create no *run*. The
dashboard watches **runs**, so it shows nothing until one exists. `orbit runs
list` returning an empty table is the tell.

**A load run** — a rate-controlled attach storm:

```sh
orbit runs start-load -t lab1 --count 100 --rate 10
```

For **CI**, `orbit load` runs the same storm in the foreground and gates on an
SLO, exiting non-zero on breach — which is what makes it usable as a build
step. The gate lives here rather than on `runs start-load`, because a run that
outlives its client has nobody to return an exit status to:

```sh
orbit load -t lab1 --count 100 --rate 10 \
    --slo-min-success 0.99 --slo-reg-p99 500ms --slo-attach-p99 1s
```

**A fleet run** — a population with continuous behaviours, from a scenario
file:

```sh
orbit runs start-fleet examples/fleet-testbed.yaml -t lab1
```

**Watch it**, from the terminal or the browser:

```sh
orbit runs watch            # live one-line summary
orbit runs events           # attach milestones, failures, handovers
orbit runs get <run-id>     # full snapshot: cohorts, flows, latencies
```

While a fleet run is going, `runs get` shows the UEs attached right now.
Once it has completed, it shows the end-of-run report instead: the attach
totals overall and per gNB, handovers, traffic and deregistrations. The live
count has fallen to zero by then, because every UE deregistered.

…or open `http://<host>:8412/`.

## 7. The far end for application traffic

`ue app` and fleet app cohorts need a loom agent on the **N6** side to talk to.
ORBIT ships one:

```sh
orbit responder --bind <n6-host>:9551
```

Run it on a host in the data network, and add it to the target
(`--responder` on `target init`, or `app.responders` in the file); commands and
scenarios then use it without `--peer`. Stock `loomd` works too and is
interchangeable.

## 8. Writing a scenario

The shipped examples are annotated and are the fastest way in:

| File | What it shows |
|---|---|
| `examples/fleet-testbed.yaml` | a small fleet with an http cohort and a latency probe |
| `examples/fleet-testbed-mobility.yaml` | a gNB grid with mobility and handover |
| `examples/fleet-testbed-heavy.yaml` | a mixed http/video/voip population |
| `examples/fleet-testbed-staged.yaml` | cohorts joining over time (`start_after`) |
| `examples/fleet-testbed-ramp.yaml` | load escalating through the run |

A minimal one. It holds only the workload; the core, keys, gNB IDs, addresses
and the far end come from the target (`-t lab1`, or a `target: lab1` line):

```yaml
kind: fleet
name: first-fleet

topology:
  gnbs:
    count: 1

fleet:
  count: 20
  attach_rate: 5/s
  pdu_session: true

behaviors:
  traffic:
    mix:
      - { app: http, name: web, share: 1.0,
          params: { object_size: 256KB, think: "200ms" } }

run:
  duration: 120s
```

```sh
orbit run first-fleet.yaml -t lab1
```

## 9. Things that will bite you

- **Use a fresh gNB ID per run.** The AMF does not cleanly re-key a reused gNB
  ID from a new association. Until the server leases IDs from the target's
  range, commands use its first ID; pass `--gnb-id` to pick another.
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

It writes a target named `lxd` on its RAN nodes, so everything above works
there with `-t lxd` (start with `orbit target probe lxd`). Its
[README](../testing/lxd-testbed/README.md) documents the layout.
