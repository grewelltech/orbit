import { describe, expect, it } from "vitest";
import { runConfigRefusal, runEnded } from "./runConfig";

describe("when the run view reads a run's config again", () => {
  it("once the run has ended, however it ended", () => {
    for (const s of ["complete", "failed", "cancelled"]) expect(runEnded(s), s).toBe(true);
    for (const s of ["starting", "running", "draining", "idle", undefined]) expect(runEnded(s), String(s)).toBe(false);
  });
});

describe("a run GetRunConfig refused", () => {
  it("says so in the server's words, not an empty page", () => {
    expect(runConfigRefusal({ error: "run run-00000000 not found" })).toBe("no resolved config: run run-00000000 not found");
  });
  it("says nothing while there is a config, or no answer yet", () => {
    expect(runConfigRefusal({ config: {}, error: undefined })).toBeUndefined();
    expect(runConfigRefusal({ config: {}, error: "a stale refusal" })).toBeUndefined();
    expect(runConfigRefusal({})).toBeUndefined();
  });
});

