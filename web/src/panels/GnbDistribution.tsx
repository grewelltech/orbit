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

export function GnbDistribution({ frame, className }: GnbDistributionProps) {
  const chartRef = useRef<ChartHandle>(null);
  const entries = useMemo(() => Object.entries(frame?.perGnb ?? {}).sort(([a], [b]) => a.localeCompare(b)), [frame?.perGnb]);

  const option = useMemo<EChartsOption>(() => {
    const total = entries.reduce((s, [, v]) => s + v, 0) || 1;
    return {
      animation: false,
      grid: { top: 6, right: 52, bottom: 6, left: 62 },
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
      <div className="h-full min-h-0">
        <Chart ref={chartRef} option={option} optionDeps={[option]} ariaLabel="UE distribution by gNB" />
      </div>
    </Panel>
  );
}
