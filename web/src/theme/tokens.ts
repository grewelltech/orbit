/**
 * Runtime access to the CSS design tokens.
 *
 * Charts are drawn to a canvas and cannot inherit CSS, so they read the same
 * custom properties the chrome uses. Reading them here — rather than
 * duplicating hexes in TypeScript — is what keeps charts and chrome from
 * drifting when the palette is retuned in tokens.css.
 */

const read = (name: string): string =>
  getComputedStyle(document.documentElement).getPropertyValue(name).trim();

/** Token values resolved once at startup. */
export interface Tokens {
  bg: string;
  surface: string;
  surface2: string;
  surface3: string;
  border: string;
  ink: string;
  ink2: string;
  ink3: string;
  accent: string;
  accentDim: string;
  amber: string;
  ok: string;
  warn: string;
  error: string;
  idle: string;
  /** Categorical series colours, in fixed assignment order. */
  series: string[];
  gridLine: string;
  axisLine: string;
  crosshair: string;
  tooltipBg: string;
  fontMono: string;
  fontSans: string;
}

let cached: Tokens | null = null;

export function tokens(): Tokens {
  if (cached) return cached;
  cached = {
    bg: read("--o-bg"),
    surface: read("--o-surface"),
    surface2: read("--o-surface-2"),
    surface3: read("--o-surface-3"),
    border: read("--o-border"),
    ink: read("--o-ink"),
    ink2: read("--o-ink-2"),
    ink3: read("--o-ink-3"),
    accent: read("--o-accent"),
    accentDim: read("--o-accent-dim"),
    amber: read("--o-amber"),
    ok: read("--o-ok"),
    warn: read("--o-warn"),
    error: read("--o-error"),
    idle: read("--o-idle"),
    series: Array.from({ length: 8 }, (_, i) => read(`--o-series-${i + 1}`)),
    gridLine: read("--o-grid-line"),
    axisLine: read("--o-axis-line"),
    crosshair: read("--o-crosshair"),
    tooltipBg: read("--o-tooltip-bg"),
    fontMono: read("--o-font-mono"),
    fontSans: read("--o-font-sans"),
  };
  return cached;
}

/** Discards the cache so a token change can be picked up without a reload. */
export function invalidateTokens(): void {
  cached = null;
}

/** Series colour for index i. Beyond the palette, callers fold into "other". */
export function seriesColor(i: number): string {
  const s = tokens().series;
  return s[i % s.length] ?? tokens().accent;
}
