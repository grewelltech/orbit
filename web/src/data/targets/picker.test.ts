import { describe, expect, it } from "vitest";
import { pickInto, pickTarget } from "./picker";

describe("pickTarget", () => {
  it("fills the conformance form from a target, defaults under it", () => {
    const p = pickTarget('version: 1\nkind: target\nname: a\ncore:\n    amf: 10.0.0.1:38412\n    plmn:\n        mcc: "001"\n        mnc: "01"\n');
    expect(p).toEqual({ amf: "10.0.0.1:38412", identity: { mcc: "001", mnc: "01", tac: "1", sst: "1", sd: "" }, unresolved: [] });
  });

  it("takes the first slice and the target's TAC", () => {
    const p = pickTarget("version: 1\nkind: target\nname: a\ncore:\n    tac: 7\n    slices:\n        - sst: 2\n          sd: \"010203\"\n        - sst: 3\n");
    expect(p?.identity).toMatchObject({ tac: "7", sst: "2", sd: "010203" });
  });

  it("leaves a ${VAR} field empty and says so: the browser has no environment", () => {
    const p = pickTarget("version: 1\nkind: target\nname: a\ncore:\n    amf: ${AMF}\n");
    expect(p?.amf).toBe("");
    expect(p?.unresolved).toEqual(["core.amf"]);
  });

  it("a target that does not parse picks nothing", () => {
    expect(pickTarget("a: [1")).toBeUndefined();
  });
});

describe("pickInto", () => {
  const a = 'version: 1\nkind: target\nname: a\ncore:\n    amf: 10.0.0.1:38412\n    plmn:\n        mcc: "001"\n        mnc: "01"\n';
  const varAMF = "version: 1\nkind: target\nname: b\ncore:\n    amf: ${AMF}\n    slices:\n        - sst: ${SST}\n";
  const noAMF = "version: 1\nkind: target\nname: c\n";
  const empty = { amf: "", identity: { mcc: "", mnc: "", tac: "1", sst: "1", sd: "" }, unresolved: [] };

  it("picking B after A clears A's AMF when B's is a ${VAR} or absent: never the last target's core", () => {
    const afterA = pickInto(empty, a);
    expect(afterA.amf).toBe("10.0.0.1:38412");
    const afterB = pickInto(afterA, varAMF);
    expect(afterB.amf).toBe("");
    expect(afterB.identity.mcc).toBe("");
    expect(afterB.unresolved).toEqual(["core.amf", "core.slices[0].sst"]);
    expect(pickInto(afterA, noAMF).amf).toBe("");
  });

  it("a target that does not parse leaves the form", () => {
    const afterA = pickInto(empty, a);
    expect(pickInto(afterA, "a: [1")).toBe(afterA);
    expect(pickInto(afterA, undefined)).toBe(afterA);
  });
});
