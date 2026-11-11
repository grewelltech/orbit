# ADR-0010: Home, chooser, per-kind dashboards; a run summary field

- Status: Accepted; amended by ADR-0011 (conformance tally and the table's category column)
- Date: 2026-11-10
- Builds on: ADR-0005 (the server owns runs), ADR-0008 (one unified run)

## Context

The web UI and the CLI are already peers for every server-side run operation:
both are thin Connect clients of `RunService` / `ConformanceService`, and the
server renders the HTML report so neither re-implements it (ADR-0005). Runs are
persisted and `ListRuns` already merges live and archived of both kinds; a
performance run's live and past views are already identical, because
`RunTelemetry(from_seq=0)` replays the archived frames through the same shape
the live dashboard consumes.

What was missing was presentation, not capability. The web app had no router
and no home page: it booted the latest run's dashboard and offered only a
run-picker dropdown, with conformance-versus-performance expressed as
conditional blocks inside one `App.tsx`. The CLI printed conformance results
as borderless plain text, while the rich table lived only in the web
`SuiteResults`. And `ListRuns` carried no per-run outcome, so a home page would
have fanned out an extra `GetRun`/`GetRunReport` per row to label results.

## Decision

1. **A run-summary field.** `Run.summary` (`RunSummary`) is a small outcome
   digest — conformance tally, attach figures, gate verdict, and the run's
   workload kinds — computed from the run's per-workload reports **at archive
   time** and attached to terminal runs in `ListRuns` and `GetRun`. A list or
   home view labels a run's result from one call. A run that produced no report
   (FAILED, CANCELLED) carries no summary and is labelled from its state.

2. **Routing.** The web app uses `react-router`. `/` is the run history
   (Home); `/runs` is the dashboard and `/runs/:id` pins a run; `/new` chooses
   a test kind and routes into `/launch[/:preset]` (the existing Launch
   editor, pre-seeded from a template); `/conformance` opens Launch on the
   suite; `/targets` edits target files. The pre-router query-string URLs
   (`?run`, `?mock`, `?conformance`, `?targets`, `#import=`) redirect to their
   new paths, so shared and bookmarked links keep working.

3. **Per-kind dashboards.** The one `App.tsx` is split into a shell (header,
   run picker, stop control, time window, resolved-config modal) and two
   composed views — `PerformanceDashboard` and `ConformanceDashboard` — chosen
   by `RunDashboard` from the run's kind. A mixed run shows both, the suite
   verdicts above the charts. The views reuse every existing panel and do not
   touch the live/past mechanics, so a live run and an archived one render
   identically; only the charts' time-axis auto-scroll is live-only.

4. **A CLI conformance table.** `orbit conformance` renders results as a
   colour-coded, box-drawn `lipgloss` table (`verdict · test · category · spec
   · observed`, with a tally) on an interactive terminal, and keeps the plain
   borderless output for pipes, files, CI and `--json` — exit codes unchanged.

## Consequences

- The home page renders outcomes from `ListRuns` alone; no per-row report
  fetch. The summary is computed once, at archive, not on every list.
- `/runs/:id` is a shareable, bookmarkable per-run URL; the picker and Home
  navigate to it rather than mutating a query string.
- A reader gets the same dashboard for a live run and a finished one, and the
  CLI's conformance output matches the web table's columns and semantics.

## Alternatives and deferrals

- Computing the summary live on each `ListRuns` was rejected: archive-time
  computation keeps the list cheap and the number stable.
- Peak user-plane throughput in the summary, a richer live conformance stream,
  UI surfaces for the ad-hoc `ue`/`cell` primitives, and step scenarios as
  server-tracked runs are deferred follow-ups, tracked in the epic (#266).
