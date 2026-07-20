/**
 * Sparkline — trend shape for a stat tile.
 *
 * Plain SVG rather than a chart instance: a dashboard carries dozens of these,
 * and an ECharts instance each would cost far more than the shape is worth.
 * No axes, no labels — it answers "which way is this going?" and nothing else.
 */
import { useMemo } from "react";

export interface SparklineProps {
  values: readonly number[];
  stroke: string;
  width?: number;
  height?: number;
  className?: string;
}

export function Sparkline({ values, stroke, width = 72, height = 22, className }: SparklineProps) {
  const path = useMemo(() => {
    if (values.length < 2) return "";
    let min = Infinity;
    let max = -Infinity;
    for (const v of values) {
      if (v < min) min = v;
      if (v > max) max = v;
    }
    // A flat series draws through the middle instead of dividing by zero.
    const span = max - min || 1;
    const pad = 1.5; // keeps the 2px stroke from clipping at the edges
    const stepX = (width - pad * 2) / (values.length - 1);
    const scaleY = (height - pad * 2) / span;

    let d = "";
    for (let i = 0; i < values.length; i++) {
      const x = pad + i * stepX;
      const y = height - pad - ((values[i] as number) - min) * scaleY;
      d += `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`;
    }
    return d;
  }, [values, width, height]);

  if (!path) return null;

  return (
    <svg
      className={className}
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      fill="none"
      aria-hidden
      focusable="false"
    >
      <path d={path} stroke={stroke} strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round" opacity={0.85} />
    </svg>
  );
}
