/**
 * StopButton — stops the run the view shows while it is in flight
 * (RunService.StopRun, a cancel), after a confirm; disabled while the
 * server answers, then says it stopped or why not.
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { stoppable, stopRun, type StopState } from "@/data/stop";
import { orbitTransport } from "@/data/transport";

export function StopButton({ runId, state }: { runId: string | undefined; state: string | undefined }) {
  const client = useMemo(() => createClient(RunService, orbitTransport()), []);
  const [stop, setStop] = useState<StopState>({ phase: "idle" });
  useEffect(() => setStop({ phase: "idle" }), [runId]);
  if (!runId || (!stoppable(state) && stop.phase === "idle")) return null;
  const label = stop.phase === "stopping" ? "stopping…" : stop.phase === "stopped" ? "stopped" : "stop run";
  return (
    <span className="flex items-center gap-2">
      <button
        type="button"
        disabled={stop.phase === "stopping" || stop.phase === "stopped"}
        className="o-label cursor-pointer border px-2 py-0.5 disabled:cursor-not-allowed disabled:opacity-60"
        style={{ color: "var(--o-error)", borderColor: "var(--o-border)", borderRadius: "var(--o-radius)" }}
        onClick={() => {
          if (window.confirm(`Stop ${runId}? It ends as cancelled; its gates are not judged.`)) void stopRun(client, runId, setStop);
        }}
      >
        {label}
      </button>
      {stop.phase === "failed" && (
        <span className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
          not stopped: {stop.error}
        </span>
      )}
    </span>
  );
}
