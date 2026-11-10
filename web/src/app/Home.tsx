/**
 * Home — the run history / index at "/".
 *
 * A browsable list of what is running now and what finished lately, from one
 * ListRuns call (useRuns), filterable by kind. Each row names a run, its
 * kinds, state, when it ran, and its outcome digest (Run.summary) — so a run's
 * result reads at a glance without opening it. A row opens the run dashboard
 * at /runs/:id; "New test" opens the chooser at /new.
 */
import { useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router";
import { useRuns, type RunOutcome, type RunSummary } from "@/data/runs";
import { Panel } from "@/components/Panel";
import { StatTile } from "@/components/StatTile";
import { PageLinks } from "./PageLinks";

type Filter = "all" | "performance" | "conformance";

const PERF_KINDS = new Set(["load", "fleet", "scenario", "mixed"]);

function matchesFilter(r: RunSummary, f: Filter): boolean {
  if (f === "all") return true;
  if (f === "conformance") return r.kind === "conformance" || r.kind === "mixed";
  return PERF_KINDS.has(r.kind);
}

function relativeTime(ms: number): string {
  if (!ms) return "—";
  const secs = Math.max(0, (Date.now() - ms) / 1000);
  if (secs < 60) return "just now";
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}

function duration(startMs: number, endMs: number): string {
  if (!startMs || !endMs || endMs < startMs) return "";
  const s = (endMs - startMs) / 1000;
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}

/** The outcome digest for a row, as compact colored spans. */
function Outcome({ outcome }: { outcome?: RunOutcome }) {
  if (!outcome) return <span style={{ color: "var(--o-ink-3)" }}>—</span>;
  const parts: ReactNode[] = [];

  if (outcome.conformance) {
    const c = outcome.conformance;
    const realFail = Math.max(0, c.failed - c.expectedFailures);
    // An ERROR is a failed check too (the suite couldn't judge it), so a run
    // with errors must not read as a healthy green result.
    const bad = realFail > 0 || c.errored > 0;
    const color = bad ? "var(--o-error)" : "var(--o-ok, #2f9e52)";
    const extra = [realFail > 0 ? `${realFail} fail` : "", c.errored > 0 ? `${c.errored} error` : ""].filter(Boolean).join(" · ");
    parts.push(
      <span key="conf" style={{ color }}>
        {c.passed}/{c.total} pass{extra ? ` · ${extra}` : ""}
      </span>,
    );
  }
  if (outcome.attach) {
    const a = outcome.attach;
    const color = a.failed > 0 ? "var(--o-warn, #c9820c)" : "var(--o-ink-2)";
    parts.push(
      <span key="attach" style={{ color }}>
        {a.succeeded}/{a.attempted} attached
      </span>,
    );
  }
  if (outcome.gates) {
    const g = outcome.gates;
    parts.push(
      <span key="gates" style={{ color: g.ok ? "var(--o-ok, #2f9e52)" : "var(--o-error)" }}>
        gates {g.passed}/{g.total}
      </span>,
    );
  }
  if (parts.length === 0) return <span style={{ color: "var(--o-ink-3)" }}>—</span>;
  return (
    <span className="flex flex-wrap gap-x-3 gap-y-0.5">
      {parts}
    </span>
  );
}

function RunRow({ r }: { r: RunSummary }) {
  const navigate = useNavigate();
  const to = `/runs/${encodeURIComponent(r.runId)}`;
  const kinds = r.outcome?.kinds?.length ? r.outcome.kinds.join(" + ") : r.kind;
  const when = r.active ? relativeTime(r.startedAt) : relativeTime(r.endedAt || r.startedAt);
  const dur = duration(r.startedAt, r.endedAt);
  return (
    <tr
      className="cursor-pointer border-t transition-colors hover:bg-[var(--o-surface-2)]"
      style={{ borderColor: "var(--o-border)" }}
      onClick={() => navigate(to)}
    >
      <td className="py-2 pr-4">
        <Link to={to} style={{ color: "var(--o-accent)" }} onClick={(e) => e.stopPropagation()}>
          {r.name || r.runId}
        </Link>
      </td>
      <td className="py-2 pr-4" style={{ color: "var(--o-ink-2)" }}>
        {kinds}
      </td>
      <td className="py-2 pr-4">
        <span className="inline-flex items-center gap-1.5" style={{ color: r.state === "failed" ? "var(--o-error)" : "var(--o-ink-2)" }}>
          {r.active && (
            <span
              aria-hidden
              className="inline-block h-1.5 w-1.5 rounded-full"
              style={{ background: "var(--o-accent)" }}
            />
          )}
          {r.state}
        </span>
      </td>
      <td className="py-2 pr-4" style={{ color: "var(--o-ink-3)" }}>
        {when}
        {dur ? ` · ${dur}` : ""}
      </td>
      <td className="py-2">
        <Outcome outcome={r.outcome} />
      </td>
    </tr>
  );
}

function RunTable({ runs }: { runs: RunSummary[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="o-label w-full min-w-[640px] text-left" style={{ textTransform: "none" }}>
        <thead>
          <tr style={{ color: "var(--o-ink-3)" }}>
            <th className="pb-1 pr-4 font-normal">run</th>
            <th className="pb-1 pr-4 font-normal">kind</th>
            <th className="pb-1 pr-4 font-normal">state</th>
            <th className="pb-1 pr-4 font-normal">when</th>
            <th className="pb-1 font-normal">outcome</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <RunRow key={r.runId} r={r} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

const FILTERS: { id: Filter; label: string }[] = [
  { id: "all", label: "all" },
  { id: "performance", label: "performance" },
  { id: "conformance", label: "conformance" },
];

export function Home() {
  const { runs, loaded, error } = useRuns();
  const [filter, setFilter] = useState<Filter>("all");

  const active = useMemo(() => runs.filter((r) => r.active && matchesFilter(r, filter)), [runs, filter]);
  const past = useMemo(() => runs.filter((r) => !r.active && matchesFilter(r, filter)), [runs, filter]);
  const failed = useMemo(() => runs.filter((r) => r.state === "failed").length, [runs]);

  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <header
        className="flex items-center gap-6 border-b px-4 py-2.5"
        style={{ borderColor: "var(--o-border)" }}
      >
        <span className="o-label" style={{ color: "var(--o-accent)", fontWeight: 700, letterSpacing: "0.12em" }}>
          orbit
        </span>
        <PageLinks current="home" />
      </header>

      <main className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3">
        <div className="flex flex-wrap items-center gap-3">
          <div className="grid flex-1 grid-cols-3 gap-3">
            <StatTile label="live runs" value={String(active.length)} tone="accent" />
            <StatTile label="total runs" value={String(runs.length)} />
            <StatTile label="failed" value={String(failed)} tone={failed > 0 ? "error" : "neutral"} />
          </div>
          <Link
            to="/new"
            className="o-label flex items-center gap-1.5 border px-3 py-2 transition-colors"
            style={{
              color: "var(--o-surface)",
              background: "var(--o-accent)",
              borderColor: "var(--o-accent)",
              borderRadius: "var(--o-radius)",
              textTransform: "none",
            }}
          >
            + New test
          </Link>
        </div>

        {error && (
          <div
            className="o-label shrink-0 border px-3 py-2"
            role="status"
            style={{
              textTransform: "none",
              color: loaded ? "var(--o-warn, #c9820c)" : "var(--o-error)",
              borderColor: "var(--o-border)",
              background: "var(--o-surface)",
              borderRadius: "var(--o-radius)",
            }}
          >
            {loaded
              ? "server unreachable — showing the last known runs (reconnecting…)"
              : "can't reach the server — retrying…"}
          </div>
        )}

        {/* Filter by kind. */}
        <div className="flex shrink-0 items-center gap-1.5">
          <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
            show
          </span>
          {FILTERS.map((f) => (
            <button
              key={f.id}
              type="button"
              onClick={() => setFilter(f.id)}
              className="o-label cursor-pointer border px-1.5 py-0.5 transition-colors"
              style={{
                color: filter === f.id ? "var(--o-accent)" : "var(--o-ink-3)",
                borderColor: filter === f.id ? "var(--o-border-accent)" : "var(--o-border)",
                borderRadius: "var(--o-radius)",
                textTransform: "none",
              }}
              aria-pressed={filter === f.id}
            >
              {f.label}
            </button>
          ))}
        </div>

        <Panel title="Live now" className="shrink-0">
          {active.length > 0 ? (
            <RunTable runs={active} />
          ) : (
            <p className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
              {!loaded && error ? (
                "waiting for the server…"
              ) : (
                <>
                  nothing running — <Link to="/new" style={{ color: "var(--o-accent)" }}>start a test</Link>
                </>
              )}
            </p>
          )}
        </Panel>

        <Panel title="Recent runs" className="shrink-0">
          {past.length > 0 ? (
            <RunTable runs={past} />
          ) : (
            <p className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
              {!loaded && error ? "—" : "no finished runs match this filter yet"}
            </p>
          )}
        </Panel>
      </main>
    </div>
  );
}
