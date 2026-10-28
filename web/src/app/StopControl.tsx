/**
 * StopControl — stops a run in flight, either way StopRun can: finish
 * (the gates judged on what ran) or cancel (the gates failed as
 * did-not-complete), each after its own confirm; disabled while the server
 * answers, then says it stopped or why not.
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { stoppable, stopRun, type StopMode, type StopState } from "@/data/stop";
import { orbitTransport } from "@/data/transport";

export function StopControl({ runId, state }: { runId: string | undefined; state: string | undefined }) {
  const client = useMemo(() => createClient(RunService, orbitTransport()), []);
  const [stop, setStop] = useState<StopState>({ phase: "idle" });
  useEffect(() => setStop({ phase: "idle" }), [runId]);
  if (!runId || (!stoppable(state) && stop.phase === "idle")) return null;
  const busy = stop.phase === "stopping" || stop.phase === "stopped";
  const button = (mode: StopMode, label: string) => (
    <button
      type="button"
      disabled={busy}
      className="o-label cursor-pointer border px-2 py-0.5 disabled:cursor-not-allowed disabled:opacity-60"
      style={{ color: mode === "cancel" ? "var(--o-error)" : "var(--o-amber)", borderColor: "var(--o-border)", borderRadius: "var(--o-radius)" }}
      onClick={() => void stopRun(client, runId, setStop, mode, (text) => window.confirm(text))}
    >
      {label}
    </button>
  );
  return (
    <span className="flex items-center gap-2">
      {stop.phase === "stopping" ? (
        <span className="o-label" style={{ color: "var(--o-ink-3)" }}>stopping…</span>
      ) : stop.phase === "stopped" ? (
        <span className="o-label" style={{ color: "var(--o-ink-2)" }}>stopped</span>
      ) : (
        <>
          {button("finish", "finish run")}
          {button("cancel", "cancel run")}
        </>
      )}
      {stop.phase === "failed" && (
        <span className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
          not stopped: {stop.error}
        </span>
      )}
    </span>
  );
}
