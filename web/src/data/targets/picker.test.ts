import { describe, expect, it } from "vitest";
import { pickTarget } from "./picker";

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
