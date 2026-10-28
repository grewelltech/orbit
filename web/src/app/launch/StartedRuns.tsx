/**
 * StartedRuns — the runs this tab started from Launch (the last few), each
 * with its state, a link to its run view and, while in flight, Stop. A run
 * started elsewhere is stopped from its run view.
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { RunService, RunState } from "@/gen/orbit/v1/run_pb";
import { Panel } from "@/components/Panel";
import type { StartedRun } from "@/data/launch/startedRuns";
import { orbitTransport } from "@/data/transport";
import { StopControl } from "../StopControl";

const STATE: Partial<Record<RunState, string>> = {
  [RunState.PENDING]: "starting",
  [RunState.RUNNING]: "running",
  [RunState.DRAINING]: "draining",
  [RunState.COMPLETE]: "complete",
  [RunState.FAILED]: "failed",
  [RunState.CANCELLED]: "cancelled",
};

export function StartedRuns({ runs }: { runs: readonly StartedRun[] }) {
  const client = useMemo(() => createClient(RunService, orbitTransport()), []);
  const [states, setStates] = useState<Record<string, string>>({});
  useEffect(() => {
    let live = true;
    const poll = async () => {
      const next: Record<string, string> = {};
      for (const r of runs) {
        try {
          const got = await client.getRun({ runId: r.id });
          next[r.id] = STATE[got.run?.state ?? RunState.UNSPECIFIED] ?? "?";
        } catch {
          next[r.id] = "gone";
        }
      }
      if (live) setStates(next);
    };
    void poll();
    const t = setInterval(() => void poll(), 3000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [client, runs]);
  if (runs.length === 0) return null;
  return (
    <Panel title="started from this tab" meta={`${runs.length}`}>
      <ul className="flex flex-col gap-1">
        {runs.map((r) => (
          <li key={r.id} className="flex flex-wrap items-center gap-3">
            <a href={`?run=${encodeURIComponent(r.id)}`} className="o-label" style={{ color: "var(--o-accent)", textTransform: "none" }}>
              {r.id}
            </a>
            <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
              {r.name ? `${r.name} · ` : ""}
              {states[r.id] ?? "…"}
            </span>
            <StopControl runId={r.id} state={states[r.id]} />
          </li>
        ))}
      </ul>
    </Panel>
  );
}
