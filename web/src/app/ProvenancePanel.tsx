/**
 * ProvenancePanel — the Resolved config panel: every field a run (or, on
 * Launch, a plan about to start) resolved to, the layer and source that
 * set it, and the lower layers' values it replaced, struck through. The
 * notices (an env var that beat a file) sit on top. One component for a
 * past or live run (GetRunConfig) and for Launch (PlanRun).
 */
import { useMemo, useState } from "react";
import type { FieldProvenance } from "@/gen/orbit/v1/target_pb";
import type { OverrideNotice } from "@/gen/orbit/v1/run_pb";
import { Panel } from "@/components/Panel";
import { ALL_ROWS, filterRows, madeBy, noticeText, rows, type RowFilter } from "@/data/provenance";

const inputStyle = { color: "var(--o-ink-2)", background: "var(--o-surface-2)", border: "1px solid var(--o-border)", borderRadius: "var(--o-radius)" };

export function ProvenancePanel({
  provenance,
  notices,
  resolved,
  footnote,
}: {
  provenance: readonly FieldProvenance[];
  notices: readonly OverrideNotice[];
  resolved: boolean;
  /** A line under the title (Launch: there is no env layer here). */
  footnote?: string;
}) {
  const [filter, setFilter] = useState<RowFilter>(ALL_ROWS);
  const all = useMemo(() => rows(provenance), [provenance]);
  const shown = filterRows(all, filter);
  return (
    <Panel title="resolved config" meta={resolved ? `${shown.length} of ${all.length} fields` : "no provenance"}>
      <div className="flex flex-col gap-2">
        <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
          {madeBy(provenance, resolved)}
        </span>
        {footnote && (
          <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            {footnote}
          </span>
        )}
        {notices.length > 0 && (
          <ul className="flex flex-col gap-0.5" aria-label="notices">
            {notices.map((n) => (
              <li key={n.path} className="o-label" role="note" style={{ color: "var(--o-amber)", textTransform: "none" }}>
                notice: {noticeText(n)}
              </li>
            ))}
          </ul>
        )}
        {resolved && (
          <>
            <div className="flex flex-wrap items-center gap-3">
              <input
                aria-label="filter fields"
                placeholder="field contains…"
                value={filter.text}
                onChange={(e) => setFilter({ ...filter, text: e.target.value })}
                className="o-label px-2 py-0.5"
                style={{ ...inputStyle, textTransform: "none" }}
              />
              <label className="o-label flex items-center gap-1" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                <input type="checkbox" checked={filter.overriddenOnly} onChange={(e) => setFilter({ ...filter, overriddenOnly: e.target.checked })} />
                only overridden
              </label>
              <label className="o-label flex items-center gap-1" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                <input type="checkbox" checked={filter.commandLineOnly} onChange={(e) => setFilter({ ...filter, commandLineOnly: e.target.checked })} />
                only env or flag
              </label>
            </div>
            <div className="max-h-[28rem] overflow-auto">
              <table className="w-full" style={{ borderCollapse: "collapse", fontFamily: "var(--o-font-mono)", fontSize: "0.8rem" }}>
                <thead>
                  <tr>
                    {["field", "value", "layer", "source"].map((h) => (
                      <th key={h} className="o-label" style={{ textAlign: "left", padding: "4px 10px", color: "var(--o-ink-3)", borderBottom: "1px solid var(--o-border)" }}>
                        {h}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {shown.map((r) => (
                    <tr key={r.path} style={{ borderBottom: "1px solid var(--o-border)", verticalAlign: "top" }}>
                      <td style={{ padding: "4px 10px", color: "var(--o-ink)" }}>{r.path}</td>
                      <td style={{ padding: "4px 10px", color: "var(--o-ink)" }}>
                        <div>{r.value}</div>
                        {r.struck.map((s, i) => (
                          <div key={i} style={{ color: "var(--o-ink-3)" }}>
                            <s>{s.value}</s>
                          </div>
                        ))}
                      </td>
                      <td style={{ padding: "4px 10px", color: r.commandLine ? "var(--o-amber)" : "var(--o-ink-2)" }}>
                        <div>{r.layer}</div>
                        {r.struck.map((s, i) => (
                          <div key={i} style={{ color: "var(--o-ink-3)" }}>
                            <s>{s.layer}</s>
                          </div>
                        ))}
                      </td>
                      <td style={{ padding: "4px 10px", color: "var(--o-ink-3)" }}>
                        <div>{r.source}</div>
                        {r.struck.map((s, i) => (
                          <div key={i}>
                            <s>{s.source}</s>
                          </div>
                        ))}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </div>
    </Panel>
  );
}
