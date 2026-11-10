/**
 * PerformanceDashboard — the load/fleet view: headline tiles, the four time
 * series, the per-app cohort slots, and the flows/distribution/events row.
 *
 * Factored out of App (ADR-0010) so a run's kind, not a stack of conditionals,
 * picks the view, and so the same component serves a live run and an archived
 * one — only TimeSeriesPanel's time-axis auto-scroll is live-only. All of its
 * inputs come from the shared telemetry stream; it owns the derived series and
 * sparkline state so the shell need not.
 */
import { useMemo } from "react";
import { StatTile } from "@/components/StatTile";
import { EventStream } from "@/panels/EventStream";
import { CohortSlot } from "@/panels/CohortCard";
import { FlowTable } from "@/panels/FlowTable";
import { GnbDistribution } from "@/panels/GnbDistribution";
import { TimeSeriesPanel, type SeriesDef } from "@/panels/TimeSeriesPanel";
import { attachRateTile, attachRateLabel } from "@/app/attachRateTile";
import { cpLatencyTile } from "@/app/cpLatencyTile";
import { tokens } from "@/theme/tokens";
import { bps, count, ms, pct } from "@/lib/format";
import type { Telemetry } from "@/hooks/useTelemetry";
import type { TelemetryFrame, TestEvent } from "@/data/types";

/** Fixed slot order for the cohort panels — voice, web, then video. */
const COHORT_APPS = ["voip", "http", "video"] as const;

export interface PerformanceDashboardProps {
  telemetry: Telemetry;
  windowMs: number;
  /** The pinned run, if any; undefined follows the latest. */
  selectedRun: string | undefined;
  /** Events with conformance verdicts already filtered out. */
  logEvents: TestEvent[];
  clearEvents: () => void;
}

export function PerformanceDashboard({
  telemetry,
  windowMs,
  selectedRun,
  logEvents,
  clearEvents,
}: PerformanceDashboardProps) {
  const { latest, history } = telemetry;
  const t = tokens();

  // Sessions in context: a bare count says nothing about whether it is all of
  // them. Prefer the run's declared target; otherwise the funnel total, which
  // is every UE seen including the ones that failed.
  const sessionsDetail = (f: TelemetryFrame | null): string => {
    if (f?.run.targetUes) return `target ${count(f.run.targetUes)}`;
    const u = f?.ues;
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
  // run reads as all failures. At the bottom the red band is the height of the
  // failure count — invisible when nothing has failed, the honest picture.
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

  // Sparkline history. Recomputed per publish, not per sample. history is a
  // ring buffer appended to in place; latest, which each publish replaces, is
  // what says it changed.
  const spark = useMemo(() => {
    void latest;
    const recent = history.toArray().slice(-60);
    return {
      sessions: recent.map((f) => f.ues.sessionActive),
      attach: recent.map((f) => f.rates.attachPerSec),
      downlink: recent.map((f) => f.throughput.downlinkBps),
      p99: recent.map((f) => f.cpLatency.p99),
    };
  }, [history, latest]);

  // Cohort cards use the same window as the large charts. Bound SVG complexity
  // by sampling at most 180 frames; these are shape summaries, while the
  // adjacent values retain the exact latest percentiles.
  const cohortHistory = useMemo(() => {
    // In follow mode a completed run is no longer live, so do not leave its
    // cohort trends looking active indefinitely. An explicitly selected run is
    // historical analysis and keeps its archived charts.
    if (!selectedRun && latest && ["complete", "failed"].includes(latest.run.state)) return [];
    const cutoff = (latest?.t ?? Date.now()) - windowMs;
    const frames = history.toArray().filter((f) => f.t >= cutoff);
    const stride = Math.max(1, Math.ceil(frames.length / 180));
    return frames.filter((_, i) => i % stride === 0 || i === frames.length - 1);
  }, [history, latest, selectedRun, windowMs]);

  // Peak and average over the WHOLE run, not the visible window: the attach
  // burst is over in seconds, and a shorter span would scroll it out of view.
  const atTile = useMemo(() => {
    void latest;
    return attachRateTile(history.toArray(), (v) => pct(v, 1));
  }, [history, latest]);

  const cpTile = useMemo(() => {
    void latest;
    return cpLatencyTile(history.toArray().slice(-60), ms);
  }, [history, latest]);

  const dl = bps(latest?.throughput.downlinkBps ?? 0);
  const attachOk = latest ? latest.rates.attachSuccess : 1;
  const flowsActive =
    latest?.run.state === "starting" || latest?.run.state === "running" || latest?.run.state === "draining";

  return (
    <>
      {/* Headline numbers. */}
      <section className="grid shrink-0 grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
        <StatTile
          label="sessions active"
          value={count(latest?.ues.sessionActive ?? 0)}
          tone="accent"
          history={spark.sessions}
          detail={sessionsDetail(latest)}
        />
        <StatTile
          label={attachRateLabel(atTile)}
          value={atTile.value}
          unit="/s"
          history={spark.attach}
          detail={atTile.detail}
          tone={attachOk < 0.95 ? "warn" : "neutral"}
        />
        <StatTile
          label="downlink"
          value={dl.value}
          unit={dl.unit}
          history={spark.downlink}
          detail={`uplink ${(() => {
            const u = bps(latest?.throughput.uplinkBps ?? 0);
            return `${u.value} ${u.unit}`;
          })()}`}
        />
        <StatTile
          label={cpTile.label}
          value={ms(latest?.cpLatency.p99 ?? 0)}
          unit="ms"
          history={spark.p99}
          detail={cpTile.detail}
          tone={!cpTile.stale && (latest?.cpLatency.p99 ?? 0) > 150 ? "warn" : "neutral"}
        />
        <StatTile
          label="attach failures"
          value={count(latest?.ues.failed ?? 0)}
          tone={(latest?.ues.failed ?? 0) > 0 ? "error" : "ok"}
          detail={handoverDetail(latest)}
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
          windowMs={windowMs}
          className="h-[260px]"
        />
        <TimeSeriesPanel
          title="N3 throughput"
          telemetry={telemetry}
          series={throughputSeries}
          format={(v) => {
            const b = bps(v);
            return `${b.value} ${b.unit}`;
          }}
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

      {/* Application quality, one fixed slot per app kind, always in order. */}
      <section className="grid min-h-0 shrink-0 grid-cols-1 gap-3 lg:grid-cols-2 xl:grid-cols-3">
        {COHORT_APPS.map((app) => {
          const of = latest?.cohorts.filter((c) => c.app === app) ?? [];
          if (of.length === 0) return <CohortSlot key={app} app={app} cohort={null} history={cohortHistory} />;
          return of.map((c) => <CohortSlot key={c.name} app={app} cohort={c} history={cohortHistory} />);
        })}
      </section>

      {/* Flows + distribution + events. */}
      <section className="grid min-h-0 flex-1 grid-cols-1 gap-3 xl:grid-cols-[320px_1fr_1fr]">
        <GnbDistribution frame={latest} className="min-h-[540px]" />
        <FlowTable
          flows={flowsActive ? (latest?.flows ?? []) : []}
          total={flowsActive ? (latest?.flowsTotal ?? 0) : 0}
          active={flowsActive}
          className="min-h-[540px]"
        />
        <EventStream events={logEvents} onClear={clearEvents} className="min-h-[540px]" />
      </section>
    </>
  );
}
