/**
 * What the attach-rate tile should say.
 *
 * The instantaneous rate is only meaningful while UEs are still attaching. A
 * fleet run finishes its attach phase in the first seconds and then reads
 * "0.0/s" for the rest of the run, next to a sparkline with no axis — so the
 * one number worth knowing, how fast the core actually took the population, is
 * on screen briefly and then gone.
 *
 * The peak and the average survive the phase they describe, which is what makes
 * them worth a tile. Both are computed over the run's whole retained history,
 * not the visible window, so selecting a shorter span cannot scroll the attach
 * burst out of view and make the headline figure vanish.
 */
import type { TelemetryFrame } from "@/data/types";

export interface AttachRateTile {
  /** Headline: the peak rate reached, or the live rate while still attaching. */
  value: string;
  detail: string;
  /** True while UEs are still attaching, which is when the live rate matters. */
  active: boolean;
}

/** Below this a rate is treated as "not attaching" rather than a slow trickle.
 *  Well under one UE per sampling interval, so a genuinely slow attach still
 *  counts as active. */
const IDLE_RATE = 0.05;

function fmt(v: number): string {
  return v.toFixed(1);
}

/**
 * Builds the tile from the run's history, oldest-first.
 *
 * The average is taken over the samples where attaching was actually happening,
 * not over the whole run: dividing the population by the run's duration would
 * report a soak that attached 1000 UEs in 6 seconds and then ran for 15 minutes
 * as roughly 1/s, which is true of nothing anyone wants to know.
 */
export function attachRateTile(
  frames: TelemetryFrame[],
  successPct: (v: number) => string,
): AttachRateTile {
  const latest = frames.at(-1);
  const live = latest?.rates.attachPerSec ?? 0;
  const success = latest ? latest.rates.attachSuccess : 1;

  let peak = 0;
  let sum = 0;
  let activeSamples = 0;
  for (const f of frames) {
    const r = f.rates.attachPerSec;
    if (r > peak) peak = r;
    if (r > IDLE_RATE) {
      sum += r;
      activeSamples++;
    }
  }

  const active = live > IDLE_RATE;
  const avg = activeSamples > 0 ? sum / activeSamples : 0;

  // Nothing has attached yet: report that rather than an authoritative 0.0.
  if (peak === 0) {
    return { value: "0.0", detail: "no attaches yet", active: false };
  }

  const parts: string[] = [];
  if (active) {
    // While attaching, the live rate leads and the peak gives it context.
    parts.push(`peak ${fmt(peak)}/s`);
  } else {
    // Afterwards the peak IS the headline, so the detail carries the average
    // and says plainly that the phase is over.
    parts.push(`avg ${fmt(avg)}/s`);
  }
  parts.push(`success ${successPct(success)}%`);

  return {
    value: active ? fmt(live) : fmt(peak),
    detail: parts.join(" · "),
    active,
  };
}

/** Label for the tile, which changes with the phase so the number is never
 *  ambiguous about what it is measuring. */
export function attachRateLabel(t: AttachRateTile): string {
  return t.active ? "attach rate" : "peak attach rate";
}
