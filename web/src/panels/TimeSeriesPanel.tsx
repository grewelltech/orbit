/**
 * TimeSeriesPanel — the workhorse chart panel.
 *
 * Series are declared as accessors over TelemetryFrame, so a panel is a
 * declaration rather than a chart implementation. Updates are pushed straight
 * to the ECharts instance on an animation frame; the component itself never
 * re-renders in response to data.
 */
import { useEffect, useMemo, useRef } from "react";
import type { EChartsOption } from "echarts";
import { Chart, type ChartHandle } from "@/components/Chart";
import { Panel } from "@/components/Panel";
import type { Telemetry } from "@/hooks/useTelemetry";
import type { TelemetryFrame } from "@/data/types";
import { clock } from "@/lib/format";

/**
 * Selectable spans. The panels share one setting so they can be read against
 * each other — two charts silently showing different spans is worse than
 * either span being wrong.
 */
export const TIME_WINDOWS = [
  { label: "1m", ms: 60_000 },
  { label: "5m", ms: 300_000 },
  { label: "10m", ms: 600_000 },
  { label: "30m", ms: 1_800_000 },
] as const;

export const DEFAULT_WINDOW_MS = 600_000;

/**
 * Hard cap on buffered points, independent of the window. At one frame per
 * second the 30m span is 1800 points; this only bites if frames arrive faster
 * than that, and keeps a fast stream from growing the buffer without bound.
 */
const MAX_POINTS = 4000;

export interface SeriesDef {
  name: string;
  color: string;
  value: (f: TelemetryFrame) => number;
  /** Fills beneath the line. Use for volumes, not for rates. */
  area?: boolean;
}

export interface TimeSeriesPanelProps {
  title: string;
  meta?: React.ReactNode;
  telemetry: Telemetry;
  series: SeriesDef[];
  /** Formats y-axis ticks and tooltip values. */
  format: (v: number) => string;
  /** Stacks series into a cumulative band. */
  stack?: boolean;
  /** Pins the y-axis maximum, e.g. 1 for a ratio. */
  max?: number;
  /** Span shown on the x-axis, in milliseconds. */
  windowMs?: number;
  className?: string;
}

export function TimeSeriesPanel({
  title,
  meta,
  telemetry,
  series,
  format,
  stack = false,
  max,
  windowMs = DEFAULT_WINDOW_MS,
  className,
}: TimeSeriesPanelProps) {
  const chartRef = useRef<ChartHandle>(null);
  const { history, subscribeFrames } = telemetry;

  // Held in a ref so an inline `format` from the caller can't destabilise the
  // option memo. An unstable option re-fires setOption, which resets every
  // series' data to the empty array declared below — the panel silently blanks.
  const formatRef = useRef(format);
  formatRef.current = format;

  // Column-oriented buffers: one array per series, plus shared timestamps.
  // Reused across updates so the steady state allocates only the sliced views
  // ECharts needs.
  // Points are [timestamp, value] pairs so the x-axis can be a real time
  // axis: a category axis spaces samples evenly regardless of when they
  // happened, so the visual density tracked how many samples existed rather
  // than elapsed time — and a gap in the stream drew as continuous.
  const buffers = useRef<{ pts: [number, number][][] }>({ pts: [] });

  const option = useMemo<EChartsOption>(
    () => ({
      animation: false, // live data: animating between samples reads as lag
      // Top gap clears the legend; left gap fits formatted y-axis labels.
      grid: { top: 30, right: 14, bottom: 22, left: 58 },
      tooltip: {
        trigger: "axis",
        axisPointer: { type: "line" },
        valueFormatter: (v) => formatRef.current(Number(v)),
      },
      legend:
        series.length > 1
          ? {
              show: true,
              top: 0,
              left: 0,
              itemGap: 14,
              padding: 0,
              // Scroll rather than clip when the series names outrun the width.
              type: "scroll",
            }
          : { show: false },
      xAxis: {
        type: "time",
        // The extent is pinned to the chosen window on every draw, so points
        // scroll off the left edge instead of the whole series compressing to
        // fit as a run gets longer.
        axisLabel: { formatter: (v: number) => clock(v), hideOverlap: true },
      },
      yAxis: {
        type: "value",
        ...(max != null ? { max } : {}),
        axisLabel: { formatter: (v: number) => formatRef.current(v) },
      },
      series: series.map((s) => ({
        type: "line" as const,
        name: s.name,
        showSymbol: false,
        lineStyle: { width: 2, color: s.color },
        itemStyle: { color: s.color },
        ...(stack ? { stack: "total" } : {}),
        ...(s.area
          ? {
              areaStyle: {
                // A soft vertical fade keeps stacked bands readable without
                // burying the lines above them.
                color: {
                  type: "linear" as const,
                  x: 0,
                  y: 0,
                  x2: 0,
                  y2: 1,
                  colorStops: [
                    { offset: 0, color: `${s.color}59` },
                    { offset: 1, color: `${s.color}0a` },
                  ],
                },
              },
            }
          : {}),
        data: [] as number[],
      })),
    }),
    // Structural only — see Chart.optionDeps. `format` is deliberately absent;
    // it is read through formatRef so callers may pass an inline function.
    [series, stack, max],
  );

  useEffect(() => {
    const buf = buffers.current;
    buf.pts = series.map(() => []);

    // Prune by TIME, not by count: the window is a span, and a stream that
    // slows down must not start showing a longer history than was asked for.
    const prune = (now: number) => {
      const cutoff = now - windowMs;
      for (const col of buf.pts) {
        let drop = 0;
        while (drop < col.length && (col[drop] as [number, number])[0] < cutoff) drop++;
        if (drop > 0) col.splice(0, drop);
        if (col.length > MAX_POINTS) col.splice(0, col.length - MAX_POINTS);
      }
    };

    // Seed from history so a newly mounted panel isn't blank mid-run.
    for (const f of history.toArray()) {
      series.forEach((s, i) => (buf.pts[i] as [number, number][]).push([f.t, s.value(f)]));
    }
    prune(Date.now());

    let dirty = buf.pts.some((c) => c.length > 0);

    const off = subscribeFrames((f) => {
      series.forEach((s, i) => (buf.pts[i] as [number, number][]).push([f.t, s.value(f)]));
      prune(f.t);
      dirty = true;
    });

    // One chart update per frame at most. The extent is rewritten every draw
    // even when no sample arrived, so an idle chart keeps scrolling rather
    // than freezing with a stale right edge.
    let last = 0;
    let raf = requestAnimationFrame(function draw() {
      const now = Date.now();
      if (dirty || now - last >= 1000) {
        dirty = false;
        last = now;
        prune(now);
        chartRef.current?.setOption({
          xAxis: { min: now - windowMs, max: now },
          series: buf.pts.map((data) => ({ data })),
        });
      }
      raf = requestAnimationFrame(draw);
    });

    return () => {
      off();
      cancelAnimationFrame(raf);
    };
  }, [series, history, subscribeFrames, windowMs]);

  return (
    <Panel title={title} meta={meta} live className={className}>
      <div className="h-full min-h-0">
        <Chart ref={chartRef} option={option} optionDeps={[option]} ariaLabel={title} />
      </div>
    </Panel>
  );
}
