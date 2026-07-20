/**
 * Panel — the single container primitive the dashboard is built from.
 *
 * One hairline border, an all-caps mono title rail, and an optional accent
 * rule that marks the panel as live. No corner ticks or decorative leaders:
 * at monitoring density they become visual noise rather than structure.
 */
import type { ReactNode } from "react";

export interface PanelProps {
  title: string;
  /** Right-aligned metadata in the title rail — units, counts, scope. */
  meta?: ReactNode;
  /** Draws the accent rule, indicating the panel is receiving live data. */
  live?: boolean;
  /** Removes body padding, for charts that should bleed to the border. */
  flush?: boolean;
  className?: string;
  children: ReactNode;
}

export function Panel({ title, meta, live = false, flush = false, className = "", children }: PanelProps) {
  return (
    <section
      className={`relative flex min-h-0 min-w-0 flex-col border border-[var(--o-border)] bg-[var(--o-surface)] ${className}`}
      style={{ borderRadius: "var(--o-radius-lg)" }}
    >
      {/* Live rule: a 1px accent bar along the top edge, inset to the radius. */}
      {live && (
        <span
          aria-hidden
          className="pointer-events-none absolute inset-x-0 top-0 h-px"
          style={{
            background: `linear-gradient(90deg, var(--o-accent) 0%, var(--o-accent-dim) 38%, transparent 100%)`,
          }}
        />
      )}

      <header className="flex shrink-0 items-baseline justify-between gap-3 border-b border-[var(--o-border-soft)] px-3 py-2">
        <h2 className="o-label truncate" style={{ color: "var(--o-ink-2)" }}>
          {title}
        </h2>
        {meta != null && (
          <div className="o-num shrink-0 text-[length:var(--o-text-2xs)]" style={{ color: "var(--o-ink-3)" }}>
            {meta}
          </div>
        )}
      </header>

      <div className={`min-h-0 flex-1 ${flush ? "" : "p-3"}`}>{children}</div>
    </section>
  );
}
