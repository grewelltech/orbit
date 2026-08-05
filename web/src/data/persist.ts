/**
 * Frame history that survives a page refresh.
 *
 * The server does not retain telemetry frames — RunTelemetry streams live
 * state and a finished run replays nothing (see issue #77). So a refresh
 * mid-run used to leave every chart empty and rebuild only from new frames,
 * throwing away the part of the run already watched. Events do not have this
 * problem: RunEvents replays its ring from sequence zero.
 *
 * Until frames are retained server-side this keeps a copy in the tab.
 * sessionStorage rather than localStorage on purpose: it is scoped to the tab
 * and cleared when the tab closes, which is exactly "a refresh should not
 * disturb the data" and not "yesterday's run should reappear".
 *
 * Stored frames are TRIMMED. `flows` and `cohorts` are the fat parts — up to a
 * hundred flow rows per frame — and nothing reads them from history; the
 * charts read scalars, and the panels that show flows and cohorts read only
 * the latest frame, which arrives within a second of reconnecting.
 */
import type { TelemetryFrame } from "./types";

const KEY = "orbit.history";

/** How stale a snapshot may be and still be restored. A refresh takes under a
 *  second; a minute-old snapshot means the tab was away long enough that the
 *  run may have moved on, and a gap drawn as continuous would be a lie. */
const MAX_AGE_MS = 60_000;

interface Stored {
  runId: string;
  savedAt: number;
  frames: TelemetryFrame[];
}

/** A frame with the bulky, history-irrelevant parts removed. */
function trim(f: TelemetryFrame): TelemetryFrame {
  return { ...f, flows: [], cohorts: [] };
}

export function saveHistory(runId: string, frames: TelemetryFrame[]): void {
  if (!runId || frames.length === 0) return;
  try {
    const payload: Stored = { runId, savedAt: Date.now(), frames: frames.map(trim) };
    sessionStorage.setItem(KEY, JSON.stringify(payload));
  } catch {
    // Quota or a blocked store: history is a convenience, not a correctness
    // requirement, and a failed save must never break the live view.
  }
}

/** The stored snapshot, or null when absent, unparseable or too old. */
export function loadHistory(): { runId: string; frames: TelemetryFrame[] } | null {
  try {
    const raw = sessionStorage.getItem(KEY);
    if (!raw) return null;
    const s = JSON.parse(raw) as Stored;
    if (!s?.runId || !Array.isArray(s.frames) || s.frames.length === 0) return null;
    if (Date.now() - s.savedAt > MAX_AGE_MS) return null;
    return { runId: s.runId, frames: s.frames };
  } catch {
    return null;
  }
}

export function clearHistory(): void {
  try {
    sessionStorage.removeItem(KEY);
  } catch {
    // Nothing to do; the freshness check keeps a stale entry from being used.
  }
}
