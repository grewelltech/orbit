/**
 * StatusDot — state indicator.
 *
 * Always ships with its label: state is never conveyed by colour alone, which
 * matters both for accessibility and for the screenshots that end up in bug
 * reports. The pulse marks a live/transitional state, not merely a healthy one.
 */
export type Status = "ok" | "warn" | "error" | "idle" | "active";

const STATUS_COLOR: Record<Status, string> = {
  ok: "var(--o-ok)",
  warn: "var(--o-warn)",
  error: "var(--o-error)",
  idle: "var(--o-idle)",
  active: "var(--o-accent)",
};

export interface StatusDotProps {
  status: Status;
  label: string;
  /** Hides the text label; the accessible name is still the label. */
  compact?: boolean;
}

export function StatusDot({ status, label, compact = false }: StatusDotProps) {
  const color = STATUS_COLOR[status];
  const pulsing = status === "active";
  return (
    <span className="inline-flex items-center gap-1.5" title={compact ? label : undefined}>
      <span className="relative inline-flex h-1.5 w-1.5 shrink-0">
        {pulsing && (
          <span
            aria-hidden
            className="absolute inline-flex h-full w-full animate-ping rounded-full opacity-60"
            style={{ background: color }}
          />
        )}
        <span className="relative inline-flex h-1.5 w-1.5 rounded-full" style={{ background: color }} />
      </span>
      {compact ? (
        <span className="sr-only">{label}</span>
      ) : (
        <span className="o-label" style={{ color: "var(--o-ink-2)" }}>
          {label}
        </span>
      )}
    </span>
  );
}
