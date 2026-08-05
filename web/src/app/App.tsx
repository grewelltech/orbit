/**
 * App — the live-watch layout.
 *
 * Reading order top to bottom: identity and health, then the headline numbers,
 * then the time-series that explain them, then the event stream that accounts
 * for individual occurrences. Density is deliberate — an operator watching a
 * run should not have to scroll to see whether it is healthy.
 */
import { useMemo } from "react";
import { Header } from "./Header";
import { StatTile } from "@/components/StatTile";
import { EventStream } from "@/panels/EventStream";
import { CohortCard } from "@/panels/CohortCard";
import { FlowTable } from "@/panels/FlowTable";
import { GnbDistribution } from "@/panels/GnbDistribution";
import { TimeSeriesPanel, type SeriesDef } from "@/panels/TimeSeriesPanel";
import { MockSource } from "@/data/mock";
import { ConnectSource } from "@/data/connect";
import type { TelemetrySource } from "@/data/source";
import { useTelemetry } from "@/hooks/useTelemetry";
import { tokens } from "@/theme/tokens";
import { bps, count, ms, pct } from "@/lib/format";

export function App() {
  // The real RunService by default; ?mock forces the offline demo generator,
  // and ?run=<id> pins a specific run instead of auto-selecting the active one.
  const source = useMemo<TelemetrySource>(() => {
    const params = new URLSearchParams(window.location.search);
    if (params.has("mock")) return new MockSource();
    const runId = params.get("run") ?? undefined;
    return new ConnectSource(runId ? { runId } : {});
  }, []);
  const telemetry = useTelemetry(source);
  const { latest, history, events, state, clearEvents } = telemetry;

  const t = tokens();

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
      <Header frame={latest} sourceState={state} sourceName={source.name} />

      <main className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3">
        {/* Headline numbers */}
        <section className="grid shrink-0 grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
          <StatTile
            label="sessions active"
            value={count(latest?.ues.sessionActive ?? 0)}
            tone="accent"
            history={spark.sessions}
            detail={latest?.run.targetUes ? `target ${count(latest.run.targetUes)}` : undefined}
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
          <StatTile
            label="failures"
            value={count(latest?.ues.failed ?? 0)}
            tone={(latest?.ues.failed ?? 0) > 0 ? "error" : "ok"}
            detail={`handover ok ${pct(latest?.rates.handoverSuccess ?? 1, 1)}%`}
          />
        </section>

        {/* Time series */}
        <section className="grid min-h-0 shrink-0 grid-cols-1 gap-3 xl:grid-cols-2">
          <TimeSeriesPanel
            title="UE population by state"
            meta="stacked"
            telemetry={telemetry}
            series={ueSeries}
            stack
            format={count}
            className="h-[260px]"
          />
          <TimeSeriesPanel
            title="N3 throughput"
            telemetry={telemetry}
            series={throughputSeries}
            format={(v) => { const b = bps(v); return `${b.value} ${b.unit}`; }}
            className="h-[260px]"
          />
          <TimeSeriesPanel
            title="Procedure rate"
            meta="per second"
            telemetry={telemetry}
            series={rateSeries}
            format={(v) => v.toFixed(1)}
            className="h-[220px]"
          />
          <TimeSeriesPanel
            title="Control-plane latency"
            meta="ms"
            telemetry={telemetry}
            series={latencySeries}
            format={ms}
            className="h-[220px]"
          />
        </section>

        {/* Per-cohort application quality, when the run has app cohorts. */}
        {(latest?.cohorts.length ?? 0) > 0 && (
          <section className="grid min-h-0 grid-cols-1 gap-3 lg:grid-cols-2 xl:grid-cols-3">
            {latest?.cohorts.map((c) => (
              <CohortCard key={c.name} cohort={c} />
            ))}
          </section>
        )}

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
