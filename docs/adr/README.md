# Architecture Decision Records

ADRs capture significant architectural decisions — the **context**, the
**choice**, and its **consequences** — so the *why* survives after the diff is
forgotten. One decision per file. Once **Accepted**, an ADR is immutable:
supersede it with a new ADR rather than editing it.

Write one when a decision is hard to reverse, shapes multiple packages, or picks
between real alternatives (not for routine implementation choices). Use
[`0000-template.md`](0000-template.md). Status is **Proposed**, **Accepted**,
**Accepted; amended by ADR-XXXX** (a later ADR changes part of the decision;
the amended ADR's body is never edited, only its status line), or
**Superseded by ADR-XXXX**.

## Log

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-reuse-free5gc-codecs.md) | Reuse free5gc codecs; build the differentiators clean-room | Accepted |
| [0002](0002-strict-codecs-with-core-profiles.md) | Strict-by-default codecs; core quirks are opt-in and named | Accepted |
| [0003](0003-scenario-entities-and-steps.md) | Scenario files: declare entities, then ordered steps | Accepted |
| [0004](0004-fleet-population-mode.md) | A fleet/population mode for large dynamic scenarios | Accepted |
| [0005](0005-server-owned-run-execution.md) | The server owns run execution; every client drives it through the API | Accepted |
| [0006](0006-live-monitoring-surface.md) | Live monitoring: snapshot frames for aggregates, sequenced events for occurrences | Accepted |
| [0007](0007-orbit-hosted-loom-responder.md) | ORBIT hosts the loom responder; stock loomd stays supported | Accepted; amended by ADR-0009 (install and bootstrap) |
| [0008](0008-targets-and-runs.md) | Targets (where) and one Run (what): client-held targets, layered config with provenance, leasing, TLS + token | Accepted |
| [0009](0009-responder-join.md) | App-side setup by join: token one-liner, outbound registration, client-side SSH, doctor | Accepted |
