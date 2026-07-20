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

/** Samples held on screen. Older samples stay in the ring for export. */
const WINDOW = 600;

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
  const buffers = useRef<{ t: number[]; cols: number[][] }>({ t: [], cols: [] });

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
        type: "category",
        boundaryGap: false,
        axisLabel: { formatter: (v: string) => v, hideOverlap: true },
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
    buf.cols = series.map(() => []);
    buf.t = [];

    // Seed from history so a newly mounted panel isn't blank mid-run.
    for (const f of history.toArray().slice(-WINDOW)) {
      buf.t.push(f.t);
      series.forEach((s, i) => (buf.cols[i] as number[]).push(s.value(f)));
    }

    let dirty = buf.t.length > 0;

    const off = subscribeFrames((f) => {
      buf.t.push(f.t);
      series.forEach((s, i) => (buf.cols[i] as number[]).push(s.value(f)));
      if (buf.t.length > WINDOW) {
        buf.t.shift();
        for (const col of buf.cols) col.shift();
      }
      dirty = true;
    });

    // One chart update per frame at most, and none while idle.
    let raf = requestAnimationFrame(function draw() {
      if (dirty) {
        dirty = false;
        chartRef.current?.setOption({
          xAxis: { data: buf.t.map(clock) },
          series: buf.cols.map((data) => ({ data })),
        });
      }
      raf = requestAnimationFrame(draw);
    });

    return () => {
      off();
      cancelAnimationFrame(raf);
    };
  }, [series, history, subscribeFrames]);

  return (
    <Panel title={title} meta={meta} live className={className}>
      <div className="h-full min-h-0">
        <Chart ref={chartRef} option={option} optionDeps={[option]} ariaLabel={title} />
      </div>
    </Panel>
  );
}
