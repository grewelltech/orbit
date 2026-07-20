# ADR-0005: Server-owned run execution

- Status: Proposed
- Date: 2026-07-20

## Context

DESIGN §3 states the architecture as CLI → API → Engine, and `system.proto`
records the intent that every client drives ORBIT exclusively through the
services it defines. The CLI is meant to be one of N equal clients — a web app,
a Windows app, or a test harness should have the same capabilities.

Three execution models exist in the tree today, and they do not agree:

| Entry point | Executes | Reaches `orbit serve`? |
|---|---|---|
| `orbit ue <verb>` | CLI → Connect API → server `engine.Manager` | Yes |
| `orbit run <step scenario>` | CLI drives the API step by step (`internal/cli/run.go:52`) | Partially — the server sees individual UE RPCs but has no run identity |
| `orbit load` | In-process: `engine.RunLoad` (`internal/cli/load.go`) | No |
| `orbit run <kind: fleet>` | In-process: `engine.RunFleet` (`internal/cli/run.go:135`) | No |

`RunLoad`, `RunFleet`, and `RunHandoverUnderLoad` construct their own `Fleet` or
a private `Manager`. Their UEs never appear in `Manager.List()`, and they emit no
hub events: `Attach` is called with `emit=nil` (`internal/engine/fleet.go:88`) or
with a private latency-capturing closure (`internal/engine/load_driver.go:32`).

The consequences are not limited to observability:

- A monitoring dashboard served by `orbit serve` renders nothing during a load
  or fleet run — precisely the runs worth watching.
- Capability is unequal by construction. Anything the CLI does in-process is
  unavailable to any other client, so a second client cannot reach parity
  without reimplementing the orchestrator.
- A run cannot be observed from another machine, cannot be observed by two
  clients at once, and leaves no record once the process exits.
- The N6/core-side agent that user-plane work requires has no natural home,
  because there is no service that owns run execution to talk to.

This is drift from the intended design rather than a deliberate trade-off: the
API grew per-phase around single-UE operations, and the load orchestrator was
written before remote observation was a requirement.

## Decision

**The ORBIT server owns run execution. Clients start, observe, and stop runs
through the API; no client orchestrates a run in its own process.**

1. A **run** becomes a first-class engine concept with an identity: a `RunID`, a
   kind (`load`, `fleet`, `scenario`, `conformance`), the spec that produced it,
   a lifecycle state, and start/end timestamps.
2. The `Manager` gains a **run registry** — the set of active runs plus a
   bounded history of completed ones — enumerable at any time.
3. `engine.RunLoad`, `engine.RunFleet`, `engine.RunHandoverUnderLoad`, and the
   scenario runner are driven **by the server**, not by the CLI. They report
   progress through the engine rather than by printing.
4. A new **`RunService`** exposes start/stop/list/get plus the observation
   streams specified in ADR-0006.
5. The CLI becomes a thin client: `orbit load` and `orbit run` submit a run and
   render the streams they get back. Their console output is a *rendering* of
   the same data any other client receives, not a privileged view.
6. Runs outlive the client that started them. Disconnecting a client does not
   stop a run; stopping is an explicit call.

Scenario execution moves server-side too. The step runner currently *is* an API
client (`internal/scenario/run.go`), which looks like the right shape but puts
the orchestrating loop in the wrong process — a disconnected client abandons the
scenario mid-way with no record.

## Alternatives considered

**Keep runs in the CLI; have the CLI serve its own telemetry.** The dashboard
would attach to the CLI process during a run. Rejected: it entrenches the
capability split this ADR exists to remove, needs a port-discovery mechanism,
loses all history when the process exits, and leaves the dashboard blank when
served from the daemon — the normal deployment.

**Keep runs in the CLI; push progress to the daemon.** The daemon becomes an
aggregation point without owning execution. Rejected as a permanent design: the
reporting path is best-effort, so the daemon holds an approximation rather than
the truth; a crashed CLI leaves a phantom run; and capability stays unequal
because only the CLI can start a run. It remains viable as a migration step if
lifting the orchestrators proves too large for one change.

**Expose the existing in-process engine over the API without restructuring.**
Rejected: `RunLoad`/`RunFleet` build their own `Manager`/`Fleet`, so their UEs
would remain invisible to `Manager.List()` and the hub. The isolation is the
problem, not the entry point.

## Consequences

Easier:

- Every client gets the same capabilities; a web or desktop client reaches CLI
  parity without reimplementing anything.
- Runs become observable remotely, by several clients at once, and after the
  fact.
- The N6/core-side agent has an obvious counterpart to talk to.
- Load and fleet UEs join the single `Manager`, so `List()`, `DataStats`, and
  the hub cover them — several existing features start working during load runs
  for free.

Harder:

- `RunLoad`/`RunFleet` must stop constructing private `Manager`/`Fleet`
  instances and take the server's. This is the bulk of the work and touches
  concurrency: the shared `Manager` must tolerate thousands of UEs arriving from
  a run while unary RPCs read the same maps.
- Credentials (Ki/OPc) move from CLI flags into API requests. DESIGN §8 already
  expects the API to carry subscriber secrets and to stay on a lab-internal
  listener, but this raises the exposure and should be revisited when the API is
  reachable off-host.
- Long-running work now lives in the daemon: cancellation, resource limits, and
  a policy for concurrent runs are required. The initial policy is one active
  run of a given kind, rejecting a second with `FAILED_PRECONDITION`.
- `orbit load` becomes useless without a reachable server, changing an existing
  workflow. Accepted, and consistent with the other subcommands.

Follow-up work:

- ADR-0006 specifies the observation surface.
- `Session.gnbCfg`/`gnbN3` need accessors before any per-gNB view is possible.
- Deciding whether the engine keeps a supported in-process (library) path for
  tests. It should: the engine stays a library, and only the *entry points* move.
