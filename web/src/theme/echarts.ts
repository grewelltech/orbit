/**
 * ECharts setup: tree-shaken module registration plus the ORBIT theme.
 *
 * Importing from `echarts/core` (rather than the `echarts` barrel) keeps the
 * bundle to the chart types actually used. Adding a new chart type means
 * registering it here.
 */
import * as echarts from "echarts/core";
import { BarChart, LineChart, ScatterChart } from "echarts/charts";
import {
  DataZoomComponent,
  GridComponent,
  LegendComponent,
  MarkAreaComponent,
  MarkLineComponent,
  TitleComponent,
  TooltipComponent,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { tokens } from "./tokens";

// Only what panels actually render. Heatmap, gauge, pie, and graph are
// deliberately absent — register them here when a panel needs one, rather than
// carrying them speculatively.
echarts.use([
  LineChart,
  BarChart,
  ScatterChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  TitleComponent,
  DataZoomComponent,
  MarkLineComponent,
  MarkAreaComponent,
  CanvasRenderer,
]);

export const ORBIT_THEME = "orbit";

let registered = false;

/**
 * Registers the ORBIT theme with ECharts. Idempotent, and safe to call before
 * the first chart mounts; requires the stylesheet to be applied so the tokens
 * resolve.
 */
export function registerOrbitTheme(): void {
  if (registered) return;
  const t = tokens();

  const axis = {
    axisLine: { show: true, lineStyle: { color: t.axisLine, width: 1 } },
    axisTick: { show: false },
    axisLabel: {
      color: t.ink3,
      fontFamily: t.fontMono,
      fontSize: 11,
      // Tabular figures keep tick labels from jittering as live data rescales.
      fontFeatureSettings: '"tnum" 1',
    },
    splitLine: { show: true, lineStyle: { color: t.gridLine, width: 1, type: "solid" } },
    // Recessive grid: the data is the loudest thing in the frame.
    splitArea: { show: false },
  };

  echarts.registerTheme(ORBIT_THEME, {
    color: t.series,
    backgroundColor: "transparent",
    textStyle: { fontFamily: t.fontSans, color: t.ink2 },

    title: {
      textStyle: { color: t.ink, fontFamily: t.fontMono, fontSize: 12, fontWeight: 500 },
      subtextStyle: { color: t.ink3, fontFamily: t.fontMono, fontSize: 11 },
    },

    grid: { top: 16, right: 16, bottom: 24, left: 48, containLabel: false },

    categoryAxis: { ...axis, splitLine: { show: false } },
    valueAxis: axis,
    timeAxis: axis,
    logAxis: axis,

    legend: {
      textStyle: { color: t.ink2, fontFamily: t.fontMono, fontSize: 11 },
      icon: "roundRect",
      itemWidth: 8,
      itemHeight: 8,
      itemGap: 14,
      inactiveColor: t.ink3,
    },

    tooltip: {
      backgroundColor: t.tooltipBg,
      borderColor: t.border,
      borderWidth: 1,
      padding: [8, 10],
      textStyle: { color: t.ink, fontFamily: t.fontMono, fontSize: 12 },
      extraCssText: "backdrop-filter: blur(6px); border-radius: 2px;",
      axisPointer: {
        type: "line",
        lineStyle: { color: t.crosshair, width: 1, type: "solid" },
        crossStyle: { color: t.crosshair, width: 1 },
        label: {
          backgroundColor: t.surface3,
          color: t.ink,
          fontFamily: t.fontMono,
          fontSize: 11,
          borderWidth: 0,
        },
      },
    },

    // 2px lines and ≥8px markers: thin marks, legible hit targets.
    line: {
      lineStyle: { width: 2 },
      symbol: "circle",
      symbolSize: 8,
      showSymbol: false,
      smooth: false,
    },

    bar: { itemStyle: { borderRadius: [2, 2, 0, 0], borderColor: t.surface, borderWidth: 1 } },

    scatter: { symbolSize: 8 },

    dataZoom: {
      borderColor: t.border,
      backgroundColor: "transparent",
      fillerColor: "rgb(79 214 232 / 0.10)",
      handleStyle: { color: t.accentDim, borderColor: t.accent },
      moveHandleStyle: { color: t.accentDim },
      textStyle: { color: t.ink3, fontFamily: t.fontMono, fontSize: 10 },
      dataBackground: {
        lineStyle: { color: t.axisLine },
        areaStyle: { color: t.surface2 },
      },
      selectedDataBackground: {
        lineStyle: { color: t.accentDim },
        areaStyle: { color: "rgb(79 214 232 / 0.12)" },
      },
    },
  });

  registered = true;
}

export { echarts };
