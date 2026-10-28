/**
 * StartedRuns — the runs this tab started from Launch (the last few), each
 * with its state, a link to its run view and, while in flight, Stop. A run
 * started elsewhere is stopped from its run view. Settled runs are not read
 * again (pollStarted), and once all are, the polling stops.
 */
import { useEffect, useMemo, useRef, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { Panel } from "@/components/Panel";
import { pollStarted, settled, type StartedRun } from "@/data/launch/startedRuns";
import { orbitTransport } from "@/data/transport";
import { StopControl } from "../StopControl";

export function StartedRuns({ runs }: { runs: readonly StartedRun[] }) {
  const client = useMemo(() => createClient(RunService, orbitTransport()), []);
  const [states, setStates] = useState<Record<string, string>>({});
  const seen = useRef<Record<string, string>>({});
  useEffect(() => {
    let live = true;
    let t: ReturnType<typeof setInterval> | undefined;
    const poll = async () => {
      const next = await pollStarted(client, runs, seen.current);
      if (!live) return;
      seen.current = next;
      setStates(next);
      // Every run settled: nothing left to read until the list changes.
      if (runs.every((r) => settled(next[r.id]))) clearInterval(t);
    };
    void poll();
    t = setInterval(() => void poll(), 3000);
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
