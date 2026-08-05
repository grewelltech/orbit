/**
 * CohortCard — one app cohort's live quality.
 *
 * Shows only the families the cohort's app produces. The server omits the
 * rest, and rendering an absent distribution as 0 would present "not
 * measured" as "measured perfect" — the same reason the origin's TTFB is
 * suppressed rather than printed as 0.00ms.
 */
import { Panel } from "@/components/Panel";
import type { CohortQuality, Quantiles } from "@/data/types";
import { bps } from "@/lib/format";

function Row({ label, q, unit, digits = 2 }: { label: string; q: Quantiles | null; unit?: string; digits?: number }) {
  if (!q) return null;
  return (
    <div className="flex items-baseline justify-between gap-3 px-3 py-1">
      <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
        {label}
      </span>
      <span className="font-mono text-xs">
        {q.p5.toFixed(digits)} / <strong>{q.p50.toFixed(digits)}</strong> / {q.p95.toFixed(digits)}
        {unit ? ` ${unit}` : ""}
      </span>
    </div>
  );
}

/**
 * A slot for one app kind. Rendered whether or not the run has such a cohort,
 * so the three cards keep fixed positions: a panel that appears and disappears
 * with the traffic mix moves everything below it, and an operator learns where
 * to look rather than re-reading the layout each run.
 */
export function CohortSlot({ app, cohort }: { app: string; cohort: CohortQuality | null }) {
  if (!cohort) {
    return (
      <Panel title={app} flush>
        <p className="o-label px-3 py-3" style={{ color: "var(--o-ink-3)" }}>
          no {app} cohort in this run
        </p>
      </Panel>
    );
  }
  return <CohortCard cohort={cohort} />;
}

export function CohortCard({ cohort: c }: { cohort: CohortQuality }) {
  return (
    <Panel
      title={`${c.name} · ${c.app}`}
      live
      flush
      meta={
        <span
          className="o-label"
          style={{ color: c.failed > 0 ? "var(--o-error)" : "var(--o-ink-3)" }}
        >
          {c.ues} UEs
          {c.failed > 0 ? ` · ${c.failed} failed` : ""} · {Math.round(c.elapsedMs / 1000)}s
        </span>
      }
    >
      <div className="py-1">
        <p className="o-label px-3 pb-1" style={{ color: "var(--o-ink-3)" }}>
          p5 / p50 / p95 across members
        </p>
        <Row label="MOS" q={c.mos} />
        <Row label="TTFB" q={c.ttfbMs} unit="ms" digits={1} />
        <Row label="goodput" q={c.goodputMbps} unit="Mbps" />
        <Row label="bitrate" q={c.bitrateKbps} unit="kbps" digits={0} />
        <Row label="stall" q={c.stallTimeMs} unit="ms" digits={0} />
        <Row label="rebuffer" q={c.rebufferRatio} digits={3} />
        <Row label="startup" q={c.startupMs} unit="ms" digits={0} />
        {c.farEnd && (
          <div
            className="mt-1 px-3 pt-1"
            style={{ borderTop: "1px solid var(--o-border)" }}
          >
            {c.farEnd.available ? (
              <div className="flex items-baseline justify-between gap-3">
                <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
                  N6 far end
                </span>
                <span className="font-mono text-xs">
                  {(() => {
                    const r = bps(c.farEnd.bitsPerSec);
                    return `${r.value} ${r.unit}`;
                  })()}
                  {c.farEnd.requests > 0 ? ` · ${c.farEnd.requests} reqs` : ""}
                  {c.farEnd.errors > 0 ? ` · ${c.farEnd.errors} err` : ""}
                </span>
              </div>
            ) : (
              // The reason, not a blank: an unwatched far end must not read as
              // a far end that received nothing.
              <p className="o-label" style={{ color: "var(--o-ink-3)" }}>
                N6 far end — {c.farEnd.reason || "not collected"}
              </p>
            )}
          </div>
        )}
      </div>
    </Panel>
  );
}
