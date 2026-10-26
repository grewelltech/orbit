/**
 * CohortCard — one app cohort's live quality.
 *
 * Shows only the families the cohort's app produces. The server omits the
 * rest, and rendering an absent distribution as 0 would present "not
 * measured" as "measured perfect" — the same reason the origin's TTFB is
 * suppressed rather than printed as 0.00ms.
 */
import { useMemo, useState } from "react";
import type { EChartsOption } from "echarts";
import { Chart } from "@/components/Chart";
import { Panel } from "@/components/Panel";
import type { CohortQuality, Quantiles, TelemetryFrame } from "@/data/types";
import { bps, clock } from "@/lib/format";
import { tokens } from "@/theme/tokens";

type QuantileMetric = "mos" | "ttfbMs" | "goodputMbps" | "bitrateKbps" | "stallTimeMs" | "rebufferRatio" | "startupMs";

interface MetricDef {
  key: QuantileMetric;
  label: string;
  unit?: string;
  digits: number;
}

const METRICS: MetricDef[] = [
  { key: "mos", label: "MOS", digits: 2 },
  { key: "ttfbMs", label: "TTFB", unit: "ms", digits: 1 },
  { key: "goodputMbps", label: "goodput", unit: "Mbps", digits: 2 },
  { key: "bitrateKbps", label: "bitrate", unit: "kbps", digits: 0 },
  { key: "stallTimeMs", label: "stall", unit: "ms", digits: 0 },
  { key: "rebufferRatio", label: "rebuffer", digits: 3 },
  { key: "startupMs", label: "startup", unit: "ms", digits: 0 },
];

const MOS_QUALITY = [
  { label: "excellent", range: "4.30–4.50", min: 4.3, max: 4.5, token: "ok", css: "var(--o-ok)" },
  { label: "good", range: "4.00–4.29", min: 4, max: 4.3, token: "accent", css: "var(--o-accent)" },
  { label: "fair", range: "3.60–3.99", min: 3.6, max: 4, token: "amber", css: "var(--o-amber)" },
  { label: "poor", range: "3.10–3.59", min: 3.1, max: 3.6, token: "warn", css: "var(--o-warn)" },
  { label: "bad", range: "1.00–3.09", min: 1, max: 3.1, token: "error", css: "var(--o-error)" },
] as const;

function Row({
  label,
  q,
  unit,
  digits = 2,
}: {
  label: string;
  q: Quantiles | null;
  unit?: string;
  digits?: number;
}) {
  if (!q) return null;
  return (
    <div className="flex items-baseline justify-between gap-3 px-3 py-1">
      <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
        {label}
      </span>
      <span className="whitespace-nowrap font-mono text-xs">
        {q.p5.toFixed(digits)} / <strong>{q.p50.toFixed(digits)}</strong> / {q.p95.toFixed(digits)}
        {unit ? ` ${unit}` : ""}
      </span>
    </div>
  );
}

/**
 * A slot for one app kind. Rendered whether or not the run has such a cohort,
 * so the three cards keep fixed positions: a panel that appears and disappears
 * with the traffic mix moves everything below it, and an operator learns where
 * to look rather than re-reading the layout each run.
 */
export function CohortSlot({
  app,
  cohort,
  history,
}: {
  app: string;
  cohort: CohortQuality | null;
  history: readonly TelemetryFrame[];
}) {
  if (!cohort) {
    return (
      <Panel title={app} flush>
        <p className="o-label px-3 py-3" style={{ color: "var(--o-ink-3)" }}>
          no {app} cohort in this run
        </p>
      </Panel>
    );
  }
  return <CohortCard cohort={cohort} history={history} />;
}

