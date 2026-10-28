/**
 * Stopping a run: RunService.StopRun, in either of its modes. A finish
 * stops the workloads, keeps what they report, and judges the gates (the
 * run ends complete, stopped early); a cancel ends it cancelled, its gates
 * failed as did-not-complete. Each goes after its own confirm.
 */
import { ConnectError } from "@connectrpc/connect";
import { RunState, type Run, type StopRunResponse } from "@/gen/orbit/v1/run_pb";

/** Where a stop is: not asked, asked and not answered, done, or refused. */
export type StopPhase = "idle" | "stopping" | "stopped" | "failed";

export interface StopState {
  phase: StopPhase;
  /** Why it failed, in the server's words. */
  error?: string;
  /** Once stopped, what the run is, as the server answered the stop (stoppedText). */
  final?: string;
}

/** The part of a RunService client a stop uses. */
export interface StopClient {
  stopRun(req: { runId: string; finish: boolean }): Promise<StopRunResponse>;
}

/** The run states a run is in flight in: the ones Stop is offered for. */
export function stoppable(state: string | undefined): boolean {
  return state === "starting" || state === "running" || state === "draining";
}

/**
 * What a stopped run is, as the StopRunResponse has it. A run that ended
 * while the confirm was open says how it ended (it was not stopped); one
 * the stop is still ending, "stopped".
 */
export function stoppedText(run: Run | undefined): string {
  switch (run?.state) {
    case RunState.COMPLETE:
      return run.stoppedEarly ? "complete, stopped early" : "complete";
    case RunState.CANCELLED:
      return "cancelled";
    case RunState.FAILED:
      return "failed";
    default:
      return "stopped";
  }
}

/**
 * What the Stop control shows: nothing for a run not in flight that it has
 * not stopped, the two buttons, or (instead of the buttons, so nothing can
 * be sent twice) stopping and then the run as stopped.
 */
export function stopView(state: string | undefined, phase: StopPhase): "none" | "buttons" | "stopping" | "stopped" {
  if (phase === "stopping" || phase === "stopped") return phase;
  return stoppable(state) || phase === "failed" ? "buttons" : "none";
}

/** How a stop ends a run. */
export type StopMode = "finish" | "cancel";

/** What the confirm for mode says before anything is sent. */
export function stopConfirm(mode: StopMode, runId: string): string {
  return mode === "finish"
    ? `Finish ${runId}? Its workloads stop and report what they have, and its gates are judged on that (it ends complete, stopped early).`
    : `Cancel ${runId}? It ends cancelled, and its gates are failed as did-not-complete.`;
}

/**
 * Stops run runId in mode, after confirm (stopConfirm's text) says yes:
 * stopping while the server answers, then stopped (with what the run is,
 * stoppedText), or failed with the server's reason. A declined confirm sends nothing.
 */
export async function stopRun(client: StopClient, runId: string, set: (s: StopState) => void, mode: StopMode = "cancel", confirm: (text: string) => boolean = () => true): Promise<void> {
  if (!confirm(stopConfirm(mode, runId))) return;
  set({ phase: "stopping" });
  try {
    const res = await client.stopRun({ runId, finish: mode === "finish" });
    set({ phase: "stopped", final: stoppedText(res.run) });
  } catch (err) {
    set({ phase: "failed", error: ConnectError.from(err).rawMessage });
  }
}
