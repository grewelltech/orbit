import { describe, expect, it } from "vitest";
import { runEnded } from "./runConfig";

describe("when the run view reads a run's config again", () => {
  it("once the run has ended, however it ended", () => {
    for (const s of ["complete", "failed", "cancelled"]) expect(runEnded(s), s).toBe(true);
    for (const s of ["starting", "running", "draining", "idle", undefined]) expect(runEnded(s), String(s)).toBe(false);
  });
});
