/**
 * RunDashboard — picks the dashboard for a run's kind (ADR-0010).
 *
 * A conformance run is about verdicts, so it gets ConformanceDashboard; a
 * load, fleet, scenario or still-unresolved run (and the ?mock demo) gets the
 * performance view so charts are never hidden by accident; a mixed run shows
 * both, the suite verdicts above the charts.
 */
import { SuiteResults } from "@/app/SuiteResults";
import { PerformanceDashboard } from "./PerformanceDashboard";
import { ConformanceDashboard } from "./ConformanceDashboard";
import type { Telemetry } from "@/hooks/useTelemetry";
import type { TestEvent } from "@/data/types";

export interface RunDashboardProps {
  /** Run-kind name from the run list ("load", "conformance", "mixed", ...);
   * undefined until it resolves, or in ?mock (always a load soak). */
  kind: string | undefined;
  mock: boolean;
  telemetry: Telemetry;
  windowMs: number;
  /** The pinned run, if any; undefined follows the latest. */
  selectedRun: string | undefined;
  /** The run on view (pinned, else live) — the id the suite view reads. */
  shownRun: string | undefined;
  logEvents: TestEvent[];
  clearEvents: () => void;
}

export function RunDashboard(props: RunDashboardProps) {
  const { kind, mock } = props;

  if (!mock && kind === "conformance") {
    return <ConformanceDashboard runId={props.shownRun} logEvents={props.logEvents} clearEvents={props.clearEvents} />;
  }

  const isMixed = !mock && kind === "mixed";
  return (
    <>
      {isMixed && <SuiteResults runId={props.shownRun} />}
      <PerformanceDashboard
        telemetry={props.telemetry}
        windowMs={props.windowMs}
        selectedRun={props.selectedRun}
        logEvents={props.logEvents}
        clearEvents={props.clearEvents}
      />
    </>
  );
}