export function CohortCard({ cohort: c, history }: { cohort: CohortQuality; history: readonly TelemetryFrame[] }) {
  const available = METRICS.filter((metric) => c[metric.key] != null);
  const [selected, setSelected] = useState<QuantileMetric>(() => available[0]?.key ?? "mos");
  const metric = available.find((candidate) => candidate.key === selected) ?? available[0];
  const t = tokens();
  const option = useMemo<EChartsOption>(() => {
    if (!metric) return {};
    const points = { p5: [] as [number, number][], p50: [] as [number, number][], p95: [] as [number, number][] };
    for (const frame of history) {
      const q = frame.cohorts.find((candidate) => candidate.name === c.name)?.[metric.key];
      if (!q) continue;
      points.p5.push([frame.t, q.p5]);
      points.p50.push([frame.t, q.p50]);
      points.p95.push([frame.t, q.p95]);
    }
    const format = (value: number) => `${value.toFixed(metric.digits)}${metric.unit ? ` ${metric.unit}` : ""}`;
    const axisDigits = metric.key === "mos" ? 3 : metric.digits;
    return {
      animation: false,
      grid: { top: 28, right: 10, bottom: 22, left: 48 },
      tooltip: { trigger: "axis", valueFormatter: (value) => format(Number(value)) },
      legend: { top: 0, right: 0, itemGap: 10, padding: 0 },
      xAxis: { type: "time", axisLabel: { formatter: (value: number) => clock(value), hideOverlap: true } },
      yAxis: {
        type: "value",
        min: metric.key === "mos" ? 1 : 0,
        ...(metric.key === "mos" ? { max: 4.5 } : {}),
        ...(metric.key === "rebufferRatio" ? { max: 1 } : {}),
        axisLabel: { formatter: (value: number) => value.toFixed(axisDigits) },
      },
      series: [
        {
          type: "line",
          name: "p5",
          showSymbol: false,
          data: points.p5,
          lineStyle: { width: 1, color: t.series[2] },
          itemStyle: { color: t.series[2] },
          ...(metric.key === "mos"
            ? {
                markArea: {
                  silent: true,
                  label: { show: false },
                  data: MOS_QUALITY.map((band) => [
                    { yAxis: band.min, itemStyle: { color: t[band.token], opacity: 0.055 } },
                    { yAxis: band.max },
                  ]),
                },
              }
            : {}),
        },
        { type: "line", name: "p50", showSymbol: false, data: points.p50, lineStyle: { width: 2, color: t.series[0] }, itemStyle: { color: t.series[0] } },
        { type: "line", name: "p95", showSymbol: false, data: points.p95, lineStyle: { width: 1, color: t.series[4] }, itemStyle: { color: t.series[4] } },
      ],
    };
  }, [c.name, history, metric, t.series]);

  return (
    <Panel
      title={`${c.name} · ${c.app}`}
      live
      flush
      meta={
        <span
          className="o-label"
          style={{ color: c.failed > 0 ? "var(--o-error)" : "var(--o-ink-3)" }}
        >
          {c.ues} UEs
          {c.failed > 0 ? ` · ${c.failed} failed` : ""} · {Math.round(c.elapsedMs / 1000)}s
        </span>
      }
    >
      <div className="grid min-h-[190px] grid-cols-1 sm:grid-cols-[minmax(0,1fr)_240px]">
        <div className="flex min-h-[190px] flex-col border-b p-2 sm:border-r sm:border-b-0" style={{ borderColor: "var(--o-border)" }}>
          <div className="flex min-h-5 flex-wrap gap-1">
            {available.map((candidate) => (
              <button
                key={candidate.key}
                type="button"
                onClick={() => setSelected(candidate.key)}
                className="o-label cursor-pointer border px-1.5 py-0.5"
                style={{
                  color: metric?.key === candidate.key ? "var(--o-accent)" : "var(--o-ink-3)",
                  borderColor: metric?.key === candidate.key ? "var(--o-border-accent)" : "transparent",
                  borderRadius: "var(--o-radius)",
                }}
                aria-pressed={metric?.key === candidate.key}
              >
                {candidate.label}
              </button>
            ))}
          </div>
          <div className="min-h-0 flex-1">
            {metric ? (
              <Chart option={option} optionDeps={[option]} ariaLabel={`${c.name} ${metric.label} history`} />
            ) : (
              <p className="o-label p-3" style={{ color: "var(--o-ink-3)" }}>awaiting quality samples</p>
            )}
          </div>
        </div>
        <div className="py-1">
          <p className="o-label px-3 pb-1" style={{ color: "var(--o-ink-3)" }}>
            latest p5 / p50 / p95
          </p>
          <Row label="MOS" q={c.mos} />
          <Row label="TTFB" q={c.ttfbMs} unit="ms" digits={1} />
          <Row label="goodput" q={c.goodputMbps} unit="Mbps" />
          <Row label="bitrate" q={c.bitrateKbps} unit="kbps" digits={0} />
          <Row label="stall" q={c.stallTimeMs} unit="ms" digits={0} />
          <Row label="rebuffer" q={c.rebufferRatio} digits={3} />
          <Row label="startup" q={c.startupMs} unit="ms" digits={0} />
          {c.app === "voip" && (
            <div className="mt-1 border-t px-3 pt-2" style={{ borderColor: "var(--o-border)" }}>
              <p className="o-label mb-1" style={{ color: "var(--o-ink-3)" }}>MOS quality</p>
              {MOS_QUALITY.map((band) => (
                <div key={band.label} className="flex items-center justify-between gap-2 font-mono text-[10px] leading-4">
                  <span className="flex items-center gap-1.5">
                    <span className="inline-block h-1.5 w-1.5" style={{ backgroundColor: band.css }} />
                    {band.label}
                  </span>
                  <span style={{ color: "var(--o-ink-3)" }}>{band.range}</span>
                </div>
              ))}
            </div>
          )}
          {c.farEnd && c.app !== "voip" && (
          <div
            className="mt-1 px-3 pt-1"
            style={{ borderTop: "1px solid var(--o-border)" }}
          >
            {c.farEnd.available ? (
              <div className="flex items-baseline justify-between gap-3">
                <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
                  N6 far end
                </span>
                <span className="font-mono text-xs">
                  {(() => {
                    const r = bps(c.farEnd.bitsPerSec);
                    return `${r.value} ${r.unit}`;
                  })()}
                  {c.farEnd.requests > 0 ? ` · ${c.farEnd.requests} reqs` : ""}
                  {c.farEnd.errors > 0 ? ` · ${c.farEnd.errors} err` : ""}
                </span>
              </div>
            ) : (
              // The reason, not a blank: an unwatched far end must not read as
              // a far end that received nothing.
              <p className="o-label" style={{ color: "var(--o-ink-3)" }}>
                N6 far end — {c.farEnd.reason || "not collected"}
              </p>
            )}
          </div>
          )}
        </div>
      </div>
    </Panel>
  );
}
