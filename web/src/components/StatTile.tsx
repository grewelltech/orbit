/**
 * StatTile — a single headline number.
 *
 * The right form when the reader's question is "what is it right now?" rather
 * than "how has it moved?". A sparkline is optional and deliberately
 * unlabelled: it conveys shape, not values.
 */
import type { ReactNode } from "react";
import { Sparkline } from "./Sparkline";

export type StatTone = "neutral" | "ok" | "warn" | "error" | "accent";

const TONE_INK: Record<StatTone, string> = {
  neutral: "var(--o-ink)",
  ok: "var(--o-ok)",
  warn: "var(--o-warn)",
  error: "var(--o-error)",
  accent: "var(--o-accent)",
};

export interface StatTileProps {
  label: string;
  value: ReactNode;
  /** Unit suffix, rendered small and muted beside the value. */
  unit?: string;
  tone?: StatTone;
  /** Secondary line — a delta, a target, or a qualifier. */
  detail?: ReactNode;
  /** Recent history for the sparkline. Omit for a bare number. */
  history?: readonly number[];
}

export function StatTile({ label, value, unit, tone = "neutral", detail, history }: StatTileProps) {
  const ink = TONE_INK[tone];
  return (
    <div className="flex min-w-0 flex-col justify-between gap-2 border border-[var(--o-border)] bg-[var(--o-surface)] px-3 py-2.5"
      style={{ borderRadius: "var(--o-radius-lg)" }}>
      <div className="o-label truncate">{label}</div>

      <div className="flex items-end justify-between gap-3">
        <div className="flex min-w-0 items-baseline gap-1.5">
          <span
            className="o-num truncate leading-none"
            style={{ fontSize: "var(--o-text-metric)", color: ink, fontWeight: 500 }}
          >
            {value}
          </span>
          {unit && (
            <span className="o-num shrink-0 leading-none" style={{ fontSize: "var(--o-text-xs)", color: "var(--o-ink-3)" }}>
              {unit}
            </span>
          )}
        </div>

        {history && history.length > 1 && (
          <Sparkline values={history} stroke={ink} className="shrink-0" width={72} height={22} />
        )}
      </div>

      {detail != null && (
        <div className="o-num truncate" style={{ fontSize: "var(--o-text-2xs)", color: "var(--o-ink-3)" }}>
          {detail}
        </div>
      )}
    </div>
  );
}
