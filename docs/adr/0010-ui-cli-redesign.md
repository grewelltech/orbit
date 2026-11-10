# ADR-0010: UI and CLI redesign: Home, chooser, per-kind dashboards, lipgloss tables

- Status: Accepted
- Date: 2026-11-09

## Context

The web UI opened on a single run dashboard at `/`, with one layout for every
run kind and a launch page that started from a blank editor. The CLI printed
tables through `text/tabwriter`. Mixed runs (several workload kinds in one run)
had no name in the UI's kind mapping. Epic 266 reworks both surfaces so that the
UI and CLI present runs consistently.

## Decision

1. **Routes.** The web app uses react-router. `/` is Home (live runs, recent
   runs, headline counts). The run dashboard moves to `/runs`; `/runs?run=<id>`
   selects a run. `/launch` is a run-kind chooser, `/launch/edit` is the plain
   editor (resumes the tab's draft), and `/launch/:kind` opens the editor with a
   template preset (`mixed` means an empty run file). Legacy `/?run=…`, `/?mock`,
   `?launch`, `?targets` and `?conformance` URLs are redirected.
2. **Per-kind dashboards.** `RunDashboard` picks a dashboard from the run kind:
   conformance gets `ConformanceDashboard`; mixed gets `PerformanceDashboard`
   with the suite results above the charts; everything else, including unknown
   kinds, gets `PerformanceDashboard`. A new kind is a new branch there.
3. **RunSummary.** `run.proto` gains `RunSummary` (a `Run`, its `kinds`, and
   cumulative attempted/succeeded/failed counts) so list views need not fetch
   progress per run. The message exists, but no RPC returns it yet. Home is built
   from the existing `ListRuns` result and shows identity, kinds and state only.
4. **MIXED label.** `runKindName` in `web/src/data/runs.ts` handles
   `RunKind.MIXED`.
5. **CLI tables.** All CLI tables render through lipgloss via
   `internal/cli/newTable`, a drop-in for the tabwriter API (tab-separated lines,
   first is the header, `Flush()` renders borderless with a bold header). New
   tables use `newTable`; `text/tabwriter` is not used in `internal/cli`.

## Alternatives considered

**Keep `/` as the dashboard and add Home elsewhere.** No URL churn, but the
landing page stays a single-run view. Rejected; redirects keep old links working.

**One dashboard with per-kind conditionals.** Smaller diff, but each kind's
layout grows tangled. Separate components keep kinds independent.

**Keep tabwriter.** No dependency, but no styling. Lipgloss gives a consistent
look at the cost of a new dependency.

**Add a `ListRunSummaries` RPC in this epic.** Would give Home real counts, but
widens the epic into server work. Deferred.

## Consequences

Easier: a clear landing page; adding a dashboard or launch kind is one branch or
one chooser card; CLI table styling lives in one helper.

Harder: the `/` to `/runs` move breaks bookmarks that skip the redirect (the
redirect covers the known legacy forms); the module gains lipgloss and its
dependencies; the embedded bundle in `internal/webui/dist` changes with every
UI change.

Accepted limits:

- Home shows no per-run counts until an RPC returns `RunSummary`.
- `/launch/<unknown name>` is not specially handled.
- The new CLI look and the chooser have no tests of their appearance.

## Open questions

| # | Question | Owner |
|---|---|---|
| 1 | Which RPC returns `RunSummary` (change `ListRuns` or add a new call), and when does Home use the counts? | architect |
| 2 | Should `/launch/<unknown name>` show an error instead of opening the editor? | implementer |
