import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { createClient, createRouterTransport } from "@connectrpc/connect";
import { ConformanceResultSchema } from "@/gen/orbit/v1/conformance_pb";
import { GetRunReportResponseSchema, GetRunResponseSchema, RunKind, RunService, RunState, type GetRunResponse } from "@/gen/orbit/v1/run_pb";
import { SuiteWatch, suitesOf, type Suite } from "./suite";

const result = (id: string, verdict: string) => create(ConformanceResultSchema, { id, verdict });
const progress = { tally: { passed: 2, failed: 1 }, ran: 3, total: 9, lastCheck: "NGAP-X" };

describe("a run's suites", () => {
  it("a conformance run in flight: its progress, no results yet", () => {
    const run = create(GetRunResponseSchema, { run: { runId: "r", state: RunState.RUNNING }, conformanceProgress: progress });
    const [s] = suitesOf(run, undefined);
    expect(s).toMatchObject({ name: "", progress: { pass: 2, fail: 1, ran: 3, total: 9, lastCheck: "NGAP-X" }, results: [] });
  });

  it("a conformance run ended: every check's result, the expected list's core and its stale entries", () => {
    const run = create(GetRunResponseSchema, { run: { runId: "r", state: RunState.COMPLETE }, conformanceProgress: progress });
    const report = create(GetRunReportResponseSchema, {
      report: { case: "conformance", value: { results: [result("A", "PASS"), result("B", "FAIL")], expectedFailuresCore: "lab", stale: [{ check: "C", finding: "EX-1" }] } },
    });
    const [s] = suitesOf(run, report);
    expect(s!.results.map((r) => `${r.id}:${r.verdict}`)).toEqual(["A:PASS", "B:FAIL"]);
    expect(s!.expectedCore).toBe("lab");
    expect(s!.stale.map((x) => x.check)).toEqual(["C"]);
  });

  it("a mixed run: one suite per conformance workload, by name, populations left out", () => {
    const run = create(GetRunResponseSchema, {
      run: { runId: "r", state: RunState.COMPLETE },
      workloads: [
        { name: "pop", kind: RunKind.LOAD },
        { name: "s1", kind: RunKind.CONFORMANCE, progress: { case: "conformance", value: progress } },
      ],
    });
    const report = create(GetRunReportResponseSchema, {
      workloads: [
        { workload: "pop", kind: RunKind.LOAD },
        { workload: "s1", kind: RunKind.CONFORMANCE, report: { case: "conformance", value: { results: [result("A", "SKIP")] } } },
        // Reported, not in the live view: a finished run read cold.
        { workload: "s2", kind: RunKind.CONFORMANCE, report: { case: "conformance", value: { results: [result("B", "PASS")] } } },
      ],
    });
    expect(suitesOf(run, report).map((s) => `${s.name}:${s.results.length}:${s.progress?.ran ?? "-"}`)).toEqual(["s1:1:3", "s2:1:-"]);
  });

  it("a run with no suite: none", () => {
    expect(suitesOf(create(GetRunResponseSchema, { run: { runId: "r" } }), undefined)).toEqual([]);
    expect(suitesOf(undefined, undefined)).toEqual([]);
  });
});

describe("SuiteWatch over a server", () => {
  /** A RunService whose run is RUNNING for `polls` GetRuns, then COMPLETE with a report. */
  function server(polls: number) {
    let n = 0;
    let reports = 0;
    const transport = createRouterTransport(({ service }) => {
      service(RunService, {
        getRun(): GetRunResponse {
          n++;
          const state = n > polls ? RunState.COMPLETE : RunState.RUNNING;
          return create(GetRunResponseSchema, { run: { runId: "r", state }, conformanceProgress: { ...progress, ran: n } });
        },
        getRunReport() {
          reports++;
          return create(GetRunReportResponseSchema, { report: { case: "conformance", value: { results: [result("A", "PASS")] } } });
        },
      });
    });
    return { client: createClient(RunService, transport), polls: () => n, reports: () => reports };
  }

  it("follows progress while the run is in flight, then reads the report once", async () => {
    const srv = server(2);
    const seen: { suites: Suite[]; done: boolean }[] = [];
    await new SuiteWatch(srv.client, "r", (suites, done) => seen.push({ suites, done }), 1).start();
    expect(seen.map((v) => v.done)).toEqual([false, false, true]);
    expect(seen[1]!.suites[0]!.progress?.ran).toBe(2);
    expect(seen[2]!.suites[0]!.results.map((r) => r.id)).toEqual(["A"]);
    expect(srv.reports()).toBe(1);
  });

  it("stops polling on dispose", async () => {
    const srv = server(1_000_000);
    const w = new SuiteWatch(srv.client, "r", () => undefined, 5);
    const done = w.start();
    await new Promise((r) => setTimeout(r, 30));
    w.dispose();
    await done;
    const at = srv.polls();
    await new Promise((r) => setTimeout(r, 30));
    expect(srv.polls()).toBe(at);
    expect(srv.reports()).toBe(0);
  });
});
