/**
 * ConformanceDashboard — the conformance view: the suite's verdicts lead, with
 * the event log below for the registration and error lines behind them.
 *
 * Verdicts come through SuiteResults' own live/past bridge (SuiteWatch), so
 * this renders identically for a running suite and an archived one. Factored
 * out of App (ADR-0010).
 */
import { SuiteResults } from "@/app/SuiteResults";
import { EventStream } from "@/panels/EventStream";
import type { TestEvent } from "@/data/types";

export interface ConformanceDashboardProps {
  /** The run on view (pinned, else the live one). */
  runId: string | undefined;
  /** Events with conformance verdicts already filtered out. */
  logEvents: TestEvent[];
  clearEvents: () => void;
}

export function ConformanceDashboard({ runId, logEvents, clearEvents }: ConformanceDashboardProps) {
  return (
    <>
      <SuiteResults runId={runId} />
      <section className="grid min-h-0 flex-1 grid-cols-1 gap-3">
        <EventStream events={logEvents} onClear={clearEvents} className="min-h-[320px]" />
      </section>
    </>
  );
}
