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

export function CohortCard({ cohort: c }: { cohort: CohortQuality }) {
  return (
    <Panel
      title={`${c.name} · ${c.app}`}
      live
      flush
      meta={
        <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
          {c.ues} UEs · {Math.round(c.elapsedMs / 1000)}s
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
      </div>
    </Panel>
  );
}
