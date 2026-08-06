/**
 * What the control-plane latency tile should say.
 *
 * The tile used to be labelled "cp latency p99" whatever it was showing, while
 * the value underneath was whichever procedure the scenario happened to
 * produce — handovers on a mobility run, attach on a static one. Two different
 * measurements under one name.
 *
 * Worse, attach latency is sampled once per UE during the attach phase, which
 * ends in the first seconds of a run. For the remaining minutes the number is
 * FROZEN, sitting beside genuinely live tiles and reading as "the control plane
 * is slow right now" when it means "the opening burst cost this". That
 * misreading is what prompted "did something break — is the p99 insane now?",
 * and the honest answer was that nothing had changed since the run started.
 *
 * So the tile names its procedure, and says when the number stopped moving.
 */
import type { TelemetryFrame } from "@/data/types";

/** How the procedure is named on the tile. */
const PROCEDURE_LABELS: Record<string, string> = {
  attach: "attach",
  registration: "registration",
  pdu_session: "pdu session",
  handover_xn: "handover (Xn)",
  handover_n2: "handover (N2)",
};

export interface CpLatencyTile {
  label: string;
  detail: string;
  /** True when the procedure has stopped producing samples, so the value
   *  describes the past rather than the present. */
  stale: boolean;
}

/** Frames to look back over when deciding whether the count is still moving.
 *  At the server's 1 Hz sampling this is half a minute — long enough not to
 *  call a slow trickle of handovers stale, short enough to notice promptly
 *  once the attach phase ends. */
const STALE_WINDOW = 30;

function procedureLabel(procedure: string): string {
  if (!procedure) return "cp latency";
  return PROCEDURE_LABELS[procedure] ?? procedure.replace(/_/g, " ");
}

/**
 * Builds the tile's label and detail from the recent history.
 *
 * `frames` is oldest-first; the last is the current sample.
 */
export function cpLatencyTile(frames: TelemetryFrame[], ms: (v: number) => string): CpLatencyTile {
  const latest = frames.at(-1);
  const cp = latest?.cpLatency;
  if (!cp || !cp.procedure) {
    // Nothing measured yet. Saying so beats showing 0 ms as though the control
    // plane were instantaneous.
    return { label: "cp latency p99", detail: "no procedures measured yet", stale: false };
  }

  const label = `${procedureLabel(cp.procedure)} p99`;
  const parts = [`p50 ${ms(cp.p50)}`, `p90 ${ms(cp.p90)}`];

  // Stale when the sample count has not moved across the window. Compared
  // against the count as it was, not against elapsed time, so a procedure that
  // is merely infrequent is not mislabelled as finished.
  const earlier = frames.length > STALE_WINDOW ? frames[frames.length - 1 - STALE_WINDOW] : frames[0];
  const stale =
    frames.length > 1 && earlier !== undefined && earlier.cpLatency.count === cp.count && cp.count > 0;

  if (stale) {
    // The value is history. Name the phase it came from rather than implying it
    // is current.
    parts.push(`${cp.count} samples, none recent`);
  } else if (cp.count > 0) {
    parts.push(`${cp.count} samples`);
  }

  return { label, detail: parts.join(" · "), stale };
}
