import type { RunOutcome } from "@/data/runs";

/** A conformance run's row digest on Home: whether it reads as bad, and
 *  the counts worth naming beside the passes. */
export interface ConformanceDigest {
  bad: boolean;
  extra: string[];
}

/**
 * conformanceDigest colours a run's conformance tally. A run whose only
 * workload is the suite reads as its gates read: they judge it under its
 * policy and its expected-failures list. Any other run reads as bad on
 * every verdict the default gate fails on (FAIL outside the list, CRASH,
 * BLOCKED, ERROR, DEVIATION and WARN), since the tally alone cannot tell
 * which DEVIATIONs and WARNs a list tolerates.
 */
export function conformanceDigest(outcome: RunOutcome): ConformanceDigest | undefined {
  const c = outcome.conformance;
  if (!c) return undefined;
  const realFail = Math.max(0, c.failed - c.expectedFailures);
  const extra = [
    realFail > 0 ? `${realFail} fail` : "",
    c.crashed > 0 ? `${c.crashed} crash` : "",
    c.blocked > 0 ? `${c.blocked} blocked` : "",
    c.errored > 0 ? `${c.errored} error` : "",
    c.deviations > 0 ? `${c.deviations} deviation` : "",
    c.warned > 0 ? `${c.warned} warn` : "",
  ].filter(Boolean);
  const suiteOnly = outcome.kinds.length === 1 && outcome.kinds[0] === "conformance";
  const bad =
    outcome.gates && suiteOnly
      ? !outcome.gates.ok
      : realFail + c.crashed + c.blocked + c.errored + c.deviations + c.warned > 0;
  return { bad, extra };
}
