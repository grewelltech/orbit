/**
 * SuiteResults — a run's conformance suites on the run view: each suite's
 * progress and each check's verdict as it lands while the run is in
 * flight, then every check's full result once it has ended. Shown only
 * for a run that has a suite (a conformance run, or a mixed run with a
 * conformance workload).
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { ConformanceService, type ConformanceResult, type ConformanceTestInfo } from "@/gen/orbit/v1/conformance_pb";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { Panel } from "@/components/Panel";
import { failedLabel, suiteTally, SuiteWatch, type CheckVerdict, type Suite } from "@/data/suite";
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

/** Follows runId's suites and its checks' verdicts as they land; none when there is no run or it has no suite. */
function useSuites(runId: string | undefined, baseUrl?: string): { suites: Suite[]; done: boolean; live: CheckVerdict[] } {
  const client = useMemo(() => createClient(RunService, orbitTransport(baseUrl)), [baseUrl]);
  const [view, setView] = useState<{ suites: Suite[]; done: boolean }>({ suites: [], done: false });
  const [live, setLive] = useState<CheckVerdict[]>([]);
  useEffect(() => {
    setView({ suites: [], done: false });
    setLive([]);
    if (!runId) return;
    const w = new SuiteWatch(client, runId, (suites, done) => setView({ suites, done }), 1000, (v) => setLive((l) => [...l, v]));
    void w.start();
    return () => w.dispose();
  }, [client, runId]);
  return { ...view, live };
}

/** The suite's checks by ID (ListConformanceTests), for a verdict that names only its check. */
function useCheckInfo(): ReadonlyMap<string, ConformanceTestInfo> {
  const client = useMemo(() => createClient(ConformanceService, orbitTransport()), []);
  const [info, setInfo] = useState<ReadonlyMap<string, ConformanceTestInfo>>(new Map());
  useEffect(() => {
    let on = true;
    client
      .listConformanceTests({})
      .then((r) => on && setInfo(new Map(r.tests.map((t) => [t.id, t]))))
      .catch(() => {
        /* the verdicts show without their category and spec */
      });
    return () => {
      on = false;
    };
  }, [client]);
  return info;
}

export function SuiteResults({ runId }: { runId: string | undefined }) {
  const { suites, done, live } = useSuites(runId);
  const info = useCheckInfo();
  if (suites.length === 0) return null;
  return (
    <section className="flex shrink-0 flex-col gap-3">
      {suites.map((s) => (
        <SuitePanel key={s.name} suite={s} named={suites.length > 1} done={done} live={live.filter((v) => v.workload === s.name)} info={info} />
      ))}
    </section>
  );
}

function SuitePanel({
  suite,
  done,
  live,
  info,
  named,
}: {
  suite: Suite;
  /** The workload's name goes in the title when the run has more than one suite. */
  named: boolean;
  done: boolean;
  live: readonly CheckVerdict[];
  info: ReadonlyMap<string, ConformanceTestInfo>;
}) {
  const p = suite.progress;
  const tally = suiteTally(suite, done);
  const meta = done ? `${suite.results.length} checks` : p ? `${p.ran} of ${p.total}${p.lastCheck ? ` · ${p.lastCheck}` : ""}` : "starting";
  return (
    <Panel title={named && suite.name ? `conformance · ${suite.name}` : "conformance"} live={!done} meta={meta}>
      <div className="flex flex-col gap-2">
        <div className="flex flex-wrap gap-x-5 gap-y-1">
          <Stat label="pass" value={tally.pass} color={verdictColor("PASS")} />
          <Stat label={failedLabel(tally.expected, suite.expectedCore)} value={tally.fail} color={verdictColor("FAIL")} />
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
          <div className="flex flex-col gap-1">
            <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
              {live.length === 0 ? "verdicts show as each check ends; the full results when the run ends" : "verdicts so far; the full results when the run ends"}
            </span>
            {live.length > 0 && (
              <div className="max-h-[24rem] overflow-auto">
                <table className="w-full" style={{ borderCollapse: "collapse", fontFamily: "var(--o-font-mono)" }}>
                  <tbody>
                    {live.map((v, i) => (
                      <tr key={`${i}:${v.id}`} style={{ borderBottom: "1px solid var(--o-border)" }}>
                        <td style={{ padding: "4px 12px" }}>
                          <span className="o-label" style={{ color: verdictColor(v.verdict), fontWeight: 600 }}>
                            {v.verdict}
                          </span>
                        </td>
                        <td style={{ padding: "4px 12px", color: "var(--o-ink)", fontSize: "0.82rem" }}>{v.id}</td>
                        <td style={{ padding: "4px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>{info.get(v.id)?.category ?? ""}</td>
                        <td
                          className="max-w-[32rem] truncate"
                          title={info.get(v.id)?.specRef}
                          style={{ padding: "4px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}
                        >
                          {info.get(v.id)?.specRef ?? ""}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
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
