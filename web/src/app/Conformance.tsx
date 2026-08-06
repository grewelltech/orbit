/**
 * The conformance view: run the core conformance suite and watch verdicts land.
 *
 * Reached at ?conformance. Deliberately its own page rather than a panel on the
 * run dashboard — a conformance run is a different question ("does this core
 * behave to spec") from a load run ("how much can it take"), and mixing the two
 * controls would muddle both.
 */
import { useState } from "react";
import { useConformance, type ConformanceResult } from "@/data/conformance";

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

const DEFAULT_AMF = "10.102.0.10:38412";

export function Conformance() {
  const { state, run, cancel } = useConformance();
  const [amf, setAmf] = useState(DEFAULT_AMF);

  const t = state.tally;
  const pct = state.progress != null ? Math.round(state.progress * 100) : 0;

  return (
    <div className="flex h-dvh flex-col overflow-hidden" style={{ background: "var(--o-bg)" }}>
      <header
        className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b px-4 py-2.5"
        style={{ borderColor: "var(--o-border)", background: "var(--o-surface)" }}
      >
        <span
          style={{
            fontFamily: "var(--o-font-mono)",
            fontWeight: 600,
            letterSpacing: "var(--o-tracking-title)",
            color: "var(--o-ink)",
          }}
        >
          ORBIT
        </span>
        <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
          conformance
        </span>

        <div className="ml-auto flex items-center gap-2">
          <input
            value={amf}
            onChange={(e) => setAmf(e.target.value)}
            spellCheck={false}
            aria-label="AMF N2 endpoint"
            className="o-label px-2 py-1"
            style={{
              width: "16rem",
              color: "var(--o-ink-2)",
              background: "var(--o-surface-2)",
              border: "1px solid var(--o-border)",
              borderRadius: "var(--o-radius)",
            }}
          />
          {state.running ? (
            <button
              type="button"
              onClick={cancel}
              className="o-label cursor-pointer px-3 py-1"
              style={{
                color: "var(--o-amber)",
                border: "1px solid var(--o-border)",
                borderRadius: "var(--o-radius)",
              }}
            >
              cancel
            </button>
          ) : (
            <button
              type="button"
              onClick={() => run({ amfAddress: amf })}
              disabled={!amf}
              className="o-label cursor-pointer px-3 py-1"
              style={{
                color: "var(--o-bg)",
                background: "var(--o-accent)",
                borderRadius: "var(--o-radius)",
              }}
            >
              run suite
            </button>
          )}
        </div>
      </header>

      {/* Tally strip + progress. */}
      <div
        className="flex items-center gap-5 border-b px-4 py-2"
        style={{ borderColor: "var(--o-border)" }}
      >
        <Stat label="passed" value={t?.passed ?? 0} color="var(--o-accent)" />
        <Stat label="failed" value={t?.failed ?? 0} color={verdictColor("FAIL")} />
        <Stat label="errored" value={t?.errored ?? 0} color={verdictColor("ERROR")} />
        <Stat label="deviations" value={t?.deviations ?? 0} color="var(--o-amber)" />
        <Stat label="skipped" value={t?.skipped ?? 0} color="var(--o-ink-3)" />
        <div className="ml-auto flex items-center gap-2" style={{ minWidth: "12rem" }}>
          <div
            className="h-1.5 flex-1 overflow-hidden"
            style={{ background: "var(--o-surface-2)", borderRadius: "999px" }}
          >
            <div
              style={{
                width: `${pct}%`,
                height: "100%",
                background: state.done ? "var(--o-accent)" : "var(--o-ink-3)",
                transition: "width 180ms var(--o-ease, ease)",
              }}
            />
          </div>
          <span className="o-label" style={{ color: "var(--o-ink-3)", width: "3rem", textAlign: "right" }}>
            {state.running || state.done ? `${pct}%` : "idle"}
          </span>
        </div>
      </div>

      {state.error && (
        <div className="px-4 py-2 o-label" style={{ color: verdictColor("FAIL") }}>
          {state.error}
        </div>
      )}

      <div className="flex-1 overflow-auto">
        <table className="w-full" style={{ borderCollapse: "collapse", fontFamily: "var(--o-font-mono)" }}>
          <thead>
            <tr style={{ position: "sticky", top: 0, background: "var(--o-surface)" }}>
              {["verdict", "test", "category", "spec", "observed"].map((h) => (
                <th
                  key={h}
                  className="o-label"
                  style={{
                    textAlign: "left",
                    padding: "6px 12px",
                    color: "var(--o-ink-3)",
                    borderBottom: "1px solid var(--o-border)",
                  }}
                >
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {state.results.map((r) => (
              <Row key={r.id} r={r} />
            ))}
            {state.results.length === 0 && (
              <tr>
                <td
                  colSpan={5}
                  className="o-label"
                  style={{ padding: "24px 12px", color: "var(--o-ink-3)", textAlign: "center" }}
                >
                  {state.running ? "running…" : "no results yet — run the suite"}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function Row({ r }: { r: ConformanceResult }) {
  return (
    <tr style={{ borderBottom: "1px solid var(--o-border)" }}>
      <td style={{ padding: "5px 12px" }}>
        <span
          className="o-label"
          style={{ color: verdictColor(r.verdict), fontWeight: 600 }}
        >
          {r.verdict}
        </span>
      </td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink)", fontSize: "0.82rem" }}>{r.id}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>{r.category}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>{r.specRef}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-2)", fontSize: "0.82rem" }}>
        {r.observed || r.detail}
      </td>
    </tr>
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
