/**
 * SuiteResults — a run's conformance suites on the run view: each suite's
 * progress while the run is in flight, then every check's verdict once it
 * has ended. Shown only for a run that has a suite (a conformance run, or
 * a mixed run with a conformance workload).
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import type { ConformanceResult } from "@/gen/orbit/v1/conformance_pb";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { Panel } from "@/components/Panel";
import { SuiteWatch, type Suite } from "@/data/suite";
import { orbitTransport } from "@/data/transport";

const VERDICT_COLOR: Record<string, string> = {
  PASS: "var(--o-accent)",
  FAIL: "var(--o-bad, #ff5c6c)",
  ERROR: "var(--o-bad, #ff5c6c)",
  SKIP: "var(--o-ink-3)",
  DEVIATION: "var(--o-amber)",
};

function verdictColor(v: string): string {
  return VERDICT_COLOR[v] ?? "var(--o-ink-2)";
}

/** Follows runId's suites; none when there is no run or it has no suite. */
function useSuites(runId: string | undefined, baseUrl?: string): { suites: Suite[]; done: boolean } {
  const client = useMemo(() => createClient(RunService, orbitTransport(baseUrl)), [baseUrl]);
  const [view, setView] = useState<{ suites: Suite[]; done: boolean }>({ suites: [], done: false });
  useEffect(() => {
    setView({ suites: [], done: false });
    if (!runId) return;
    const w = new SuiteWatch(client, runId, (suites, done) => setView({ suites, done }));
    void w.start();
    return () => w.dispose();
  }, [client, runId]);
  return view;
}

export function SuiteResults({ runId }: { runId: string | undefined }) {
  const { suites, done } = useSuites(runId);
  if (suites.length === 0) return null;
  return (
    <section className="flex shrink-0 flex-col gap-3">
      {suites.map((s) => (
        <SuitePanel key={s.name} suite={s} done={done} />
      ))}
    </section>
  );
}

function SuitePanel({ suite, done }: { suite: Suite; done: boolean }) {
  const p = suite.progress;
  const count = (v: string) => suite.results.filter((r) => r.verdict === v).length;
  const tally = done
    ? { pass: count("PASS"), fail: count("FAIL"), error: count("ERROR"), deviation: count("DEVIATION"), skip: count("SKIP") }
    : { pass: p?.pass ?? 0, fail: p?.fail ?? 0, error: p?.error ?? 0, deviation: p?.deviation ?? 0, skip: p?.skip ?? 0 };
  const meta = done ? `${suite.results.length} checks` : p ? `${p.ran} of ${p.total}${p.lastCheck ? ` · ${p.lastCheck}` : ""}` : "starting";
  return (
    <Panel title={suite.name ? `conformance · ${suite.name}` : "conformance"} live={!done} meta={meta}>
      <div className="flex flex-col gap-2">
        <div className="flex flex-wrap gap-x-5 gap-y-1">
          <Stat label="pass" value={tally.pass} color={verdictColor("PASS")} />
          <Stat label="fail" value={tally.fail} color={verdictColor("FAIL")} />
          <Stat label="error" value={tally.error} color={verdictColor("ERROR")} />
          <Stat label="deviation" value={tally.deviation} color={verdictColor("DEVIATION")} />
          <Stat label="skip" value={tally.skip} color={verdictColor("SKIP")} />
        </div>
        {suite.expectedCore && (
          <div className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            expected failures: {suite.expectedCore}
            {suite.stale.map((s) => (
              <div key={`${s.check} ${s.finding} ${s.observed}`} style={{ color: "var(--o-amber)" }}>
                stale expected failure: {s.check} {s.finding}: now {s.verdict} &quot;{s.observed}&quot;
              </div>
            ))}
          </div>
        )}
        {done ? (
          <div className="max-h-[32rem] overflow-auto">
            <table className="w-full" style={{ borderCollapse: "collapse", fontFamily: "var(--o-font-mono)" }}>
              <thead>
                <tr>
                  {["verdict", "test", "category", "spec", "observed"].map((h) => (
                    <th key={h} className="o-label" style={{ textAlign: "left", padding: "6px 12px", color: "var(--o-ink-3)", borderBottom: "1px solid var(--o-border)" }}>
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {suite.results.map((r) => (
                  <Row key={r.id} r={r} />
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            each check&apos;s verdict shows when the run ends
          </span>
        )}
      </div>
    </Panel>
  );
}

function Row({ r }: { r: ConformanceResult }) {
  return (
    <tr style={{ borderBottom: "1px solid var(--o-border)" }}>
      <td style={{ padding: "5px 12px" }}>
        <span className="o-label" style={{ color: verdictColor(r.verdict), fontWeight: 600 }}>
          {r.verdict}
        </span>
        {r.expectedFailure && (
          <span className="o-label" title={`${r.expectedFailure.reason} (added ${r.expectedFailure.added})`} style={{ marginLeft: "0.5rem", color: "var(--o-ink-3)" }}>
            expected: {r.expectedFailure.finding}
          </span>
        )}
      </td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink)", fontSize: "0.82rem" }}>{r.id}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>{r.category}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>
        <SpecCell r={r} />
      </td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-2)", fontSize: "0.82rem" }}>{r.observed || r.detail}</td>
    </tr>
  );
}

// SpecCell shows the release a check requires and its clauses rendered the
// same way as the CLI and JSON; it expands to the 3GPP versions each clause
// was verified in.
function SpecCell({ r }: { r: ConformanceResult }) {
  if (r.specRefs.length === 0) return <>{r.specRef}</>;
  return (
    <details>
      <summary style={{ cursor: "pointer" }}>
        {r.minRelease > 0 ? `requires Rel-${r.minRelease} or later; ` : ""}
        {r.specRef}
      </summary>
      <ul style={{ margin: "4px 0 0 1rem", padding: 0 }}>
        {r.specRefs.map((ref) => (
          <li key={`${ref.spec} ${ref.clause}`}>
            {ref.spec} {ref.clause} ({ref.strength}, since Rel-{ref.since}): {ref.verified.map((v) => `Rel-${v.release} ${v.version}`).join(", ")}
          </li>
        ))}
      </ul>
    </details>
  );
}

function Stat({ label, value, color }: { label: string; value: number; color: string }) {
  return (
    <div className="flex items-baseline gap-1.5">
      <span style={{ fontFamily: "var(--o-font-mono)", fontWeight: 600, color }}>{value}</span>
      <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
        {label}
      </span>
    </div>
  );
}
