import { describe, expect, it } from "vitest";
import { cpLatencyTile } from "./cpLatencyTile";
import type { TelemetryFrame } from "@/data/types";

const ms = (v: number) => v.toFixed(1);

function frame(procedure: string, count: number, p99 = 100): TelemetryFrame {
  return {
    cpLatency: { procedure, count, p50: 10, p90: 50, p99, max: p99 * 2 },
  } as TelemetryFrame;
}

/** n frames whose sample count never moves — an attach burst long finished. */
function frozen(n: number, procedure = "attach", count = 100): TelemetryFrame[] {
  return Array.from({ length: n }, () => frame(procedure, count));
}

describe("cpLatencyTile", () => {
  it("names the procedure it is reporting", () => {
    // The tile previously said "cp latency" whichever procedure it showed, so
    // one label meant handover latency in one scenario and attach in another.
    expect(cpLatencyTile([frame("attach", 5)], ms).label).toBe("attach p99");
    expect(cpLatencyTile([frame("handover_xn", 5)], ms).label).toBe("handover (Xn) p99");
    expect(cpLatencyTile([frame("handover_n2", 5)], ms).label).toBe("handover (N2) p99");
    expect(cpLatencyTile([frame("registration", 5)], ms).label).toBe("registration p99");
  });

  it("flags a value that has stopped advancing", () => {
    const t = cpLatencyTile(frozen(40), ms);
    expect(t.stale).toBe(true);
    expect(t.detail).toContain("none recent");
  });

  it("does not flag a procedure still producing samples", () => {
    // Count climbing across the window: handovers are still happening.
    const frames = Array.from({ length: 40 }, (_, i) => frame("handover_xn", i + 1));
    const t = cpLatencyTile(frames, ms);
    expect(t.stale).toBe(false);
    expect(t.detail).toContain("40 samples");
    expect(t.detail).not.toContain("none recent");
  });

  it("does not call an infrequent procedure stale", () => {
    // Only two handovers in the window, but the count DID move.
    const frames = [...frozen(30, "handover_xn", 1), frame("handover_xn", 2)];
    expect(cpLatencyTile(frames, ms).stale).toBe(false);
  });

  it("says so when nothing has been measured", () => {
    // Rather than rendering 0 ms as though the control plane were instant.
    const t = cpLatencyTile([frame("", 0)], ms);
    expect(t.label).toBe("cp latency p99");
    expect(t.detail).toContain("no procedures measured");
    expect(t.stale).toBe(false);
  });

  it("handles an empty history", () => {
    expect(() => cpLatencyTile([], ms)).not.toThrow();
    expect(cpLatencyTile([], ms).stale).toBe(false);
  });

  it("is not stale on a single frame", () => {
    // One sample is not evidence that anything stopped.
    expect(cpLatencyTile([frame("attach", 100)], ms).stale).toBe(false);
  });
});
