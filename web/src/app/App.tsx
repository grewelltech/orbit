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
import { StatTile } from "@/components/StatTile";
import { EventStream } from "@/panels/EventStream";
import { CohortSlot } from "@/panels/CohortCard";
import { FlowTable } from "@/panels/FlowTable";
import { GnbDistribution } from "@/panels/GnbDistribution";
import {
  DEFAULT_WINDOW_MS,
  TIME_WINDOWS,
  TimeSeriesPanel,
  type SeriesDef,
} from "@/panels/TimeSeriesPanel";
import { MockSource } from "@/data/mock";
import { useRuns } from "@/data/runs";
import { RunPicker } from "@/app/RunPicker";
import { ConnectSource } from "@/data/connect";
import type { TelemetrySource } from "@/data/source";
import type { TelemetryFrame } from "@/data/types";
import { useTheme } from "@/theme/useTheme";
import { useTelemetry } from "@/hooks/useTelemetry";
import { tokens } from "@/theme/tokens";
import { bps, count, ms, pct } from "@/lib/format";

/** Fixed slot order for the cohort panels — voice, web, then video. */
const COHORT_APPS = ["voip", "http", "video"] as const;

export function App() {
  // ?mock forces the offline demo generator; ?run=<id> pins a run, and is kept
  // in step with the picker so a selection survives a reload and can be shared
  // as a link.
  const mock = useMemo(() => new URLSearchParams(window.location.search).has("mock"), []);
  const [selectedRun, setSelectedRun] = useState<string | undefined>(
    () => new URLSearchParams(window.location.search).get("run") ?? undefined,
  );
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
  const { latest, history, events, state, clearEvents } = telemetry;

  const { setting: themeSetting, theme, cycle: cycleTheme } = useTheme();
  // One span for every panel: the value of these charts is reading them
  // against each other, which a per-panel window would quietly break.
  const [windowMs, setWindowMs] = useState(DEFAULT_WINDOW_MS);
  const t = tokens();

  // Sessions in context: a bare count says nothing about whether it is all of
  // them. Prefer the run's declared target; otherwise the funnel total, which
  // is every UE seen including the ones that failed.
  const sessionsDetail = (f: TelemetryFrame | null): string => {
    if (f?.run.targetUes) return `target ${count(f.run.targetUes)}`;
    const u = f?.ues;
    // Always a string: an omitted detail collapses the tile and leaves it
    // shorter than the row, and the idle case — no run yet — is exactly when
    // the earlier version returned nothing.
    if (!u) return "awaiting run";
    const total = u.sessionActive + u.registered + u.registering + u.failed;
    return total > 0 ? `of ${count(total)} UEs` : "no UEs yet";
  };

  // Mobility, stated as what happened: no handovers at all is a different
  // fact from every handover succeeding, and the tile should not blur them.
  const handoverDetail = (f: TelemetryFrame | null): string => {
    const h = f?.mobility.handovers ?? 0;
    const bad = f?.mobility.failed ?? 0;
    if (h + bad === 0) return "no handovers";
    return bad > 0 ? `handovers ${h} ok · ${bad} failed` : `handovers ${h} ok`;
  };

  // Stack order is bottom-first, and `failed` goes at the bottom deliberately.
  // Stacked on top it draws its band at the population's ceiling, so a healthy
  // run of 100 UEs renders a red line across the top of the chart and reads as
  // 100 failures. At the bottom the red band is the height of the failure
  // count — invisible when nothing has failed, which is the honest picture.
  // The rest ascend by progress through attach, so the chart fills upward as
  // the population advances.
  const ueSeries = useMemo<SeriesDef[]>(
    () => [
      { name: "failed", color: t.series[4] as string, value: (f) => f.ues.failed, area: true },
      { name: "registering", color: t.series[1] as string, value: (f) => f.ues.registering, area: true },
      { name: "registered", color: t.series[3] as string, value: (f) => f.ues.registered, area: true },
      { name: "session active", color: t.series[0] as string, value: (f) => f.ues.sessionActive, area: true },
    ],
    [t.series],
  );

  const rateSeries = useMemo<SeriesDef[]>(
    () => [
      { name: "attach/s", color: t.series[0] as string, value: (f) => f.rates.attachPerSec },
      { name: "handover/s", color: t.series[2] as string, value: (f) => f.rates.handoverPerSec },
    ],
    [t.series],
  );

  const throughputSeries = useMemo<SeriesDef[]>(
    () => [
      { name: "downlink", color: t.series[0] as string, value: (f) => f.throughput.downlinkBps, area: true },
      { name: "uplink", color: t.series[1] as string, value: (f) => f.throughput.uplinkBps, area: true },
    ],
    [t.series],
  );

  const latencySeries = useMemo<SeriesDef[]>(
    () => [
      { name: "p50", color: t.series[0] as string, value: (f) => f.cpLatency.p50 },
      { name: "p90", color: t.series[1] as string, value: (f) => f.cpLatency.p90 },
      { name: "p99", color: t.series[4] as string, value: (f) => f.cpLatency.p99 },
    ],
    [t.series],
  );

  // Sparkline history. Recomputed per publish, not per sample.
  const spark = useMemo(() => {
    const recent = history.toArray().slice(-60);
    return {
      sessions: recent.map((f) => f.ues.sessionActive),
      attach: recent.map((f) => f.rates.attachPerSec),
      downlink: recent.map((f) => f.throughput.downlinkBps),
      p99: recent.map((f) => f.cpLatency.p99),
    };
  }, [history, latest]);

  const dl = bps(latest?.throughput.downlinkBps ?? 0);
  const attachOk = latest ? latest.rates.attachSuccess : 1;

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

      <main className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3">
        {/* Headline numbers */}
        <section className="grid shrink-0 grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
          <StatTile
            label="sessions active"
            value={count(latest?.ues.sessionActive ?? 0)}
            tone="accent"
            history={spark.sessions}
            // Always present, so the tile matches the height of its
            // neighbours: the run target when declared, else the population
            // the sessions are drawn from, else why there is nothing yet.
            detail={sessionsDetail(latest)}
          />
          <StatTile
            label="attach rate"
            value={(latest?.rates.attachPerSec ?? 0).toFixed(1)}
            unit="/s"
            history={spark.attach}
            detail={`success ${pct(attachOk, 1)}%`}
            tone={attachOk < 0.95 ? "warn" : "neutral"}
          />
          <StatTile
            label="downlink"
            value={dl.value}
            unit={dl.unit}
            history={spark.downlink}
            detail={`uplink ${(() => { const u = bps(latest?.throughput.uplinkBps ?? 0); return `${u.value} ${u.unit}`; })()}`}
          />
          <StatTile
            label="cp latency p99"
            value={ms(latest?.cpLatency.p99 ?? 0)}
            unit="ms"
            history={spark.p99}
            detail={`p50 ${ms(latest?.cpLatency.p50 ?? 0)} · p90 ${ms(latest?.cpLatency.p90 ?? 0)}`}
            tone={(latest?.cpLatency.p99 ?? 0) > 150 ? "warn" : "neutral"}
          />
          {/*
            The value is ATTACH failures and the detail is MOBILITY — two
            different failure domains, so both are named. The detail reports
            counts rather than a success ratio: "100% ok" over zero handovers
            reads as a healthy statistic about something that never happened.
          */}
          <StatTile
            label="attach failures"
            value={count(latest?.ues.failed ?? 0)}
            tone={(latest?.ues.failed ?? 0) > 0 ? "error" : "ok"}
            detail={handoverDetail(latest)}
          />
        </section>

        {/* Controls: which run, and over what span. */}
        <div className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2">
          {!mock && <RunPicker runs={runs} selected={selectedRun} onSelect={selectRun} />}
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
        </div>

        {/* Time series */}
        <section className="grid min-h-0 shrink-0 grid-cols-1 gap-3 xl:grid-cols-2">
          <TimeSeriesPanel
            title="UE population by state"
            meta="stacked"
            telemetry={telemetry}
            series={ueSeries}
            stack
            format={count}
            windowMs={windowMs}
            className="h-[260px]"
          />
          <TimeSeriesPanel
            title="N3 throughput"
            telemetry={telemetry}
            series={throughputSeries}
            format={(v) => { const b = bps(v); return `${b.value} ${b.unit}`; }}
            windowMs={windowMs}
            className="h-[260px]"
          />
          <TimeSeriesPanel
            title="Procedure rate"
            meta="per second"
            telemetry={telemetry}
            series={rateSeries}
            format={(v) => v.toFixed(1)}
            windowMs={windowMs}
            className="h-[220px]"
          />
          <TimeSeriesPanel
            title="Control-plane latency"
            meta="ms"
            telemetry={telemetry}
            series={latencySeries}
            format={ms}
            windowMs={windowMs}
            className="h-[220px]"
          />
        </section>

        {/*
          Application quality, one fixed slot per app kind. Always rendered and
          always in this order: showing only the cohorts a run happens to have
          moved the panels around between runs, so their position carried no
          meaning. A run with several cohorts of one app widens that slot
          rather than reordering the rest.
        */}
        <section className="grid min-h-0 grid-cols-1 gap-3 lg:grid-cols-2 xl:grid-cols-3">
          {COHORT_APPS.map((app) => {
            const of = latest?.cohorts.filter((c) => c.app === app) ?? [];
            if (of.length === 0) return <CohortSlot key={app} app={app} cohort={null} />;
            return of.map((c) => <CohortSlot key={c.name} app={app} cohort={c} />);
          })}
        </section>

        {/* Flows + distribution + events */}
        <section className="grid min-h-0 flex-1 grid-cols-1 gap-3 xl:grid-cols-[320px_1fr_1fr]">
          <GnbDistribution frame={latest} className="min-h-[180px]" />
          <FlowTable
            flows={latest?.flows ?? []}
            total={latest?.flowsTotal ?? 0}
            className="min-h-[180px]"
          />
          <EventStream events={events} onClear={clearEvents} className="min-h-[180px]" />
        </section>
      </main>
    </div>
  );
}
