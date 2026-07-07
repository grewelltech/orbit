# ADR-0004: A fleet/population mode for large dynamic scenarios

- Status: Accepted
- Date: 2026-07-07

## Context

ADR-0003's entities+steps scenarios are explicit and sequential — ideal for
scripted small-N tests, but the wrong abstraction for a large *dynamic
population*. The motivating ask: **~100 gNBs, ~10,000 UEs, UEs moving between
cells, running a mix of traffic, for a duration.** The steps model can't express
this — it would need tens of thousands of enumerated `handover`/`traffic` steps,
and steps run one-at-a-time. What's needed instead is: *generate* a topology,
*distribute* a fleet, apply *continuous* behaviors (mobility, traffic) that run
*concurrently* over a duration.

Two hard realities bound this, and must be reported, not hidden:

- **Core ceiling (integration vs sim).** SD-Core sustains ~5,000 UEs at ~10
  attach/s. 10k UEs / 100 gNBs / mass mobility is a **sim-capability** scenario
  (ORBIT's engine against a mock), not something the real core carries. Results
  must report sim vs integration separately (per the honest-scope rule).
- **Source-IP constraint.** The AMF distinguishes gNBs by SCTP source address,
  and handover needs a *distinct* one per gNB. 100 gNBs need 100 routable source
  IPs on the host — a deployment prerequisite, not something YAML can conjure.

The good news: the engines mostly exist. `internal/meas` already models cells
(`Cell.RSRP(x,y)`), UE tracks (`Track`), and measurement-driven handover
triggers (`Scenario.Run → []Trigger`, A3/A4/A5 with TTT/hysteresis);
`internal/engine.MobilityController` drives handovers from those triggers;
`internal/load.Run` does rate-controlled fleet attach; loom generates per-UE
traffic. Fleet mode is largely **orchestration over existing engines**, not new
protocol work.

## Decision

Add a **fleet/population mode** as a *complement* to entities+steps (ADR-0003),
not a replacement. It is **declarative** (topology + fleet + behaviors +
duration) and **direct-drive** — like `orbit load` / `orbit conformance`, it
uses the engine directly rather than the API server, because the concurrent,
stateful orchestration at this scale does not fit a per-call RPC.

Sketch:

```yaml
kind: fleet
core: { amf: ..., plmn: {...}, slice: {...}, dnn: internet }
credentials: { ki: ${ORBIT_KI}, opc: ${ORBIT_OPC} }

topology:
  gnbs:
    count: 10
    id_base: 1
    source_ips: [172.17.50.20, 172.17.50.21, ...]   # operator-supplied, one per gNB
    layout: grid

fleet:
  count: 100
  supi_base: 208930100007500
  distribution: even         # spread UEs across the topology
  attach_rate: 10/s          # -> internal/load
  pdu_session: true

behaviors:                   # run concurrently for `run.duration`
  mobility: { model: random_walk, speed: 3m/s, handover: xn }
  traffic:
    mix:
      - { profile: web,   share: 0.5 }
      - { profile: video, share: 0.3, rate: 8Mbps }
      - { profile: voip,  share: 0.2, rate: 64kbps }

run: { duration: 10m }
```

Engine mapping:

| Scenario piece | Engine |
|---|---|
| `topology.gnbs` | generate `meas.Cell`s at layout positions; bind each to a source IP from the pool |
| `fleet` | generate SUPIs + `meas.Track`s; attach via `internal/load.Run` (rate/concurrency) |
| `behaviors.mobility` | per-UE `meas.Scenario` (its Track + the Cells) → `MobilityController` driving Xn/N2 handover; bounded concurrency |
| `behaviors.traffic` | loom per-UE, one flow per UE per its assigned profile; continuous for the duration |
| `run.duration` | a concurrent orchestrator runs mobility + traffic together, then reports |

Reporting follows `load`: attach KPIs, handover success/latency, throughput, and
a resource trend — **labelled sim vs integration**, capped/annotated at the
core's ceiling.

## Alternatives considered

- **Extend entities+steps** (loops, selectors over the steps list) — bolts
  iteration onto an imperative model that is fundamentally about *named, ordered*
  actions; still can't express continuous concurrent behavior. Wrong shape.
- **A separate emulation tool** — throws away the `meas`/`load`/loom engines and
  the API/observability already built. No.
- **Drive it through the API server** (a streaming RPC) — pushes a huge stateful
  orchestration behind a request/response boundary; inconsistent with the
  direct-drive precedent set by `load`/`conformance`. Rejected for now (a
  read-only progress stream could be added later).

## Consequences

- Unlocks scale, soak, and mass-mobility scenarios by *exposing* engines that
  already exist — mostly orchestration + generation code, not protocol work.
- New work: topology/fleet generation, a population orchestrator (concurrent
  attach + mobility + traffic over a duration), and **traffic profiles/mix**
  (loom does constant rate today; on-off/bursty profiles likely **drive changes
  in loom** rather than working around it).
- A second execution model (direct-drive, concurrent) alongside the API-client
  step runner. Two modes, clearly delimited by `kind:`.
- Honest-scope surfaces in the output by construction (sim vs integration); the
  source-IP prerequisite is documented, and the mode runs meaningfully against a
  mock core even where SD-Core can't carry the load.

## Decisions (resolved 2026-07-07)

1. **One entry point.** `orbit run <file>` routes on a top-level `kind:` —
   absent/`steps` is the ADR-0003 step runner (API client); `fleet` is this mode
   (direct-drive). Two execution models behind one command.
2. **Real core first, at modest scale.** The first target is the real SD-Core
   at ~10 gNBs / 100 UEs (within its ceiling), not a 10k/100 sim run. The larger
   sim-against-mock numbers come later; nothing here is mock-only.
3. **User-supplied source IPs.** `topology.gnbs.source_ips` is a list the
   operator provides (host must own them); each generated gNB binds a distinct
   one for both its N2 SCTP source and N3. Error clearly if there are fewer IPs
   than gNBs (handover needs one each).
4. **Mobility.** Grid layout + random-walk first (reusing `meas.Cell`/`Track`);
   explicit trajectories later.
5. **Traffic — ORBIT-orchestrated first.** Start with profiles composed by
   *ORBIT scheduling loom's constant-rate flows* over time (VoIP = a steady
   low-rate flow; web = short on/off bursts; video = sustained higher rate;
   full-buffer = unlimited) — this ships the profile *mix* without blocking on
   loom. Only genuine in-flow shaping (jittered bursts, ramps) drives a change in
   loom. The orchestration seam lives ORBIT-side so profiles evolve here first.
