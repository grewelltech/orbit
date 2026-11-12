import { describe, expect, it } from "vitest";
import { conformanceDigest } from "./conformanceDigest";
import type { RunOutcome } from "@/data/runs";

type Tally = NonNullable<RunOutcome["conformance"]>;

const clean: Tally = {
  total: 4, passed: 4, failed: 0, errored: 0, skipped: 0, deviations: 0,
  info: 0, warned: 0, blocked: 0, crashed: 0, expectedFailures: 0,
};

function outcome(c: Partial<Tally>, gates?: RunOutcome["gates"], kinds = ["conformance"]): RunOutcome {
  return { kinds, conformance: { ...clean, ...c }, gates };
}

describe("conformanceDigest", () => {
  it("reads a clean run as good", () => {
    expect(conformanceDigest(outcome({}, { total: 1, passed: 1, ok: true }))).toEqual({ bad: false, extra: [] });
  });
  it("reads an unexpected WARN with a failed gate as bad", () => {
    const d = conformanceDigest(outcome({ passed: 3, warned: 1 }, { total: 1, passed: 0, ok: false }));
    expect(d).toEqual({ bad: true, extra: ["1 warn"] });
  });
  it("names a DEVIATION and reads it as bad without gates", () => {
    expect(conformanceDigest(outcome({ passed: 3, deviations: 1 }))).toEqual({ bad: true, extra: ["1 deviation"] });
  });
  it("reads a WARN the gate passed (spec policy, or listed) as good", () => {
    expect(conformanceDigest(outcome({ passed: 3, warned: 1 }, { total: 1, passed: 1, ok: true }))?.bad).toBe(false);
  });
  it("reads a mixed run from its tally, not from gates an SLO shares", () => {
    const d = conformanceDigest(outcome({ passed: 3, warned: 1 }, { total: 2, passed: 2, ok: true }, ["conformance", "load"]));
    expect(d?.bad).toBe(true);
  });
  it("leaves an expected FAIL out", () => {
    expect(conformanceDigest(outcome({ passed: 3, failed: 1, expectedFailures: 1 }))).toEqual({ bad: false, extra: [] });
  });
  it("is undefined without a tally", () => {
    expect(conformanceDigest({ kinds: ["load"] })).toBeUndefined();
  });
});
