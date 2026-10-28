import { describe, expect, it } from "vitest";
import { getValue, parseTargets } from "../targets/yamlio";
import type { KeyValueStore } from "../targets/store";
import { workloads } from "./form";
import { DRAFT_KEY, dropRetiredKeys, initialDraft, quickTargetProblem, quickTargetText, TAC_MAX, type QuickTarget } from "./useLaunch";

function memory(init: Record<string, string> = {}): KeyValueStore {
  const m = new Map(Object.entries(init));
  return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v), removeItem: (k) => void m.delete(k) };
}

describe("a target from an AMF and a network identity", () => {
  it("is a target the Targets page reads, with no subscriber pool", () => {
    const [t] = parseTargets(quickTargetText({ name: " core ", amf: "192.0.2.10:38412", mcc: "001", mnc: "01", sst: "1", sd: "010203", tac: "" }));
    const doc = t!.doc;
    expect(getValue(doc, ["name"])).toBe("core");
    expect(getValue(doc, ["core", "amf"])).toBe("192.0.2.10:38412");
    expect(getValue(doc, ["core", "plmn", "mcc"])).toBe("001");
    expect(getValue(doc, ["core", "plmn", "mnc"])).toBe("01");
    expect(doc.getIn(["subscribers"])).toBeUndefined();
    expect(doc.toJSON().core.slices).toEqual([{ sst: "1", sd: "010203" }]);
  });

  it("leaves the slices to the defaults without an SST", () => {
    const [t] = parseTargets(quickTargetText({ name: "c", amf: "a:1", mcc: "001", mnc: "01", sst: "", sd: "", tac: "" }));
    expect(t!.doc.getIn(["core", "slices"])).toBeUndefined();
  });
});

describe("the quick target's TAC", () => {
  const q: QuickTarget = { name: "c", amf: "a:1", mcc: "001", mnc: "01", sst: "", sd: "", tac: "" };
  it("goes in the target when given; none means the default", () => {
    const [t] = parseTargets(quickTargetText({ ...q, tac: "7" }));
    expect(getValue(t!.doc, ["core", "tac"])).toBe("7");
    expect(parseTargets(quickTargetText(q))[0]!.doc.getIn(["core", "tac"])).toBeUndefined();
  });
  const rows: [string, string | undefined][] = [
    ["", undefined],
    ["1", undefined],
    [String(TAC_MAX), undefined],
    ["0", "a TAC is 1"],
    [String(TAC_MAX + 1), "a TAC is 1"],
    ["x", "a TAC is 1"],
    ["-1", "a TAC is 1"],
    ["1.5", "a TAC is 1"],
    ["007", "TAC 007 has a leading zero; write 7"],
    ["010", "write 10"],
    ["00", "TAC 00 has a leading zero; write 0"],
  ];
  for (const [tac, want] of rows) {
    it(`TAC "${tac}"`, () => {
      const got = quickTargetProblem({ ...q, tac });
      if (want === undefined) expect(got).toBeUndefined();
      else expect(got).toContain(want);
    });
  }
  it("needs a name, an AMF and a PLMN", () => expect(quickTargetProblem({ ...q, amf: " " })).toContain("an AMF"));
});

describe("the run the page opens on", () => {
  const draft = (template: string, yaml: string) => memory({ [DRAFT_KEY]: JSON.stringify({ target: "lab1", template, yaml }) });
  const load = "kind: run\nworkloads:\n  - name: mine\n    attach: {count: 7}\n";

  it("the tab's draft", () => {
    expect(workloads(initialDraft(draft("load", load), undefined).doc).map((w) => w.name)).toEqual(["mine"]);
  });

  it("?conformance: the suite, unless the draft already started from it", () => {
    const fresh = initialDraft(draft("load", load), "conformance");
    expect(fresh.draft.template).toBe("conformance");
    expect(fresh.replaced).toBe("load"); // the page says so
    expect(initialDraft(memory(), "conformance").replaced).toBeUndefined(); // no draft: nothing replaced
    expect(workloads(fresh.doc).map((w) => w.kind)).toEqual(["conformance"]);
    const suite = "kind: run\nworkloads:\n  - name: s\n    conformance: {categories: [nas]}\n";
    expect(workloads(initialDraft(draft("conformance", suite), "conformance").doc).map((w) => w.name)).toEqual(["s"]);
  });

  it("no draft, or one that no longer reads: the load template", () => {
    expect(workloads(initialDraft(memory(), undefined).doc).map((w) => w.name)).toEqual(["load"]);
    expect(workloads(initialDraft(draft("load", "kind: target\n"), undefined).doc).map((w) => w.name)).toEqual(["load"]);
  });
});

describe("the old conformance page's key", () => {
  it("is removed once, and nothing else is", () => {
    const local = memory({ "orbit.conformance.identity": "{}", "orbit.targets.v1": "x" });
    dropRetiredKeys(local);
    expect(local.getItem("orbit.conformance.identity")).toBeNull();
    expect(local.getItem("orbit.targets.v1")).toBe("x");
    dropRetiredKeys(undefined);
  });
});
