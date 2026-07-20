# ADR-0006: Live monitoring surface

- Status: Proposed
- Date: 2026-07-20

## Context

ADR-0005 makes the server own run execution. This ADR specifies how a client
watches a run in progress. The driving client is the web dashboard, but the
surface must serve any client, and must not become a dashboard-shaped API.

What exists today:

- **`UEService.StateStream`** — per-UE lifecycle transitions (`SUPI`, `state`,
  `detail`, `unix_nano`). Fan-out is a 64-slot buffered channel per subscriber,
  and `hub.publish` (`internal/engine/hub.go:28`) **drops on a full buffer**
  rather than blocking. There is no replay for a subscriber that attaches late.
- **`UEService.AppStream`** — the richest live feed: full VoIP quality plus
  `CorrelationEvent`s joining handover phases to media gaps. Scoped to one app
  session, hence one UE.
- **`Manager.List()`** and **`Manager.DataStats(supi)`** — continuously
  readable, but per-UE and poll-only.
- **`load.Observer`** (`internal/load/load.go:31`) — an interface called once per
  completed attempt, documented as "a live hook for metrics exposition during
  long runs". `load.PrometheusObserver` implements it fully. **It is never
  wired**: `engine.RunLoad` never sets `load.Config.Observer`.
- **HDR histograms** per procedure (`attach`, `registration`, `pdu_session`),
  read exactly once at completion.
- `internal/observability` registers **no** domain metrics, and OpenTelemetry is
  wired with **no** spans anywhere in the tree.

The forces:

- **Scale.** SD-Core's envelope is ~5,000 UEs. A client must not receive 5,000
  messages per tick, and must not issue 5,000 RPCs to build one view.
- **Loss.** The existing stream drops under load — exactly when a monitoring
  client most needs to be right. A dashboard that silently under-reports during
  a storm is worse than one that reports nothing.
- **Two kinds of data.** Aggregates ("how many UEs are registered") and discrete
  occurrences ("this UE's handover failed") have different correctness
  requirements, and conflating them is what makes the current stream unusable
  for both.
- Prometheus already exists and Grafana is already in use. This surface must not
  duplicate that role.

## Decision

**Two streams with different delivery semantics, plus server-side aggregation.**

### 1. Aggregates travel as periodic self-contained snapshots

`rpc RunTelemetry(RunTelemetryRequest) returns (stream TelemetryFrame)`

Each `TelemetryFrame` is a **complete snapshot** of the run's aggregate state at
an instant — never a delta. The client requests an interval; the server clamps
it to a supported range and reports the interval it chose.

Rationale: a lost snapshot costs one sample, and the next frame is fully
correct. Loss cannot accumulate into a wrong picture, so this stream may drop
frames under load without lying. This is the property the current per-UE stream
lacks.

A frame carries run-global counts and rates, per-procedure latency percentiles
read live from the existing HDR histograms, and **per-gNB aggregates** — not
per-UE rows. Aggregation happens in the engine, so cost is independent of client
count.

### 2. Discrete events travel as a sequenced, gap-detectable stream

`rpc RunEvents(RunEventsRequest) returns (stream RunEvent)`

Every event carries a **monotonic `seq` scoped to the run**. The server retains a
bounded ring per run. A client may resume `from_seq`; when the requested point
has been evicted, the server says so explicitly with the number of events it
could not supply.

Rationale: ORBIT is a *conformance and integration* tool. "You missed 412 events"
is an acceptable answer; silently missing them is not. The bounded ring keeps
memory fixed while making loss observable rather than invisible. This does not
make delivery guaranteed — it makes gaps **detectable**, which is what a test
tool actually needs.

### 3. Per-UE detail is pulled, not pushed

The frame carries aggregates. A client that wants one UE's detail calls the
existing unary RPCs, or subscribes to `StateStream` with a SUPI filter. The
dashboard renders aggregates and drills down on demand; it never holds 5,000 UE
rows.

