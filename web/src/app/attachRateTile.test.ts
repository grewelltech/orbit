import { describe, expect, it } from "vitest";
import { attachRateTile, attachRateLabel } from "./attachRateTile";
import type { TelemetryFrame } from "@/data/types";

const pct = (v: number) => (v * 100).toFixed(1);

function frame(attachPerSec: number, success = 1): TelemetryFrame {
  return { rates: { attachPerSec, attachSuccess: success } } as TelemetryFrame;
}

/** An attach burst that finishes, then a long idle tail — the shape of every
 *  fleet run: attach in seconds, then run for minutes. */
function burstThenIdle(idleSamples: number): TelemetryFrame[] {
  return [
    frame(0),
    frame(120),
    frame(167.4),
    frame(150),
    frame(40),
    ...Array.from({ length: idleSamples }, () => frame(0)),
  ];
}

describe("attachRateTile", () => {
  it("keeps the peak visible after attaching finishes", () => {
    // The regression: the tile read 0.0/s for the rest of the run, so the one
    // number worth knowing was on screen for seconds and then gone.
    const t = attachRateTile(burstThenIdle(600), pct);
    expect(t.active).toBe(false);
    expect(t.value).toBe("167.4");
    expect(attachRateLabel(t)).toBe("peak attach rate");
  });

  it("averages over the attach phase, not the whole run", () => {
    // Dividing by the run's duration would report a 1000-UE burst followed by
    // 15 idle minutes as roughly 1/s, which is true of nothing.
    const t = attachRateTile(burstThenIdle(600), pct);
    const avg = (120 + 167.4 + 150 + 40) / 4;
    expect(t.detail).toContain(`avg ${avg.toFixed(1)}/s`);
  });

  it("shows the live rate while attaching, with the peak as context", () => {
    const t = attachRateTile([frame(0), frame(120), frame(150)], pct);
    expect(t.active).toBe(true);
    expect(t.value).toBe("150.0");
    expect(t.detail).toContain("peak 150.0/s");
    expect(attachRateLabel(t)).toBe("attach rate");
  });

  it("says nothing has attached rather than asserting 0.0", () => {
    const t = attachRateTile([frame(0), frame(0)], pct);
    expect(t.detail).toBe("no attaches yet");
    expect(t.active).toBe(false);
  });

  it("always carries the success ratio", () => {
    expect(attachRateTile(burstThenIdle(5), pct).detail).toContain("success 100.0%");
    // attachSuccess is cumulative, so it persists on later frames rather than
    // resetting once attaching stops.
    const degraded = [frame(0), frame(50, 0.83), frame(0, 0.83)];
    expect(attachRateTile(degraded, pct).detail).toContain("success 83.0%");
  });

  it("handles an empty history", () => {
    expect(() => attachRateTile([], pct)).not.toThrow();
    expect(attachRateTile([], pct).value).toBe("0.0");
  });

  it("does not treat a slow trickle as idle", () => {
    // A genuinely slow attach must still read as active rather than being
    // rounded away as noise.
    const t = attachRateTile([frame(0), frame(0.4)], pct);
    expect(t.active).toBe(true);
  });
});
