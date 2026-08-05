/**
 * Header — run identity and connection state.
 *
 * Answers, at a glance and from across a room: is this live, what is running,
 * and how long has it been going.
 */
import { StatusDot, type Status } from "@/components/StatusDot";
import type { RunState, SourceState, TelemetryFrame } from "@/data/types";
import { duration } from "@/lib/format";

const SOURCE_STATUS: Record<SourceState, { status: Status; label: string }> = {
  connecting: { status: "warn", label: "connecting" },
  connected: { status: "ok", label: "connected" },
  live: { status: "active", label: "live" },
  stalled: { status: "warn", label: "stalled" },
  disconnected: { status: "idle", label: "disconnected" },
  error: { status: "error", label: "error" },
};

const RUN_STATUS: Record<RunState, Status> = {
  idle: "idle",
  starting: "warn",
  running: "active",
  draining: "warn",
  complete: "ok",
  failed: "error",
};

export interface HeaderProps {
  frame: TelemetryFrame | null;
  sourceState: SourceState;
  sourceName: string;
}

export function Header({ frame, sourceState, sourceName }: HeaderProps) {
  const src = SOURCE_STATUS[sourceState];
  const run = frame?.run;

  return (
    <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-[var(--o-border)] bg-[var(--o-surface)]/70 px-4 py-2.5 backdrop-blur">
      <div className="flex items-baseline gap-2.5">
        <span
          style={{
            fontFamily: "var(--o-font-mono)",
            fontSize: "var(--o-text-base)",
            fontWeight: 600,
            letterSpacing: "var(--o-tracking-title)",
            color: "var(--o-ink)",
          }}
        >
          ORBIT
        </span>
        <span className="o-label" style={{ color: "var(--o-accent)" }}>
          live
        </span>
      </div>

      <div className="hidden h-4 w-px bg-[var(--o-border)] sm:block" />

      <Field label="scenario" value={run?.scenario ?? "—"} />
      <Field label="run" value={run?.runId ?? "—"} />
      <Field label="elapsed" value={duration(run?.elapsedMs ?? 0)} />

      <div className="ml-auto flex items-center gap-4">
        {run && <StatusDot status={RUN_STATUS[run.state]} label={run.state} />}
        <StatusDot status={src.status} label={`${src.label} · ${sourceName}`} />
      </div>
    </header>
  );
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline gap-2">
      <span className="o-label">{label}</span>
      <span className="o-num" style={{ fontSize: "var(--o-text-xs)", color: "var(--o-ink)" }}>
        {value}
      </span>
    </div>
  );
}