`StateStream`'s server-side SUPI filter is currently applied after subscribing to
everything (`internal/server/ue.go:144` discards with `continue`). Push the
filter into the hub so a filtered subscriber cannot be starved by unrelated
traffic.

### 4. `load.Observer` is the ingestion path

Live load statistics come from the existing unwired hook, not from new
instrumentation: `engine.RunLoad` sets `Config.Observer` to a fan-out that feeds
both the run's live aggregate and the Prometheus registry. This turns finished
work on, and keeps one code path feeding both surfaces.

### 5. Prometheus keeps its role; the browser never scrapes it

Prometheus and Grafana remain the long-term, cross-run, alerting surface. The
Connect streams serve live, run-scoped, sub-second observation. The dashboard
does not scrape `/metrics`: it would lose run identity, inherit scrape-interval
granularity, and force PromQL into the client.

### 6. Mobility state and serving gNB become first-class

`UEStatus` gains the serving gNB and a mobility state distinct from the
registration state. These are orthogonal axes: a UE that has handed over is
still `SESSION_ACTIVE`, which is why `Session.state` is correct today but
incomplete. `Session.gnbCfg`/`gnbN3` need accessors; nothing outside
`internal/engine` can read them.

The `HANDOVER_FAILED` case is explicitly modelled rather than left implicit: a
failed handover leaves the registration state untouched while the data path may
be broken, and a monitoring client must be able to see that combination.

## Alternatives considered

**One stream carrying everything.** Simpler surface, but it forces a single
delivery semantic onto data with different requirements: either aggregates get
expensive guaranteed delivery, or events get lossy delivery that hides failures.
The split exists precisely because the correctness requirements differ.

**Deltas instead of snapshots for aggregates.** Less bandwidth. Rejected: a lost
delta corrupts every subsequent value until a resync, which is the failure mode
least acceptable in a measurement tool. At the frame sizes involved, the
bandwidth saving is not worth the class of bug.

**Guaranteed, acknowledged event delivery.** Rejected as disproportionate: it
requires per-subscriber persistence and backpressure into the engine, and
backpressure into the hot path is forbidden by the DESIGN §risk invariant that
observability must never slow the thing being measured. A bounded ring plus
explicit gap reporting gets the needed honesty at a fraction of the cost.

**Reuse `/metrics` as the dashboard's data source.** Rejected: no run identity,
scrape-granularity, cardinality problems at 5,000 UEs, and it would put PromQL
in the browser.

**A separate monitoring API/port.** Rejected: DESIGN §3(h) already decided
server-streaming on the one Connect API, and `system.proto` reserves the service
names. A second surface means a second auth story and a second place for schemas
to drift.

## Consequences

Easier:

- A dashboard renders a 5,000-UE run at fixed cost, independent of population.
- Loss becomes visible instead of silent — a client can state its own accuracy.
- Live load statistics arrive by wiring an existing, tested hook.
- One aggregation path feeds the API, Prometheus, and the CLI's own rendering.

Harder:

- The engine must maintain aggregates incrementally. Recomputing over all
  sessions per tick is O(UEs) per frame and will not hold at scale.
- Per-run event rings and retained run history are new memory the daemon holds.
  Both are bounded and configurable; defaults must be chosen deliberately.
- HDR histograms must be read concurrently while a run writes them. `hdrhistogram`
  is not safe for concurrent read/write, so live percentiles need either a
  snapshot copy under the existing mutex or a rotating pair.
- Frame and event schemas become compatibility surface once released.

Known limits accepted:

- Frames may be dropped; the surface is explicitly sampled, not a lossless
  time series. Anything needing exactness must come from the end-of-run report,
  which stays authoritative.
- Event rings are bounded, so a client that disconnects for long enough will be
  told it missed events and cannot recover them.

Follow-up:

- Proto definitions land with the engine layers behind them, per the
  `system.proto` note that schemas freeze against working code rather than
  guesses.
- The dashboard's provisional `TelemetryFrame` model (`web/src/data/types.ts`)
  is reconciled against the real schema; mismatches are a question about which
  side is right, not an automatic edit to the client.
