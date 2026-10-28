/**
 * A run's conformance suites as the run view shows them: while the run is
 * in flight, each suite's progress (GetRun: its tally, checks run of those
 * selected, the last check); once it has ended, every check's result
 * (GetRunReport). A conformance run has one suite; a mixed run has one per
 * conformance workload.
 */
import type { ConformanceResult, StaleExpectation } from "@/gen/orbit/v1/conformance_pb";
import type { ConformanceSummary } from "@/gen/orbit/v1/run_pb";
import { RunKind, RunState, type ConformanceProgress, type GetRunReportResponse, type GetRunResponse } from "@/gen/orbit/v1/run_pb";

export interface Suite {
  /** The workload's name; "" for a conformance run's one suite. */
  name: string;
  progress?: { pass: number; fail: number; error: number; skip: number; deviation: number; ran: number; total: number; lastCheck: string };
  results: ConformanceResult[];
  /** The expected-failures list the results were judged against, by its core line. */
  expectedCore: string;
  /** Entries of that list whose check ran without the FAIL they tolerate. */
  stale: StaleExpectation[];
}

/** A suite's results from its summary, or none yet. */
function fromSummary(summary: ConformanceSummary | undefined): Pick<Suite, "results" | "expectedCore" | "stale"> {
  return { results: summary?.results ?? [], expectedCore: summary?.expectedFailuresCore ?? "", stale: summary?.stale ?? [] };
}

function progressOf(p: ConformanceProgress | undefined): Suite["progress"] {
  if (!p) return undefined;
  const t = p.tally;
  return {
    pass: t?.passed ?? 0,
    fail: t?.failed ?? 0,
    error: t?.errored ?? 0,
    skip: t?.skipped ?? 0,
    deviation: t?.deviations ?? 0,
    ran: p.ran,
    total: p.total,
    lastCheck: p.lastCheck,
  };
}

/** The suites of a run: its progress from run, its results from report once there is one. */
export function suitesOf(run: GetRunResponse | undefined, report: GetRunReportResponse | undefined): Suite[] {
  const out: Suite[] = [];
  if (run?.conformanceProgress || report?.report.case === "conformance") {
    const summary = report?.report.case === "conformance" ? report.report.value : undefined;
    out.push({ name: "", progress: progressOf(run?.conformanceProgress), ...fromSummary(summary) });
  }
  const reported = new Map((report?.workloads ?? []).filter((w) => w.report.case === "conformance").map((w) => [w.workload, w]));
  for (const w of run?.workloads ?? []) {
    if (w.kind !== RunKind.CONFORMANCE) continue;
    const r = reported.get(w.name);
    const summary = r?.report.case === "conformance" ? r.report.value : undefined;
    out.push({
      name: w.name,
      progress: progressOf(w.progress.case === "conformance" ? w.progress.value : undefined),
      ...fromSummary(summary),
    });
    reported.delete(w.name);
  }
  // A suite the report has and the live view did not (a finished run read cold).
  for (const [name, r] of reported) {
    const summary = r.report.case === "conformance" ? r.report.value : undefined;
    out.push({ name, ...fromSummary(summary) });
  }
  return out;
}

/** Whether a run state is one it ends in. */
export function ended(state: RunState | undefined): boolean {
  return state === RunState.COMPLETE || state === RunState.FAILED || state === RunState.CANCELLED;
}

/** The part of a RunService client the suite view uses. */
export interface SuiteClient {
  getRun(req: { runId: string }, opts?: { signal?: AbortSignal }): Promise<GetRunResponse>;
  getRunReport(req: { runId: string }, opts?: { signal?: AbortSignal }): Promise<GetRunReportResponse>;
}

/**
 * Follows one run's suites: GetRun each interval while the run is in
 * flight, then GetRunReport once when it has ended. Stops on dispose.
 */
export class SuiteWatch {
  private readonly ctrl = new AbortController();

  constructor(
    private readonly client: SuiteClient,
    private readonly runId: string,
    private readonly onSuites: (s: Suite[], done: boolean) => void,
    private readonly intervalMs = 1000,
  ) {}

  async start(): Promise<void> {
    const { signal } = this.ctrl;
    try {
      for (;;) {
        const run = await this.client.getRun({ runId: this.runId }, { signal });
        if (signal.aborted) return;
        if (ended(run.run?.state)) {
          let report: GetRunReportResponse | undefined;
          try {
            report = await this.client.getRunReport({ runId: this.runId }, { signal });
          } catch {
            /* a run that failed before it had a report: its progress is all there is */
          }
          if (!signal.aborted) this.onSuites(suitesOf(run, report), true);
          return;
        }
        this.onSuites(suitesOf(run, undefined), false);
        await new Promise((r) => setTimeout(r, this.intervalMs));
        if (signal.aborted) return;
      }
    } catch {
      /* the stream of polls ends with the page, or with the server */
    }
  }

  dispose(): void {
    this.ctrl.abort();
  }
}
