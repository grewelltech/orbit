/**
 * GnbDistribution — where the UE population currently sits.
 *
 * A horizontal bar rather than a pie: the reader's question is "is the load
 * balanced?", which is a length comparison. Values are labelled directly, so
 * the chart is readable without cross-referencing a legend.
 */
import { useMemo, useRef } from "react";
import type { EChartsOption } from "echarts";
import { Chart, type ChartHandle } from "@/components/Chart";
import { Panel } from "@/components/Panel";
import { seriesColor, tokens } from "@/theme/tokens";
import type { TelemetryFrame } from "@/data/types";

export interface GnbDistributionProps {
  frame: TelemetryFrame | null;
  className?: string;
}

// GNB_ROW_HEIGHT is the per-cell pitch: the 12px bar plus breathing room.
const GNB_ROW_HEIGHT = 22;

export function GnbDistribution({ frame, className }: GnbDistributionProps) {
  const chartRef = useRef<ChartHandle>(null);
  const entries = useMemo(() => Object.entries(frame?.perGnb ?? {}).sort(([a], [b]) => a.localeCompare(b)), [frame?.perGnb]);

  const option = useMemo<EChartsOption>(() => {
    const total = entries.reduce((s, [, v]) => s + v, 0) || 1;
    return {
      animation: false,
      // Fixed padding, not proportional: the grid's height is set by the
      // wrapper below to exactly fit the rows, so the bars keep a constant
      // pitch instead of being spread to fill whatever box they are given.
      grid: { top: 4, right: 52, bottom: 4, left: 62 },
      xAxis: { type: "value", show: false, max: Math.max(1, ...entries.map(([, v]) => v)) * 1.15 },
      yAxis: {
        type: "category",
        inverse: true,
        data: entries.map(([k]) => k),
        axisLine: { show: false },
        splitLine: { show: false },
      },
      series: [
        {
          type: "bar",
          barWidth: 12,
          data: entries.map(([, v], i) => ({ value: v, itemStyle: { color: seriesColor(i) } })),
          itemStyle: { borderRadius: [0, 2, 2, 0], borderWidth: 0 },
          label: {
            show: true,
            position: "right",
            formatter: (p) => {
              const v = Number(p.value ?? 0);
              return `${v}  ${((v / total) * 100).toFixed(0)}%`;
            },
            // Canvas can't read CSS variables; tokens are resolved here.
            color: tokens().ink2,
            fontFamily: tokens().fontMono,
            fontSize: 11,
          },
        },
      ],
    };
  }, [entries]);

  return (
    <Panel
      title="UE distribution by gNB"
      live
      className={className}
      meta={`${entries.length} cells`}
    >
      {/*
        ECharts spreads category rows evenly over the grid height, so one gNB
        sits centred and four drift apart — the spacing reads as meaningful
        when it is only an artefact of the box. Sizing the chart to the row
        count instead keeps the pitch constant and anchors the list to the top,
        and the outer box scrolls once a grid outgrows it.
      */}
      <div className="h-full min-h-0 overflow-y-auto">
        <div style={{ height: Math.max(1, entries.length) * GNB_ROW_HEIGHT + 8 }}>
          <Chart ref={chartRef} option={option} optionDeps={[option]} ariaLabel="UE distribution by gNB" />
        </div>
      </div>
    </Panel>
  );
}
