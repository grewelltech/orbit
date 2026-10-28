/**
 * The runs this tab started from Launch: the last few, kept in the tab
 * (sessionStorage) so a reload keeps them, listed on Launch with their
 * state and a Stop. A run started elsewhere is stopped from its run view.
 */
import { Code, ConnectError } from "@connectrpc/connect";
import { RunState, type GetRunResponse } from "@/gen/orbit/v1/run_pb";
import type { KeyValueStore } from "../targets/store";

export const STARTED_KEY = "orbit.launch.started";

/** How many the list keeps, newest first. */
export const STARTED_MAX = 5;

export interface StartedRun {
  id: string;
  name: string;
}

/** The tab's started runs, newest first; none when storage is unavailable or holds something else. */
export function loadStarted(session: KeyValueStore | undefined): StartedRun[] {
  try {
    const raw = session?.getItem(STARTED_KEY);
    const v: unknown = raw ? JSON.parse(raw) : [];
    if (!Array.isArray(v)) return [];
    return v.filter((x): x is StartedRun => typeof x?.id === "string" && typeof x?.name === "string").slice(0, STARTED_MAX);
  } catch {
    return [];
  }
}

/** Records a run this tab started, first, keeping STARTED_MAX; the list it keeps. */
export function recordStarted(session: KeyValueStore | undefined, run: StartedRun): StartedRun[] {
  const next = [run, ...loadStarted(session).filter((r) => r.id !== run.id)].slice(0, STARTED_MAX);
  try {
    session?.setItem(STARTED_KEY, JSON.stringify(next));
  } catch {
    /* storage unavailable: the list lasts this page */
  }
  return next;
}

const STATE: Partial<Record<RunState, string>> = {
  [RunState.PENDING]: "starting",
  [RunState.RUNNING]: "running",
  [RunState.DRAINING]: "draining",
  [RunState.COMPLETE]: "complete",
  [RunState.FAILED]: "failed",
  [RunState.CANCELLED]: "cancelled",
};

/** A status that cannot change: the run ended, or the server no longer knows it. */
export function settled(status: string | undefined): boolean {
  return status === "complete" || status === "failed" || status === "cancelled" || status === "gone";
}

/** The part of a RunService client the list reads with. */
export interface StartedClient {
  getRun(req: { runId: string }): Promise<GetRunResponse>;
}

/**
 * Each started run's status, read again for those not settled; a settled
 * one keeps its status, unread. A run the server does not know is gone;
 * a refusal of who is asking is an auth error, and anything else
 * unreachable, both read again next time.
 */
export async function pollStarted(client: StartedClient, runs: readonly StartedRun[], prev: Readonly<Record<string, string>>): Promise<Record<string, string>> {
  const next: Record<string, string> = {};
  for (const r of runs) {
    const was = prev[r.id];
    if (was !== undefined && settled(was)) {
      next[r.id] = was;
      continue;
    }
    try {
      const got = await client.getRun({ runId: r.id });
      next[r.id] = STATE[got.run?.state ?? RunState.UNSPECIFIED] ?? "?";
    } catch (err) {
      const e = ConnectError.from(err);
      next[r.id] =
        e.code === Code.NotFound
          ? "gone"
          : e.code === Code.Unauthenticated || e.code === Code.PermissionDenied
            ? `auth error: ${e.rawMessage}`
            : `unreachable: ${e.rawMessage}`;
    }
  }
  return next;
}
