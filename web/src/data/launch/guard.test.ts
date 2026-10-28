import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { quickTargetText } from "./useLaunch";
import { blockedNow } from "./resolve";

const suite = "kind: run\nworkloads:\n  - name: s\n    conformance: {categories: []}\n";
const quick = quickTargetText({ name: "core", amf: "192.0.2.1:38412", mcc: "001", mnc: "01", sst: "1", sd: "", tac: "" });
const base = { runYaml: suite, refs: [] as string[], values: {}, workloads: 1, loc: { protocol: "http:", hostname: "127.0.0.1" }, token: "" };

describe("why Launch may not send its run now (blockedNow, what start and plan check)", () => {
  it("is whyNotStart's reason: no target, no workload, a ${VAR} with no value, keys over plain http from another host", () => {
    expect(blockedNow({ ...base, targetYaml: undefined })).toContain("make a target first");
    expect(blockedNow({ ...base, targetYaml: quick, workloads: 0 })).toBe("add a workload");
    expect(blockedNow({ ...base, targetYaml: quick, refs: ["CATS"] })).toContain("set ${CATS} first");
    const keyed = quick + "subscribers:\n    - name: main\n      ki: 00112233445566778899aabbccddeeff\n";
    expect(blockedNow({ ...base, targetYaml: keyed, loc: { protocol: "http:", hostname: "192.0.2.10" } })).toContain("in the clear");
  });
  it("nothing when it may go", () => expect(blockedNow({ ...base, targetYaml: quick })).toBeUndefined());
});

/**
 * The hook's start and plan check blockedNow themselves, before anything
 * is sent, not only the buttons' disabled: read from the source (no DOM
 * here), comments and literals stripped.
 */
describe("useLaunch's start and plan refuse when blocked", () => {
  const src = readFileSync(join(__dirname, "useLaunch.ts"), "utf8")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1")
    .replace(/"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`/g, '""')
    .replace(/\s+/g, "");
  it("plan: the guard first, then the runner", () => {
    expect(src).toContain("plan:()=>{constwhy=blocked();if(why!==undefined){setNotice(why);returnPromise.resolve(undefined);}returninput?runner.plan(input):Promise.resolve(undefined);},");
  });
  it("start: the guard first, then the runner", () => {
    const at = src.indexOf("start:async()=>{constwhy=blocked();if(why!==undefined){setNotice(why);returnundefined;}");
    expect(at).toBeGreaterThan(0);
    expect(src.indexOf("runner.start(", at)).toBeGreaterThan(at);
  });
  it("blocked is blockedNow over the page's own inputs, the location and the token read when it is called", () => {
    expect(src).toContain("constblocked=()=>blockedNow({targetYaml:target?.yaml,runYaml,refs:input?refsOf(input):[],values,workloads:workloads(doc).length,loc:window.location,token:loadToken(),});");
  });
});
