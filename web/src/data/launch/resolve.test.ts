import { describe, expect, it } from "vitest";
import { formKey, planRequest, refsOf, refusedAt, renderRequest, type LaunchInput } from "./resolve";

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

  it("renders keyless: no values at all", () => {
    expect(renderRequest(input(all))).toEqual({ runYaml: run, targetName: "lab1", template: "fleet" });
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
