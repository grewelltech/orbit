import { describe, expect, it } from "vitest";
import { toJson } from "@bufbuild/protobuf";
import { ResolvedTargetSchema } from "@/gen/orbit/v1/target_pb";
import { TARGET_FIELDS } from "./schema.gen";
import { parseNumber, requestTarget } from "./request";
import { addElement, newTarget, setSlices, setValue, type Step } from "./yamlio";

/** A document with every schema field set to a value of its own. */
function everyField() {
  const d = newTarget("t1");
  addElement(d, ["subscribers"], "pool1");
  addElement(d, ["app", "responders"], "resp1");
  const want = new Map<string, string>();
  let n = 11;
  for (const f of TARGET_FIELDS) {
    if (f.path === "version" || f.path === "kind" || f.path === "name" || f.path.endsWith("[].name")) continue;
    const [list, rest] = f.path.includes("[].") ? (f.path.split("[].") as [string, string]) : [undefined, f.path];
    const steps: Step[] = list === undefined ? rest.split(".") : [...list.split("."), 0, ...rest.split(".")];
    const v = String(n++);
    if (f.type === "slices") setSlices(d, steps, [{ sst: v, sd: "0a0b0c" }]);
    else if (f.type === "strings") setValue(d, steps, ["s" + v]);
    else setValue(d, steps, f.type === "uint" ? v : "v" + v);
    want.set(f.path, f.type === "uint" || f.type === "slices" ? v : f.type === "strings" ? "s" + v : "v" + v);
  }
  return { d, want };
}

describe("requestTarget", () => {
  it("carries every field of the schema onto the wire: a field Go adds and this drops fails here", () => {
    const { d, want } = everyField();
    const { target, unresolved } = requestTarget(d, {});
    expect(unresolved).toEqual([]);
    const json = JSON.stringify(toJson(ResolvedTargetSchema, target));
    for (const [path, v] of want) expect(json, path).toContain(v);
  });

  it("replaces each ${VAR} with the tab's value, anywhere in a string; one without a value is listed, not sent as a value", () => {
    const d = newTarget("t1");
    setValue(d, ["core", "amf"], "${HOST}:38412");
    addElement(d, ["subscribers"], "main");
    setValue(d, ["subscribers", 0, "ki"], "${ORBIT_KI}");
    setValue(d, ["subscribers", 0, "opc"], "${ORBIT_OPC}");
    const r = requestTarget(d, { HOST: "10.0.0.1", ORBIT_KI: "k".repeat(32) });
    expect(r.target.core?.amf).toBe("10.0.0.1:38412");
    expect(r.target.subscribers[0]?.ki).toBe("k".repeat(32));
    expect(r.unresolved).toEqual(["ORBIT_OPC"]);
  });

  it("numbers as the CLI reads them", () => {
    expect([parseNumber("7"), parseNumber("0x10"), parseNumber("0o10"), parseNumber("0b11"), parseNumber("0")]).toEqual([7, 16, 8, 3, 0]);
    expect(parseNumber("010")).toBeNaN();
    expect(parseNumber("${X}")).toBeNaN();
  });
});
