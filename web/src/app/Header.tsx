/**
 * Header — run identity and connection state.
 *
 * Answers, at a glance and from across a room: is this live, what is running,
 * and how long has it been going.
 */
import { StatusDot, type Status } from "@/components/StatusDot";
import type { RunState, SourceState, TelemetryFrame } from "@/data/types";
import type { ThemeName, ThemeSetting } from "@/theme/useTheme";
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
  theme: ThemeName;
  themeSetting: ThemeSetting;
  onCycleTheme: () => void;
}

export function Header({ frame, sourceState, sourceName, theme, themeSetting, onCycleTheme }: HeaderProps) {
  const src = SOURCE_STATUS[sourceState];
  const run = frame?.run;
  const isMock = sourceName !== "orbit";

  return (
    <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-[var(--o-border)] bg-[var(--o-surface)]/70 px-4 py-2.5 backdrop-blur">
      <div className="flex items-baseline gap-2.5">
        {/*
          The wordmark expands on hover. The acronym is not self-evident, and
          the header is where someone new to the tool looks first — cursor:help
          signals there is something to hover for, which a bare title attribute
          does not.
        */}
        <span
          title="Open Radio Benchmark and Integration Testbed"
          style={{
            fontFamily: "var(--o-font-mono)",
            fontSize: "var(--o-text-base)",
            fontWeight: 600,
            letterSpacing: "var(--o-tracking-title)",
            color: "var(--o-ink)",
            cursor: "help",
          }}
        >
          ORBIT
        </span>
        {/*
          Live-vs-mock belongs here, beside the wordmark: it is the first thing
          worth knowing and the badge was previously a decorative literal that
          read "live" even when the numbers were synthetic. Amber because mock
          data is a caveat on everything else on screen.
        */}
        <span
          className="o-label"
          style={{ color: isMock ? "var(--o-amber)" : "var(--o-accent)" }}
          title={isMock ? "synthetic data — not a real core" : "live data from the ORBIT server"}
        >
          {isMock ? "mock" : "live"}
        </span>
      </div>

      <div className="hidden h-4 w-px bg-[var(--o-border)] sm:block" />

      <Field label="scenario" value={run?.scenario ?? "—"} />
      <Field label="run" value={run?.runId ?? "—"} />
      <Field label="elapsed" value={duration(run?.elapsedMs ?? 0)} />

      <div className="ml-auto flex items-center gap-4">
        {run && <StatusDot status={RUN_STATUS[run.state]} label={run.state} />}
        {/* Reachability only. Which source it is now reads beside the wordmark. */}
        <StatusDot status={src.status} label={src.label} />
        <button
          type="button"
          onClick={onCycleTheme}
          className="o-label cursor-pointer border px-1.5 py-0.5 transition-colors"
          style={{
            color: "var(--o-ink-3)",
            borderColor: "var(--o-border)",
            borderRadius: "var(--o-radius)",
            transitionDuration: "var(--o-dur-fast)",
          }}
          aria-label={`Theme: ${themeSetting}. Click to change.`}
          title={
            themeSetting === "system"
              ? `following the system (${theme})`
              : `pinned to ${themeSetting}`
          }
        >
          {/* The SETTING, not the resolved theme: "system" is a distinct
              choice from "dark", and a button that only ever showed the
              latter would hide whether the OS is still being followed. */}
          {themeSetting}
        </button>
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
