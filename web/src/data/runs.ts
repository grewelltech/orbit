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
import { orbitTransport } from "./transport";
import { RunKind, RunService, RunState, type Run as PbRun } from "@/gen/orbit/v1/run_pb";

/** One selectable run. */
/**
 * RunOutcome is the headline digest the server attaches to a terminal run
 * (Run.summary), so the home page labels a run's result without fetching its
 * report. Each sub-object is present only when the run ran that kind of
 * workload. Absent entirely while a run is live or when it produced no report.
 */
export interface RunOutcome {
  /** Every workload kind in the run, named (always non-empty). */
  kinds: string[];
  conformance?: {
    total: number;
    passed: number;
    failed: number;
    errored: number;
    skipped: number;
    deviations: number;
    expectedFailures: number;
  };
  attach?: { attempted: number; succeeded: number; failed: number };
  gates?: { total: number; passed: number; ok: boolean };
}

export interface RunSummary {
  runId: string;
  name: string;
  kind: string;
  state: string;
  /** Still producing frames — a live run, as opposed to reviewable history. */
  active: boolean;
  startedAt: number;
  /** Epoch ms the run ended; 0 while it has not. */
  endedAt: number;
  /** Outcome digest for a terminal run; undefined while live / no report. */
  outcome?: RunOutcome;
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
    endedAt: Number(r.endedUnixNano / 1_000_000n),
    outcome: outcomeOf(r),
  };
}

/**
 * outcomeOf maps the proto Run.summary digest to RunOutcome, keeping only the
 * sub-objects the run actually produced (a load-only run has no conformance
 * tally, a gate-less run no gates). Returns undefined when no summary is set.
 */
function outcomeOf(r: PbRun): RunOutcome | undefined {
  const s = r.summary;
  if (!s) return undefined;
  const out: RunOutcome = { kinds: s.kinds.map(runKindName) };
  if (s.conformanceTotal > 0) {
    out.conformance = {
      total: s.conformanceTotal,
      passed: s.conformancePassed,
      failed: s.conformanceFailed,
      errored: s.conformanceErrored,
      skipped: s.conformanceSkipped,
      deviations: s.conformanceDeviations,
      expectedFailures: s.conformanceExpectedFailures,
    };
  }
  if (s.attachAttempted > 0) {
    out.attach = { attempted: s.attachAttempted, succeeded: s.attachSucceeded, failed: s.attachFailed };
  }
  if (s.gatesTotal > 0) {
    out.gates = { total: s.gatesTotal, passed: s.gatesPassed, ok: s.gatesOk };
  }
  return out;
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
    case RunKind.MIXED:
      // A plan with more than one workload (ADR-0008 §2). Without this case a
      // mixed run fell through to "kind-5" and lost its conformance half.
      return "mixed";
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
export interface RunsState {
  runs: RunSummary[];
  /** True once a ListRuns call has succeeded at least once — tells an empty
   * list ("no runs") apart from never having reached the server. */
  loaded: boolean;
  /** True when the most recent poll failed: the server is unreachable and the
   * list (if any) is stale. */
  error: boolean;
}

export function useRuns(baseUrl?: string): RunsState {
  const client = useMemo(
    () =>
      createClient(
        RunService,
        orbitTransport(baseUrl),
      ),
    [baseUrl],
  );
  const [state, setState] = useState<RunsState>({ runs: [], loaded: false, error: false });

  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const { runs: got } = await client.listRuns({});
        if (!stop) setState({ runs: got.map(summarise), loaded: true, error: false });
      } catch {
        // Keep the last list and retry, but flag that this poll failed so the
        // caller can distinguish an unreachable server from an empty list
        // instead of presenting a down server as "nothing running".
        if (!stop) setState((s) => ({ ...s, error: true }));
      }
    };
    void tick();
    const h = setInterval(tick, POLL_MS);
    return () => {
      stop = true;
      clearInterval(h);
    };
  }, [client]);

  return state;
}

/** Internals under test. */
export const __test = { summarise };
