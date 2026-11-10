import type { ReactNode } from "react";
import { SuiteResults } from "@/app/SuiteResults";
import { ConformanceDashboard } from "./ConformanceDashboard";
import { PerformanceDashboard } from "./PerformanceDashboard";
import type { Telemetry } from "@/hooks/useTelemetry";
import type { TestEvent } from "@/data/types";

interface Props {
  /** Run kind from the run list ("load", "conformance", ...); undefined until it resolves. */
  kind: string | undefined;
  telemetry: Telemetry;
  selectedRun: string | undefined;
  /** The run on view: the pinned one, else the live one. */
  runId: string | undefined;
  windowMs: number;
  controls: ReactNode;
  logEvents: TestEvent[];
}

/**
 * Picks the dashboard for a run's kind. Conformance gets the verdict view;
 * mixed gets the performance view with the suite results above the charts;
 * load, fleet, scenario and any kind not yet resolved (or unknown to this
 * build) get the performance view, so charts are never hidden by accident.
 */
export function RunDashboard({ kind, telemetry, selectedRun, runId, windowMs, controls, logEvents }: Props) {
  if (kind === "conformance") {
    return <ConformanceDashboard telemetry={telemetry} runId={runId} controls={controls} logEvents={logEvents} />;
  }
  return (
    <PerformanceDashboard
      telemetry={telemetry}
      selectedRun={selectedRun}
      windowMs={windowMs}
      controls={controls}
      suite={kind === "mixed" ? <SuiteResults runId={runId} /> : undefined}
      logEvents={logEvents}
    />
  );
}
