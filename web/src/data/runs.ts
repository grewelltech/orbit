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
import { isAuthRefusal, orbitTransport } from "./transport";
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
    info: number;
    warned: number;
    blocked: number;
    crashed: number;
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
      info: s.conformanceInfo,
      warned: s.conformanceWarned,
      blocked: s.conformanceBlocked,
      crashed: s.conformanceCrashed,
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
  /** Why the most recent poll failed, or null when it succeeded; on a failure
   * the list (if any) is stale. */
  problem: RunsProblem;
}

/**
 * Why a poll failed: the server could not be reached, or it answered and
 * refused the API token (none given yet, a wrong one, or a lockout) — the
 * token prompt is then open, and the server is not down.
 */
export type RunsProblem = "unreachable" | "unauthorized" | null;

/** The problem a failed ListRuns call reports. */
export function pollProblem(err: unknown): Exclude<RunsProblem, null> {
  return isAuthRefusal(err) ? "unauthorized" : "unreachable";
}

/**
 * The notice the run list shows for a failed poll, and whether it is a
 * warning (a stale list is still shown) or an error; null when there is none.
 */
export function problemNotice(problem: RunsProblem, loaded: boolean): { text: string; warn: boolean } | null {
  switch (problem) {
    case null:
      return null;
    case "unauthorized":
      return { text: "the server needs its API token — enter it to load runs", warn: true };
    case "unreachable":
      return loaded
        ? { text: "server unreachable — showing the last known runs (reconnecting…)", warn: true }
        : { text: "can't reach the server — retrying…", warn: false };
  }
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
  const [state, setState] = useState<RunsState>({ runs: [], loaded: false, problem: null });

  useEffect(() => {
    let stop = false;
    const tick = async () => {
      try {
        const { runs: got } = await client.listRuns({});
        if (!stop) setState({ runs: got.map(summarise), loaded: true, problem: null });
      } catch (err) {
        // Keep the last list and retry, but flag why this poll failed so the
        // caller can tell an unreachable server, or one waiting for its token,
        // from an empty list instead of presenting either as "nothing running".
        if (!stop) setState((s) => ({ ...s, problem: pollProblem(err) }));
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
