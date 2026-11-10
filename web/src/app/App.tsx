/**
 * App — the run dashboard shell at /runs and /runs/:id.
 *
 * It owns the chrome shared by every run kind — identity and health (Header),
 * the run picker, stop control, time window and resolved-config modal — and
 * delegates the body to RunDashboard, which renders the performance or
 * conformance view (or both, for a mixed run) from the same telemetry stream.
 * The view is identical for a live run and an archived one; only the charts'
 * time-axis auto-scroll is live-only.
 */
import { useCallback, useMemo, useState } from "react";
import { useNavigate, useParams } from "react-router";
import { Header } from "./Header";
import { useRuns } from "@/data/runs";
import { RunPicker } from "@/app/RunPicker";
import { ProvenancePanel } from "@/app/ProvenancePanel";
import { Panel } from "@/components/Panel";
import { Modal } from "@/components/Modal";
import { StopControl } from "@/app/StopControl";
import { runConfigRefusal, runEnded, useRunConfig } from "@/data/runConfig";
import { MockSource } from "@/data/mock";
import { ConnectSource } from "@/data/connect";
import type { TelemetrySource } from "@/data/source";
import { useTheme } from "@/theme/useTheme";
import { useTelemetry } from "@/hooks/useTelemetry";
import { DEFAULT_WINDOW_MS, TIME_WINDOWS } from "@/panels/TimeSeriesPanel";
import { RunDashboard } from "./dashboards/RunDashboard";

export function App() {
  // ?mock forces the offline demo generator; /runs/:id pins a run (shareable,
  // survives a reload), and /runs follows whatever is live.
  const { id: pinnedRun } = useParams();
  const navigate = useNavigate();
  const mock = useMemo(() => new URLSearchParams(window.location.search).has("mock"), []);
  const selectedRun = pinnedRun;

  // The resolved-config detail is off-view until asked for: it matters to few
  // people and rarely, so it opens in a modal rather than taking a band.
  const [showConfig, setShowConfig] = useState(false);
  const runs = useRuns();

  // Switching runs replaces the source outright rather than mutating it: a
  // stream carries one run's series, and tearing it down is how the hook knows
  // to clear the history it holds instead of splicing two runs together.
  const source = useMemo<TelemetrySource>(() => {
    if (mock) return new MockSource();
    return new ConnectSource(selectedRun ? { runId: selectedRun } : {});
  }, [mock, selectedRun]);

  const selectRun = useCallback(
    (runId: string | undefined) => {
      navigate(runId ? `/runs/${encodeURIComponent(runId)}` : "/runs");
    },
    [navigate],
  );

  const telemetry = useTelemetry(source);
  const { latest, events, state, clearEvents } = telemetry;
  const shownRun = (selectedRun ?? latest?.run.runId) || undefined;
  const runConfig = useRunConfig(mock ? undefined : shownRun, runEnded(latest?.run.state));

  // The kind drives which dashboard shows; until it resolves (or in mock,
  // always a load soak) RunDashboard defaults to the performance view.
  const runKind = useMemo(() => runs.find((r) => r.runId === shownRun)?.kind, [runs, shownRun]);
  // Conformance verdicts are the suite view's job; keep them out of the event
  // log so a verdict burst can't bury the registration and error lines.
  const logEvents = useMemo(() => events.filter((e) => !e.conformance), [events]);

  const { setting: themeSetting, theme, cycle: cycleTheme } = useTheme();
  // One span for every panel: the value of these charts is reading them
  // against each other, which a per-panel window would quietly break.
  const [windowMs, setWindowMs] = useState(DEFAULT_WINDOW_MS);

  return (
    <div className="flex h-dvh flex-col overflow-hidden">
      <Header
        frame={latest}
        sourceState={state}
        sourceName={source.name}
        theme={theme}
        themeSetting={themeSetting}
        onCycleTheme={cycleTheme}
      />

      <main className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3 pb-11">
        {/* Controls: which run, and over what span. */}
        <div className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2">
          {!mock && <RunPicker runs={runs} selected={selectedRun} onSelect={selectRun} />}
          {!mock && <StopControl runId={shownRun} state={latest?.run.state} />}
          <div className="flex items-center gap-1.5">
            <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
              window
            </span>
            {TIME_WINDOWS.map((w) => (
              <button
                key={w.label}
                type="button"
                onClick={() => setWindowMs(w.ms)}
                className="o-label cursor-pointer border px-1.5 py-0.5 transition-colors"
                style={{
                  color: windowMs === w.ms ? "var(--o-accent)" : "var(--o-ink-3)",
                  borderColor: windowMs === w.ms ? "var(--o-border-accent)" : "var(--o-border)",
                  borderRadius: "var(--o-radius)",
                  transitionDuration: "var(--o-dur-fast)",
                }}
                aria-pressed={windowMs === w.ms}
              >
                {w.label}
              </button>
            ))}
          </div>

          {/*
            How this run resolved every config field, and what overrode what.
            It opens a modal rather than sitting on the dashboard: few runs are
            watched for it, and a surprising override is the one thing worth a
            glance, so any notices are flagged in amber on the button itself.
          */}
          {!mock && (runConfig.config || runConfigRefusal(runConfig)) && (
            <button
              type="button"
              onClick={() => setShowConfig(true)}
              className="o-label ml-auto flex cursor-pointer items-center gap-1.5 border px-1.5 py-0.5 transition-colors"
              style={{
                color: "var(--o-ink-3)",
                borderColor: "var(--o-border)",
                borderRadius: "var(--o-radius)",
                transitionDuration: "var(--o-dur-fast)",
              }}
              title="How this run resolved every config field, and what overrode what"
            >
              resolved config
              {(runConfig.config?.notices.length ?? 0) > 0 && (
                <span style={{ color: "var(--o-amber)" }}>
                  {runConfig.config!.notices.length} notice{runConfig.config!.notices.length === 1 ? "" : "s"}
                </span>
              )}
            </button>
          )}
        </div>

        <RunDashboard
          kind={mock ? undefined : runKind}
          mock={mock}
          telemetry={telemetry}
          windowMs={windowMs}
          selectedRun={selectedRun}
          shownRun={shownRun}
          logEvents={logEvents}
          clearEvents={clearEvents}
        />
      </main>

      {showConfig && !mock && (
        <Modal label="resolved config" onClose={() => setShowConfig(false)}>
          {runConfig.config ? (
            <ProvenancePanel
              provenance={runConfig.config.provenance}
              notices={runConfig.config.notices}
              resolved={runConfig.config.resolved}
            />
          ) : (
            <Panel title="resolved config">
              <span className="o-label" role="status" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                {runConfigRefusal(runConfig)}
              </span>
            </Panel>
          )}
        </Modal>
      )}
    </div>
  );
}
