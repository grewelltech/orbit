/**
 * Thin React wrapper over an ECharts instance.
 *
 * Performance contract: React owns mount, unmount, and resize — nothing else.
 * Live telemetry is pushed through the imperative handle, so a 10 kHz sample
 * stream never enters React state and never triggers reconciliation. Panels
 * that re-render on every sample are the main way dashboards like this die.
 */
import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  type CSSProperties,
} from "react";
import type { EChartsOption, ECharts, SetOptionOpts } from "echarts";
import { echarts, ORBIT_THEME, registerOrbitTheme } from "@/theme/echarts";

export interface ChartHandle {
  /** The live ECharts instance, or null before mount / after unmount. */
  instance(): ECharts | null;
  /** Merges an option patch. Defaults to a non-destructive merge. */
  setOption(option: EChartsOption, opts?: SetOptionOpts): void;
  /** Appends samples to a series without re-specifying the whole option. */
  appendData(seriesIndex: number, data: unknown[]): void;
  /** Forces a resize, e.g. after a layout change the observer can't see. */
  resize(): void;
}

export interface ChartProps {
  /** Initial option. Subsequent updates should go through the handle. */
  option: EChartsOption;
  /**
   * Re-applies `option` whenever these values change. Use for structural
   * changes (series added, axis reconfigured) — never for incoming samples.
   */
  optionDeps?: readonly unknown[];
  className?: string;
  style?: CSSProperties;
  /** Accessible description of what the chart shows. */
  ariaLabel?: string;
}

export const Chart = forwardRef<ChartHandle, ChartProps>(function Chart(
  { option, optionDeps = [], className, style, ariaLabel },
  ref,
) {
  const hostRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<ECharts | null>(null);

  // Layout effect so the instance exists before the browser paints, avoiding
  // a visible empty frame on mount.
  useLayoutEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    registerOrbitTheme();
    const chart = echarts.init(host, ORBIT_THEME, {
      renderer: "canvas",
      // Charts are laid out by CSS; ECharts measures the host on init.
      width: "auto",
      height: "auto",
    });
    chartRef.current = chart;
    chart.setOption(option);

    // Resize is coalesced into an animation frame: a grid of charts otherwise
    // triggers one synchronous relayout per chart per observer callback.
    let frame = 0;
    const observer = new ResizeObserver(() => {
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        chart.resize();
      });
    });
    observer.observe(host);

    return () => {
      observer.disconnect();
      if (frame) cancelAnimationFrame(frame);
      chart.dispose();
      chartRef.current = null;
    };
    // Instance lifecycle is mount-scoped; `option` is applied via the effect below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Structural option changes only — see ChartProps.optionDeps.
  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    chart.setOption(option, { notMerge: false, lazyUpdate: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, optionDeps);

  useImperativeHandle(
    ref,
    (): ChartHandle => ({
      instance: () => chartRef.current,
      setOption: (opt, opts) =>
        chartRef.current?.setOption(opt, opts ?? { notMerge: false, lazyUpdate: true }),
      appendData: (seriesIndex, data) =>
        chartRef.current?.appendData({ seriesIndex, data }),
      resize: () => chartRef.current?.resize(),
    }),
    [],
  );

  return (
    <div
      ref={hostRef}
      className={className}
      style={{ width: "100%", height: "100%", ...style }}
      role="img"
      aria-label={ariaLabel ?? "chart"}
    />
  );
});
