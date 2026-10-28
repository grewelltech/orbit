import { describe, expect, it } from "vitest";
import type { KeyValueStore } from "../targets/store";
import { loadStarted, recordStarted, STARTED_KEY, STARTED_MAX } from "./startedRuns";

function memory(init: Record<string, string> = {}): KeyValueStore {
  const m = new Map(Object.entries(init));
  return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v), removeItem: (k) => void m.delete(k) };
}

describe("the runs this tab started", () => {
  it("newest first, the last few, a run once", () => {
    const s = memory();
    for (let i = 1; i <= STARTED_MAX + 2; i++) recordStarted(s, { id: `run-${i}`, name: `n${i}` });
    // What the tab stores is capped, not only what is read back.
    expect((JSON.parse(s.getItem(STARTED_KEY) ?? "[]") as unknown[]).length).toBe(STARTED_MAX);
    recordStarted(s, { id: "run-5", name: "n5" });
    expect(loadStarted(s).map((r) => r.id)).toEqual(["run-5", "run-7", "run-6", "run-4", "run-3"]);
  });

  it("kept in the tab across a reload (the same store, read afresh)", () => {
    const s = memory();
    recordStarted(s, { id: "run-1", name: "" });
    expect(loadStarted(s)).toEqual([{ id: "run-1", name: "" }]);
  });

  it("nothing from a store that holds something else, or none", () => {
    expect(loadStarted(memory({ [STARTED_KEY]: "{not json" }))).toEqual([]);
    expect(loadStarted(memory({ [STARTED_KEY]: '[{"id": 1}]' }))).toEqual([]);
    expect(loadStarted(undefined)).toEqual([]);
  });
});
