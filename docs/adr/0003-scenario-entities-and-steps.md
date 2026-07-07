# ADR-0003: Scenario files declare entities, then ordered steps

- Status: Accepted
- Date: 2026-07-07

## Context

The `ue` / `load` / `conformance` commands accumulate many flags; real tests
repeat the same core/PLMN/credentials on every invocation. A declarative file is
the fix. Three shapes were on the table (entities+steps, declarative fleet, flat
action list).

## Decision

A scenario **declares the core, gNBs, and UEs once**, then an ordered **`steps`**
list references them by name/SUPI (`register`, `ping`, `traffic`, `latency`,
`handover`, `deregister`, `wait`). Secrets use `${ENV}` substitution. `orbit run`
executes the steps in order and stops at the first failure; the runner is an
ordinary **API client** (the CLI never touches the engine directly). UE ranges
expand to contiguous SUPIs.

## Alternatives considered

- **Declarative fleet** (describe a desired population; the runner derives
  actions) — better for scale/soak, but weak for explicit sequences like
  handover-then-assert. Deferred to **ADR-0004** as a complementary *second*
  mode, not a replacement.
- **Flat action list** — every action repeats amf/plmn/creds, i.e. exactly the
  flag repetition we're escaping.

## Consequences

- Clean, readable, scripted small-N integration tests; a scenario doubles as a
  CI test (steps fail on RPC error or a natural assertion).
- Explicitly **not** for large dynamic populations (100s of gNBs, 10k UEs,
  continuous mobility/traffic) — the ordered-steps model can't express
  concurrent, continuous, population-level behavior. That is ADR-0004.
