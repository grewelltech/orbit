# Architecture Decision Records

ADRs capture significant architectural decisions — the **context**, the
**choice**, and its **consequences** — so the *why* survives after the diff is
forgotten. One decision per file. Once **Accepted**, an ADR is immutable:
supersede it with a new ADR rather than editing it.

Write one when a decision is hard to reverse, shapes multiple packages, or picks
between real alternatives (not for routine implementation choices). Use
[`0000-template.md`](0000-template.md). Status is **Proposed**, **Accepted**, or
**Superseded by ADR-XXXX**.

## Log

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-reuse-free5gc-codecs.md) | Reuse free5gc codecs; build the differentiators clean-room | Accepted |
| [0002](0002-strict-codecs-with-core-profiles.md) | Strict-by-default codecs; core quirks are opt-in and named | Accepted |
| [0003](0003-scenario-entities-and-steps.md) | Scenario files: declare entities, then ordered steps | Accepted |
| [0004](0004-fleet-population-mode.md) | A fleet/population mode for large dynamic scenarios | Proposed |
| [0005](0005-server-owned-run-execution.md) | The server owns run execution; every client drives it through the API | Proposed |
| [0006](0006-live-monitoring-surface.md) | Live monitoring: snapshot frames for aggregates, sequenced events for occurrences | Proposed |
