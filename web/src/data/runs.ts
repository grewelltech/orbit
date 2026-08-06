/**
 * The run list behind the picker.
 *
 * Separate from TelemetrySource on purpose: the source's job is to stream ONE
 * run, and it already restarts wholesale when the selection changes. Listing
 * what is available is a different question with a different lifetime, and
 * folding it in would mean the list disappeared whenever the stream was torn
 * down to switch runs.
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { RunKind, RunService, RunState, type Run as PbRun } from "@/gen/orbit/v1/run_pb";

/** One selectable run. */
export interface RunSummary {
  runId: string;
  name: string;
  kind: string;
  state: string;
  /** Still producing frames — a live run, as opposed to reviewable history. */
  active: boolean;
  startedAt: number;
}

const POLL_MS = 3000;

function summarise(r: PbRun): RunSummary {
  const active =
    r.state === RunState.RUNNING || r.state === RunState.PENDING || r.state === RunState.DRAINING;
  return {
    runId: r.runId,
    name: r.name,
    kind: runKindName(r.kind),
    state: runStateName(r.state),
    active,
    startedAt: Number(r.startedUnixNano / 1_000_000n),
  };
}

function runKindName(k: RunKind): string {
  switch (k) {
    case RunKind.LOAD:
      return "load";
    case RunKind.FLEET:
      return "fleet";
    case RunKind.SCENARIO:
      return "scenario";
    case RunKind.CONFORMANCE:
      return "conformance";
    default:
      // A kind added to the proto after this build. Showing the raw value beats
      // mislabelling it as one of the kinds above.
      return `kind-${k}`;
  }
}

function runStateName(s: RunState): string {
  switch (s) {
    case RunState.PENDING:
      return "pending";
    case RunState.RUNNING:
      return "running";
    case RunState.DRAINING:
      return "draining";
    case RunState.COMPLETE:
      return "complete";
    case RunState.FAILED:
      return "failed";
    case RunState.CANCELLED:
      return "cancelled";
    default:
      return "unknown";
  }
}

/**
 * Polls the server's run list. Newest first, as the server returns them.
 *
 * Polling rather than streaming because the list changes on the order of runs
 * starting and finishing, not on the order of frames — a stream here would cost
 * a connection to learn something that changes every few minutes.
 */
export function useRuns(baseUrl?: string): RunSummary[] {
  const client = useMemo(
    () =>
      createClient(
        RunService,
        createConnectTransport({ baseUrl: baseUrl ?? window.location.origin }),
      ),
    [baseUrl],
  );
  const [runs, setRuns] = useState<RunSummary[]>([]);

  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const { runs: got } = await client.listRuns({});
        if (!stop) setRuns(got.map(summarise));
      } catch {
        // The picker is not worth failing the page over; keep the last list and
        // try again. The header's own status dot already reports reachability.
      }
    };
    void tick();
    const h = setInterval(tick, POLL_MS);
    return () => {
      stop = true;
      clearInterval(h);
    };
  }, [client]);

  return runs;
}

/** Internals under test. */
export const __test = { summarise };
