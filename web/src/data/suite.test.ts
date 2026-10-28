import { describe, expect, it } from "vitest";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { parseDocument } from "yaml";
import { create, fromJson } from "@bufbuild/protobuf";
import { createClient, createRouterTransport } from "@connectrpc/connect";
import { ConformanceResultSchema } from "@/gen/orbit/v1/conformance_pb";
import { GetRunReportResponseSchema, GetRunResponseSchema, RunEventSchema, RunKind, RunService, RunState, type GetRunResponse } from "@/gen/orbit/v1/run_pb";
import { checkVerdict, failedLabel, SuiteWatch, suitesOf, type CheckVerdict, type Suite } from "./suite";

const result = (id: string, verdict: string) => create(ConformanceResultSchema, { id, verdict });
const progress = { tally: { passed: 2, failed: 1 }, ran: 3, total: 9, lastCheck: "NGAP-X" };

/**
 * What the server answers for each Launch fixture's run, written by
 * internal/server's TestLaunchRequestsRunEndToEnd (-update-launch-runs):
 * GetRun in flight and at the end, and GetRunReport. suitesOf is held to
 * these, not to shapes written by hand.
 */
const runsDir = join(__dirname, "launch", "testdata", "runs");
const fixtureDir = join(__dirname, "launch", "testdata");
const fixtures = readdirSync(runsDir)
  .filter((f) => f.endsWith(".report.json"))
  .map((f) => f.replace(/\.report\.json$/, ""));
const readRun = (name: string, kind: string) => {
  const path = join(runsDir, `${name}.${kind}.json`);
  return existsSync(path) ? JSON.parse(readFileSync(path, "utf8")) : undefined;
};
/** The run file's conformance workloads, by name: the suites the run has. */
const suiteNames = (name: string): string[] => {
  const req = JSON.parse(readFileSync(join(fixtureDir, `${name}.json`), "utf8")) as { runYaml: string };
  const doc = parseDocument(req.runYaml).toJSON() as { workloads?: { name: string; conformance?: unknown }[] };
  return (doc.workloads ?? []).filter((w) => w.conformance !== undefined).map((w) => w.name);
};

describe("a run's suites, as the server answers", () => {
  it("has all five Launch fixtures' runs", () => expect([...fixtures].sort()).toEqual(["conformance", "conformance-amf-only", "fleet", "load", "mixed"]));
  for (const name of fixtures) {
    it(`${name}: ended, one suite per conformance workload, each once, with its results`, () => {
      const run = fromJson(GetRunResponseSchema, readRun(name, "getrun-final"));
      const report = fromJson(GetRunReportResponseSchema, readRun(name, "report"));
      const suites = suitesOf(run, report);
      expect(suites.map((s) => s.name)).toEqual(suiteNames(name));
      for (const s of suites) expect(s.results.length, s.name).toBeGreaterThan(0);
    });
    it(`${name}: in flight, a suite at most once per conformance workload, none for a population`, () => {
      const raw = readRun(name, "getrun-inflight");
      if (!raw) return; // the run ended before the first GetRun
      const suites = suitesOf(fromJson(GetRunResponseSchema, raw), undefined);
      const want = suiteNames(name);
      expect(suites.length).toBeLessThanOrEqual(want.length);
      if (want.length === 0) expect(suites).toEqual([]);
      expect(new Set(suites.map((s) => s.name)).size).toBe(suites.length);
    });
  }
});

describe("the failed label", () => {
  it("plain without an expected-failures list", () => expect(failedLabel(0, "")).toBe("failed"));
  it("counts the tolerated FAILs when a list applies", () => {
    expect(failedLabel(2, "lab core")).toBe("failed (2 expected)");
    expect(failedLabel(0, "lab core")).toBe("failed (0 expected)");
    expect(failedLabel(1, "")).toBe("failed (1 expected)"); // in flight: the tally knows, the core line comes at the end
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
  it("prefers the fields (RunEvent.check) when the server sends them", () => {
    const ev = create(RunEventSchema, { kind: "CONFORMANCE", message: "older text that says otherwise", check: { checkId: "NGAP-X", verdict: "FAIL", workload: "s1" } });
    expect(checkVerdict(ev)).toEqual({ workload: "s1", id: "NGAP-X", verdict: "FAIL" });
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
