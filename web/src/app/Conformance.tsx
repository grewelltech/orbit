/**
 * The conformance view: run the core conformance suite and watch verdicts land.
 *
 * Reached at ?conformance. Deliberately its own page rather than a panel on the
 * run dashboard — a conformance run is a different question ("does this core
 * behave to spec") from a load run ("how much can it take"), and mixing the two
 * controls would muddle both.
 */
import { useState } from "react";
import {
  failedLabel,
  identityComplete,
  loadIdentity,
  paramsFromIdentity,
  saveIdentity,
  useConformance,
  type ConformanceResult,
  type NetworkIdentity,
} from "@/data/conformance";

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

// One field of the network identity: label, width and placeholder. The
// server has no PLMN default, so the MCC/MNC placeholders are examples, not
// values that get sent.
const IDENTITY_FIELDS: { key: keyof NetworkIdentity; label: string; width: string; placeholder: string }[] = [
  { key: "mcc", label: "MCC", width: "3.5rem", placeholder: "001" },
  { key: "mnc", label: "MNC", width: "3.5rem", placeholder: "01" },
  { key: "tac", label: "TAC", width: "3.5rem", placeholder: "1" },
  { key: "sst", label: "SST", width: "3rem", placeholder: "1" },
  { key: "sd", label: "SD", width: "5rem", placeholder: "010203" },
];

export function Conformance() {
  const { state, run, cancel } = useConformance();
  const [amf, setAmf] = useState(DEFAULT_AMF);
  // The identity the suite registers under. Persisted so the next visit
  // starts on the last core's values; the PLMN itself is never guessed.
  const [identity, setIdentity] = useState<NetworkIdentity>(() => loadIdentity());
  const ready = Boolean(amf) && identityComplete(identity);

  const start = () => {
    saveIdentity(identity);
    run(paramsFromIdentity(amf, identity));
  };

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

        <div className="ml-auto flex flex-wrap items-center gap-2">
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
          {IDENTITY_FIELDS.map((f) => (
            <label key={f.key} className="flex items-center gap-1">
              <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
                {f.label}
              </span>
              <input
                value={identity[f.key]}
                onChange={(e) => setIdentity({ ...identity, [f.key]: e.target.value })}
                placeholder={f.placeholder}
                spellCheck={false}
                aria-label={f.label}
                className="o-label px-2 py-1"
                style={{
                  width: f.width,
                  color: "var(--o-ink-2)",
                  background: "var(--o-surface-2)",
                  border: "1px solid var(--o-border)",
                  borderRadius: "var(--o-radius)",
                }}
              />
            </label>
          ))}
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
              onClick={start}
              disabled={!ready}
              title={ready ? undefined : "AMF endpoint and PLMN (MCC/MNC) are required"}
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
        <Stat label={failedLabel(t, state.expectedCore)} value={t?.failed ?? 0} color={verdictColor("FAIL")} />
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

      {state.expectedCore && (
        <div className="border-b px-4 py-1.5 o-label" style={{ borderColor: "var(--o-border)", color: "var(--o-ink-3)" }}>
          expected failures: {state.expectedCore}
          {state.stale.map((s) => (
            <div key={`${s.check} ${s.finding}`} style={{ color: "var(--o-amber)" }}>
              stale expected failure: {s.check} {s.finding}: now {s.verdict} &quot;{s.observed}&quot;
            </div>
          ))}
        </div>
      )}

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
        {r.expectedFailure && (
          <span
            className="o-label"
            title={`${r.expectedFailure.reason} (added ${r.expectedFailure.added})`}
            style={{ marginLeft: "0.5rem", color: "var(--o-ink-3)" }}
          >
            expected: {r.expectedFailure.finding}
          </span>
        )}
      </td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink)", fontSize: "0.82rem" }}>{r.id}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>{r.category}</td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-3)", fontSize: "0.82rem" }}>
        <SpecCell r={r} />
      </td>
      <td style={{ padding: "5px 12px", color: "var(--o-ink-2)", fontSize: "0.82rem" }}>
        {r.observed || r.detail}
      </td>
    </tr>
  );
}

// SpecCell shows the release a check requires and its clauses rendered the
// same way as the CLI and JSON; it expands to the 3GPP versions each clause
// was verified in.
function SpecCell({ r }: { r: ConformanceResult }) {
  if (r.specRefs.length === 0) {
    return <>{r.specRef}</>;
  }
  return (
    <details>
      <summary style={{ cursor: "pointer" }}>
        {r.minRelease > 0 ? `requires Rel-${r.minRelease} or later; ` : ""}
        {r.specRef}
      </summary>
      <ul style={{ margin: "4px 0 0 1rem", padding: 0 }}>
        {r.specRefs.map((ref) => (
          <li key={`${ref.spec} ${ref.clause}`}>
            {ref.spec} {ref.clause} ({ref.strength}, since Rel-{ref.since}):{" "}
            {ref.verified.map((v) => `Rel-${v.release} ${v.version}`).join(", ")}
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
