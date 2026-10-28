import { describe, expect, it } from "vitest";
import { getValue } from "../targets/yamlio";
import {
  addWorkload,
  durationOK,
  EXPECTED_TOO_LARGE,
  expectedEntries,
  gates,
  GATE_PRESETS,
  getUntil,
  inlineExpected,
  pickedFileRefusal,
  removeGate,
  removeWorkload,
  setCategories,
  setGate,
  setKeepUEs,
  setUntil,
  startFrom,
  workloads,
} from "./form";
import { parseRun, renderRun } from "./runyaml";

/** The document as the server reads it: written, then read back through the gate. */
const roundTrip = (doc: ReturnType<typeof startFrom>) => parseRun(renderRun(doc));

describe("the Launch form's edits", () => {
  it("starts from a template, or empty", () => {
    expect(workloads(startFrom("load"))).toEqual([{ index: 0, name: "load", kind: "population" }]);
    expect(workloads(startFrom("conformance"))).toEqual([{ index: 0, name: "conformance", kind: "conformance" }]);
    expect(workloads(startFrom(undefined))).toEqual([]);
  });

  it("adds workloads of each kind under free names, and removes one with its gates", () => {
    const doc = startFrom("load");
    expect(addWorkload(doc, "population")).toBe(1);
    expect(addWorkload(doc, "conformance")).toBe(2);
    expect(addWorkload(doc, "population")).toBe(3);
    expect(workloads(roundTrip(doc)).map((w) => `${w.name}:${w.kind}`)).toEqual(["load:population", "pop:population", "suite:conformance", "pop-2:population"]);
    setGate(doc, GATE_PRESETS[1]!.gate("pop"));
    removeWorkload(doc, 1);
    expect(workloads(doc).map((w) => w.name)).toEqual(["load", "suite", "pop-2"]);
    expect(gates(doc).map((g) => g.name)).toEqual(["attach_success_ratio"]);
  });

  it("sets until to one way to stop", () => {
    const doc = startFrom("load");
    expect(getUntil(doc)).toEqual({ kind: "complete" });
    setUntil(doc, { kind: "duration", duration: "10m" });
    expect(getUntil(roundTrip(doc))).toEqual({ kind: "duration", duration: "10m" });
    expect(getValue(doc, ["until", "complete"])).toBeUndefined();
    setUntil(doc, { kind: "forever" });
    expect(getUntil(roundTrip(doc))).toEqual({ kind: "forever" });
    expect(getValue(doc, ["until", "duration"])).toBeUndefined();
    setUntil(doc, { kind: "complete" });
    expect(getUntil(roundTrip(doc))).toEqual({ kind: "complete" });
  });

  it("checks a duration as Go reads one", () => {
    for (const d of ["30s", "2h", "1h30m", "500ms", "1.5h"]) expect(durationOK(d), d).toBe(true);
    for (const d of ["", "10", "0s", "1d", "-5m", "5 m"]) expect(durationOK(d), d).toBe(false);
  });

  it("sets each gate preset, replacing by name, and the run file keeps it", () => {
    const doc = startFrom("load");
    for (const p of GATE_PRESETS) setGate(doc, p.gate(p.label === "conformance" ? "" : "load"));
    const back = gates(roundTrip(doc));
    expect(back.map((g) => `${g.name}|${g.workload}|${g.kind}|${g.metric}|${g.op}|${g.threshold}`)).toEqual([
      "attach_success_ratio|load|slo|attach.success_ratio|>=|0.99",
      "registration_p99_ms|load|slo|registration.p99_ms|<=|500ms",
      "attach_p99_ms|load|slo|attach.p99_ms|<=|1s",
      "conformance||conformance|||",
    ]);
    setGate(doc, { ...back[1]!, threshold: "250ms" });
    expect(gates(doc)[1]!.threshold).toBe("250ms");
    expect(gates(doc)).toHaveLength(4);
    removeGate(doc, "attach_p99_ms");
    expect(gates(doc).map((g) => g.name)).toEqual(["attach_success_ratio", "registration_p99_ms", "conformance"]);
  });

  const list = "core: lab\nversion: '1'\nentries:\n  - {check: NGAP-NG-RESET-ACK, observed: FAIL, finding: EX-1, added: 2026-10-28, reason: r}\n";

  it("puts an expected-failures file inline", () => {
    const doc = startFrom("conformance");
    expect(expectedEntries(doc, 0)).toBeUndefined();
    inlineExpected(doc, 0, list);
    const back = roundTrip(doc);
    expect(expectedEntries(back, 0)).toBe(1);
    expect(getValue(back, ["workloads", 0, "conformance", "expected", "entries", 0, "added"])).toBe("2026-10-28");
  });

  const refusals: [string, string, string | RegExp][] = [
    ["an unknown key", "core: lab\nvendor: x\n", "an unknown field"],
    ["a list, not a mapping", "- check: X\n", "not a mapping"],
    ["an alias", "core: &c lab\nversion: *c\n", "anchors, aliases"],
    ["a tag", "core: !!str lab\n", "YAML tag"],
    ["a key written out", "core: lab\ntoken: abc\n", /unknown field|secret/],
    ["not YAML", "core: [\n", "not valid YAML"],
  ];
  for (const [name, text, why] of refusals) {
    it(`refuses an expected-failures file with ${name}, and leaves the document as it was`, () => {
      const doc = startFrom("conformance");
      const before = renderRun(doc);
      expect(() => inlineExpected(doc, 0, text)).toThrow(why);
      expect(renderRun(doc)).toBe(before);
    });
  }

  it("refuses an expected-failures file over 1 MiB", () => {
    const doc = startFrom("conformance");
    expect(() => inlineExpected(doc, 0, "core: lab\n# " + "x".repeat(1024 * 1024))).toThrow(EXPECTED_TOO_LARGE);
  });

  it("keeps a suite a suite with no categories, and a population's behaviors as asked", () => {
    const doc = startFrom("conformance");
    setCategories(doc, 0, ["nas", "ngap"]);
    expect(getValue(roundTrip(doc), ["workloads", 0, "conformance", "categories"])).toEqual(["nas", "ngap"]);
    setCategories(doc, 0, []);
    expect(workloads(roundTrip(doc))[0]!.kind).toBe("conformance");
    const pop = startFrom("load");
    setKeepUEs(pop, 0, true);
    expect(renderRun(pop)).toContain("behaviors: {}");
    setKeepUEs(pop, 0, false);
    expect(renderRun(pop)).not.toContain("behaviors");
  });

  it("refuses a picked expected-failures file over 1 MiB by its size, before reading it", () => {
    expect(pickedFileRefusal({ size: 1024 * 1024 })).toBeUndefined();
    expect(pickedFileRefusal({ size: 1024 * 1024 + 1 })).toBe(EXPECTED_TOO_LARGE);
  });
});
