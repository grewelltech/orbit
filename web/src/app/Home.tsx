/**
 * Home — the overview of runs: what is live now and what finished lately.
 *
 * Built from the run list (useRuns). The proto's RunSummary also carries
 * attempted/succeeded/failed counts, but no RPC returns it yet, so this page
 * shows identity, kinds and state only.
 */
import { useMemo } from "react";
import { useRuns, type RunSummary } from "@/data/runs";
import { Panel } from "@/components/Panel";
import { StatTile } from "@/components/StatTile";
import { PageLinks } from "./PageLinks";

const RECENT = 10;

function RunRow({ r }: { r: RunSummary }) {
  return (
    <tr className="border-t border-[var(--o-border)]">
      <td className="py-1.5 pr-4">
        <a href={`/runs?run=${encodeURIComponent(r.runId)}`} style={{ color: "var(--o-accent)" }}>
          {r.name || r.runId}
        </a>
      </td>
      <td className="py-1.5 pr-4">{r.kinds.join(" + ")}</td>
      <td className="py-1.5 pr-4" style={{ color: r.state === "failed" ? "var(--o-error)" : undefined }}>
        {r.state}
      </td>
      <td className="py-1.5">{r.startedAt > 0 ? new Date(r.startedAt).toLocaleString() : "—"}</td>
    </tr>
  );
}

function RunTable({ runs }: { runs: RunSummary[] }) {
  return (
    <table className="o-label w-full text-left" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
      <thead>
        <tr style={{ color: "var(--o-ink-3)" }}>
          <th className="pr-4 font-normal">run</th>
          <th className="pr-4 font-normal">kind</th>
          <th className="pr-4 font-normal">state</th>
          <th className="font-normal">started</th>
        </tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <RunRow key={r.runId} r={r} />
        ))}
      </tbody>
    </table>
  );
}

export function Home() {
  const runs = useRuns();
  const active = useMemo(() => runs.filter((r) => r.active), [runs]);
  const recent = useMemo(() => runs.filter((r) => !r.active).slice(0, RECENT), [runs]);
  const failed = useMemo(() => runs.filter((r) => r.state === "failed").length, [runs]);

  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <header className="flex items-center gap-6 border-b border-[var(--o-border)] px-4 py-2.5">
        <span className="o-label" style={{ color: "var(--o-ink)" }}>
          orbit
        </span>
        <PageLinks current="home" />
      </header>
      <main className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3">
        <div className="grid grid-cols-3 gap-3">
          <StatTile label="live runs" value={String(active.length)} />
          <StatTile label="total runs" value={String(runs.length)} />
          <StatTile label="failed runs" value={String(failed)} />
        </div>
        <Panel title="Live now">
          {active.length > 0 ? (
            <RunTable runs={active} />
          ) : (
            <p className="o-label" style={{ color: "var(--o-ink-3)" }}>
              nothing running — <a href="/launch">launch a run</a>
            </p>
          )}
        </Panel>
        <Panel title="Recent runs">
          {recent.length > 0 ? (
            <RunTable runs={recent} />
          ) : (
            <p className="o-label" style={{ color: "var(--o-ink-3)" }}>
              no finished runs yet
            </p>
          )}
        </Panel>
      </main>
    </div>
  );
}
