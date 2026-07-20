/** Display formatting. Kept in one place so units read identically everywhere. */

const UNITS_BPS = ["bps", "kbps", "Mbps", "Gbps", "Tbps"] as const;

/** Bits per second in SI steps, e.g. 1_500_000 → "1.50 Mbps". */
export function bps(v: number): { value: string; unit: string } {
  if (!Number.isFinite(v) || v <= 0) return { value: "0", unit: "bps" };
  const i = Math.min(UNITS_BPS.length - 1, Math.floor(Math.log10(v) / 3));
  const scaled = v / 1000 ** i;
  return {
    value: scaled.toFixed(scaled >= 100 ? 0 : scaled >= 10 ? 1 : 2),
    unit: UNITS_BPS[i] as string,
  };
}

/** Compact counts: 1234 → "1.2k". */
export function count(v: number): string {
  if (!Number.isFinite(v)) return "—";
  if (Math.abs(v) < 1000) return String(Math.round(v));
  if (Math.abs(v) < 1_000_000) return `${(v / 1000).toFixed(v < 10_000 ? 1 : 0)}k`;
  return `${(v / 1_000_000).toFixed(1)}M`;
}

/** Milliseconds with a precision that suits the magnitude. */
export function ms(v: number): string {
  if (!Number.isFinite(v)) return "—";
  if (v < 10) return v.toFixed(2);
  if (v < 100) return v.toFixed(1);
  return v.toFixed(0);
}

/** Fraction 0–1 as a percentage, e.g. 0.9824 → "98.24". */
export function pct(v: number, digits = 2): string {
  if (!Number.isFinite(v)) return "—";
  return (v * 100).toFixed(digits);
}

/** Elapsed milliseconds as HH:MM:SS. */
export function duration(msTotal: number): string {
  if (!Number.isFinite(msTotal) || msTotal < 0) return "00:00:00";
  const s = Math.floor(msTotal / 1000);
  const hh = String(Math.floor(s / 3600)).padStart(2, "0");
  const mm = String(Math.floor((s % 3600) / 60)).padStart(2, "0");
  const ss = String(s % 60).padStart(2, "0");
  return `${hh}:${mm}:${ss}`;
}

/** Wall-clock time of day, to the second. */
export function clock(t: number): string {
  const d = new Date(t);
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}
