/**
 * The runs this tab started from Launch: the last few, kept in the tab
 * (sessionStorage) so a reload keeps them, listed on Launch with their
 * state and a Stop. A run started elsewhere is stopped from its run view.
 */
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
