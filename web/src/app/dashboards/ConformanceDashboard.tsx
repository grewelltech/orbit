import type { ReactNode } from "react";
import { EventStream } from "@/panels/EventStream";
import { SuiteResults } from "@/app/SuiteResults";
import type { Telemetry } from "@/hooks/useTelemetry";
import type { TestEvent } from "@/data/types";

interface Props {
  telemetry: Telemetry;
  runId: string | undefined;
  controls: ReactNode;
  logEvents: TestEvent[];
}

/**
 * The conformance dashboard: a conformance run is about verdicts, not
 * throughput, so it drops the load charts and leads with the suite results.
 */
export function ConformanceDashboard({ telemetry, runId, controls, logEvents }: Props) {
  return (
    <>
      {controls}

      <SuiteResults runId={runId} />

      <section className="grid min-h-0 flex-1 grid-cols-1 gap-3">
        <EventStream events={logEvents} onClear={telemetry.clearEvents} className="min-h-[320px]" />
      </section>
    </>
  );
}
