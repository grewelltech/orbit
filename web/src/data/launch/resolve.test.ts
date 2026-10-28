import { describe, expect, it } from "vitest";
import { carriesSecrets, formKey, planRequest, refsOf, refusedAt, renderRequest, startBlocked, type LaunchInput } from "./resolve";
import { quickTargetText } from "./useLaunch";

const target = 'version: 1\nkind: target\nname: lab1\n# ${IN_A_COMMENT}\ncore:\n  amf: "${LAB_AMF}"\nsubscribers:\n  - name: main\n    ki: "${ORBIT_KI}"\n    opc: "${ORBIT_OPC}"\n';
const run = 'kind: run\nworkloads:\n  - name: f\n    behaviors: {latency: {target: "${ECHO}"}}\n';
const input = (values: Record<string, string>): LaunchInput => ({ targetName: "lab1", targetYaml: target, runYaml: run, template: "fleet", values });
const all = { LAB_AMF: "192.0.2.10:38412", ORBIT_KI: "k", ORBIT_OPC: "o", ECHO: "198.51.100.9" };

describe("what Launch sends", () => {
  it("names the values' ${VAR}s, not a comment's", () => {
    expect(refsOf(input({}))).toEqual(["LAB_AMF", "ORBIT_KI", "ORBIT_OPC", "ECHO"]);
  });

  it("holds the plan back until every ${VAR} has a value", () => {
    expect(planRequest(input({ LAB_AMF: "a", ORBIT_KI: "k" }))).toEqual({ unresolved: ["ORBIT_OPC", "ECHO"] });
    expect(planRequest(input({ ...all, ECHO: "" })).unresolved).toEqual(["ECHO"]);
  });

  it("sends the documents as text, with each value, and nothing else", () => {
    const r = planRequest(input({ ...all, UNUSED: "u" })).request;
    expect(r).toEqual({
      targetYaml: target,
      runYaml: run,
      template: "fleet",
      vars: [
        { name: "LAB_AMF", value: "192.0.2.10:38412" },
        { name: "ORBIT_KI", value: "k" },
        { name: "ORBIT_OPC", value: "o" },
        { name: "ECHO", value: "198.51.100.9" },
      ],
    });
  });

  it("renders keyless: the target as kept (its ${VAR}s), no values at all", () => {
    expect(renderRequest(input(all))).toEqual({ runYaml: run, targetName: "lab1", template: "fleet", targetYaml: target });
  });

  it("keys the form state: any change of target, run file, template or a value sent", () => {
    const k = formKey(input(all));
    expect(formKey(input(all))).toBe(k);
    expect(formKey(input({ ...all, UNUSED: "u" }))).toBe(k); // a value not sent
    expect(formKey({ ...input(all), runYaml: run + "until: {forever: true}\n" })).not.toBe(k);
    expect(formKey({ ...input(all), template: "" })).not.toBe(k);
    expect(formKey({ ...input(all), targetName: "lab2" })).not.toBe(k);
    expect(formKey({ ...input(all), targetYaml: target.replace("main", "pool") })).not.toBe(k);
    expect(formKey(input({ ...all, ORBIT_KI: "k2" }))).not.toBe(k);
  });

  it("reads where the server's refusal points, as it words a parse and a resolve", () => {
    expect(refusedAt('invalid run: workloads[load].attach.count: "abc" is not a non-negative integer (run file lab1.run.yaml:8)')).toEqual({
      doc: "run file",
      line: 8,
      path: "workloads[load].attach.count",
      reason: '"abc" is not a non-negative integer',
    });
    expect(refusedAt('invalid run: core.plmn.mcc: "1" is not 3 digits (target lab1.yaml:6)')).toEqual({ doc: "target", line: 6, path: "core.plmn.mcc", reason: '"1" is not 3 digits' });
    expect(refusedAt("invalid run: lab1.run.yaml:9: workloads[load].behaviors: proto: unknown field")).toEqual({
      doc: "run file",
      line: 9,
      path: "workloads[load].behaviors",
      reason: "proto: unknown field",
    });
    expect(refusedAt("invalid run: lab1.run.yaml:9: unknown key nope")).toEqual({ doc: "run file", line: 9, path: undefined, reason: "unknown key nope" });
    expect(refusedAt("invalid run: lab1.yaml:5: core.amf: ${LAB_AMF} is not set")).toMatchObject({ doc: "target", path: "core.amf" });
    expect(refusedAt("invalid run: workloads[s]: give the list inline")).toMatchObject({ doc: "run file", path: "workloads[s]", line: undefined });
    expect(refusedAt("invalid run: no run file: a plan needs one")).toBeUndefined();
    expect(refusedAt("held by run-7")).toBeUndefined();
  });
});

describe("when Start needs keysBlocked's rule", () => {
  const remoteHttp = { protocol: "http:", hostname: "192.0.2.10" };
  const quick = quickTargetText({ name: "core", amf: "192.0.2.1:38412", mcc: "001", mnc: "01", sst: "1", sd: "", tac: "" });
  const suite = "kind: run\nworkloads:\n  - name: s\n    conformance: {categories: []}\n";
  it("a keyless run (a quick target) goes from a remote http dashboard", () => {
    expect(carriesSecrets({ targetYaml: quick, runYaml: suite })).toBe(false);
    expect(startBlocked(remoteHttp, "", { targetYaml: quick, runYaml: suite })).toBeUndefined();
  });
  it("a ${VAR} that needs a value (all vars are secret) is blocked there", () => {
    const withVar = quick.replace("192.0.2.1:38412", "${LAB_AMF}");
    expect(startBlocked(remoteHttp, "", { targetYaml: withVar, runYaml: suite })).toContain("in the clear");
    expect(startBlocked(remoteHttp, "", { targetYaml: quick, runYaml: suite.replace("categories: []", 'categories: ["${CATS}"]') })).toContain("in the clear");
  });
  it("a key written out in a secret slot is blocked there", () => {
    const keyed = quick + "subscribers:\n    - name: main\n      ki: 00112233445566778899aabbccddeeff\n";
    expect(carriesSecrets({ targetYaml: keyed, runYaml: suite })).toBe(true);
    expect(startBlocked(remoteHttp, "", { targetYaml: keyed, runYaml: suite })).toContain("in the clear");
  });
  it("on loopback, nothing is blocked", () => {
    expect(startBlocked({ protocol: "http:", hostname: "127.0.0.1" }, "", { targetYaml: target, runYaml: run })).toBeUndefined();
  });
});
