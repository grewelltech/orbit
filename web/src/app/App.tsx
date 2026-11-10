/**
 * App — the live-watch layout.
 *
 * Reading order top to bottom: identity and health, then the headline numbers,
 * then the time-series that explain them, then the event stream that accounts
 * for individual occurrences. Density is deliberate — an operator watching a
 * run should not have to scroll to see whether it is healthy.
 */
import { useCallback, useMemo, useState } from "react";
import { Header } from "./Header";
import { DEFAULT_WINDOW_MS, TIME_WINDOWS } from "@/panels/TimeSeriesPanel";
import { MockSource } from "@/data/mock";
import { useRuns } from "@/data/runs";
import { RunDashboard } from "@/app/dashboards/RunDashboard";
import { RunPicker } from "@/app/RunPicker";
import { ProvenancePanel } from "@/app/ProvenancePanel";
import { Panel } from "@/components/Panel";
import { Modal } from "@/components/Modal";
import { StopControl } from "@/app/StopControl";
import { runConfigRefusal, runEnded, useRunConfig } from "@/data/runConfig";
import { ConnectSource } from "@/data/connect";
import type { TelemetrySource } from "@/data/source";
import { useTheme } from "@/theme/useTheme";
import { useTelemetry } from "@/hooks/useTelemetry";

export function App() {
  // ?mock forces the offline demo generator; ?run=<id> pins a run, and is kept
  // in step with the picker so a selection survives a reload and can be shared
  // as a link.
  const mock = useMemo(() => new URLSearchParams(window.location.search).has("mock"), []);
  const [selectedRun, setSelectedRun] = useState<string | undefined>(
    () => new URLSearchParams(window.location.search).get("run") ?? undefined,
  );
  // The resolved-config detail is off-view until asked for: it matters to few
  // people and rarely, so it opens in a modal rather than taking a band across
  // the dashboard.
  const [showConfig, setShowConfig] = useState(false);
  const runs = useRuns();

  // Switching runs replaces the source outright rather than mutating it: a
  // stream carries one run's series, and tearing it down is how the hook knows
  // to clear the history it holds instead of splicing two runs together.
  const source = useMemo<TelemetrySource>(() => {
    if (mock) return new MockSource();
    return new ConnectSource(selectedRun ? { runId: selectedRun } : {});
  }, [mock, selectedRun]);

  const selectRun = useCallback((runId: string | undefined) => {
    setSelectedRun(runId);
    const url = new URL(window.location.href);
    if (runId) url.searchParams.set("run", runId);
    else url.searchParams.delete("run");
    window.history.replaceState(null, "", url);
  }, []);
  const telemetry = useTelemetry(source);
  const { latest, events, state } = telemetry;
  const shownRun = (selectedRun ?? latest?.run.runId) || undefined;
  const runConfig = useRunConfig(mock ? undefined : shownRun, runEnded(latest?.run.state));

  // Tailor the view to the kind of run. A conformance run is about verdicts,
  // not throughput, so its view drops the load charts and leads with the suite
  // results; a load, fleet or scenario run keeps the performance charts; a
  // mixed run shows both. The kind comes from the run list; until it resolves
  // (or in mock, which is always a load soak) we default to the performance
  // view so charts are never hidden by accident.
  const runKind = useMemo(() => runs.find((r) => r.runId === shownRun)?.kind, [runs, shownRun]);
  // The dashboard follows the kind; in mock (always a load soak) that is the performance view.
  const kind = mock ? undefined : runKind;
  // Conformance verdicts are the suite view's job; keep them out of every
  // event log so a verdict burst can't bury the registration and error lines.
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
        <RunDashboard
          kind={kind}
          telemetry={telemetry}
          selectedRun={selectedRun}
          runId={shownRun}
          windowMs={windowMs}
          logEvents={logEvents}
          controls={
          <div className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2">
            {!mock && <RunPicker runs={runs} selected={selectedRun} onSelect={selectRun} />}
            {!mock && <StopControl runId={(selectedRun ?? latest?.run.runId) || undefined} state={latest?.run.state} />}
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
          }
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
