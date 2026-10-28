import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { createClient, createRouterTransport } from "@connectrpc/connect";
import { ConformanceResultSchema } from "@/gen/orbit/v1/conformance_pb";
import { GetRunReportResponseSchema, GetRunResponseSchema, RunEventSchema, RunKind, RunService, RunState, type GetRunResponse } from "@/gen/orbit/v1/run_pb";
import { checkVerdict, SuiteWatch, suitesOf, type CheckVerdict, type Suite } from "./suite";

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

describe("a check's verdict from the run's events", () => {
  const rows: [string, string, CheckVerdict | undefined][] = [
    ["CONFORMANCE", "NGAP-NG-RESET-ACK FAIL", { workload: "", id: "NGAP-NG-RESET-ACK", verdict: "FAIL" }],
    ["CONFORMANCE", "NAS-T3560-AUTH-RETRANSMIT SKIP", { workload: "", id: "NAS-T3560-AUTH-RETRANSMIT", verdict: "SKIP" }],
    ["CONFORMANCE", "X DEVIATION", { workload: "", id: "X", verdict: "DEVIATION" }],
    // A mixed run names the workload.
    ["CONFORMANCE", "[suite] NGAP-UNKNOWN-UE-SURVIVES FAIL", { workload: "suite", id: "NGAP-UNKNOWN-UE-SURVIVES", verdict: "FAIL" }],
    ["CONFORMANCE", "[s-2] X PASS", { workload: "s-2", id: "X", verdict: "PASS" }],
    ["CONFORMANCE", "[a] b] X PASS", undefined],
    ["ATTACH", "NGAP-NG-RESET-ACK FAIL", undefined],
    ["CONFORMANCE", "suite started", undefined],
    ["CONFORMANCE", "X MAYBE", undefined],
    ["CONFORMANCE", "X FAIL extra", undefined],
  ];
  for (const [kind, message, want] of rows) it(`${kind} "${message}"`, () => expect(checkVerdict({ kind, message })).toEqual(want));
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
          return create(GetRunResponseSchema, { run: { runId: "r", state, kind: RunKind.CONFORMANCE }, conformanceProgress: { ...progress, ran: n } });
        },
        getRunReport() {
          reports++;
          return create(GetRunReportResponseSchema, { report: { case: "conformance", value: { results: [result("A", "PASS")] } } });
        },
        async *runEvents() {
          yield create(RunEventSchema, { seq: 1n, kind: "RUN", message: "started" });
          yield create(RunEventSchema, { seq: 2n, kind: "CONFORMANCE", message: "A PASS" });
          yield create(RunEventSchema, { seq: 3n, kind: "CONFORMANCE", message: "[s1] B FAIL" });
        },
      });
    });
    return { client: createClient(RunService, transport), polls: () => n, reports: () => reports };
  }

  it("follows progress while the run is in flight, then reads the report once", async () => {
    const srv = server(2);
    const seen: { suites: Suite[]; done: boolean }[] = [];
    const checks: CheckVerdict[] = [];
    await new SuiteWatch(srv.client, "r", (suites, done) => seen.push({ suites, done }), 1, (v) => checks.push(v)).start();
    // Each check's verdict as it landed, in order; other events left out.
    expect(checks).toEqual([
      { workload: "", id: "A", verdict: "PASS" },
      { workload: "s1", id: "B", verdict: "FAIL" },
    ]);
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

  it("a failed GetRun is retried, with backoff, and the polls go on", async () => {
    let n = 0;
    const client = {
      getRun: async () => {
        n++;
        if (n === 1) throw new Error("the server restarting");
        return create(GetRunResponseSchema, { run: { runId: "r", state: RunState.COMPLETE, kind: RunKind.CONFORMANCE }, conformanceProgress: progress });
      },
      getRunReport: async () => create(GetRunReportResponseSchema, { report: { case: "conformance", value: { results: [result("A", "PASS")] } } }),
      runEvents: async function* () {},
    };
    const seen: boolean[] = [];
    await new SuiteWatch(client, "r", (_s, done) => seen.push(done), 1).start();
    expect(n).toBe(2);
    expect(seen).toEqual([true]);
  });

  it("a run with no suite (a load run): one GetRun, no more polls, no events read", async () => {
    let polls = 0;
    let events = 0;
    const client = {
      getRun: async () => {
        polls++;
        return create(GetRunResponseSchema, { run: { runId: "r", state: RunState.RUNNING, kind: RunKind.LOAD } });
      },
      getRunReport: async () => create(GetRunReportResponseSchema, {}),
      runEvents: async function* (_req: unknown, opts?: { signal?: AbortSignal }) {
        for (let i = 0; i < 100 && !opts?.signal?.aborted; i++) {
          events++;
          yield create(RunEventSchema, { kind: "CONFORMANCE", message: `X${i} PASS` });
          await new Promise((r) => setTimeout(r, 2));
        }
      },
    };
    const seen: Suite[][] = [];
    await new SuiteWatch(client, "r", (s) => seen.push(s), 1).start();
    expect(polls).toBe(1);
    expect(seen).toEqual([]);
    expect(events).toBeLessThan(100);
  });

  it("no verdict is handed on after dispose", async () => {
    let release = () => undefined as void;
    const client = {
      getRun: () => new Promise<GetRunResponse>(() => undefined),
      getRunReport: async () => create(GetRunReportResponseSchema, {}),
      // Ignores the signal: the guard, not the stream, has to stop it.
      runEvents: async function* () {
        yield create(RunEventSchema, { kind: "CONFORMANCE", message: "A PASS" });
        await new Promise<void>((r) => (release = r));
        yield create(RunEventSchema, { kind: "CONFORMANCE", message: "B FAIL" });
      },
    };
    const checks: CheckVerdict[] = [];
    const w = new SuiteWatch(client, "r", () => undefined, 1, (v) => checks.push(v));
    void w.start();
    await new Promise((r) => setTimeout(r, 10));
    w.dispose();
    release();
    await new Promise((r) => setTimeout(r, 10));
    expect(checks.map((c) => c.id)).toEqual(["A"]);
  });
});
