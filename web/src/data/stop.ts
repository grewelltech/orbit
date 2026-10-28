/**
 * Stopping a run: RunService.StopRun, in either of its modes. A finish
 * stops the workloads, keeps what they report, and judges the gates (the
 * run ends complete, stopped early); a cancel ends it cancelled, its gates
 * failed as did-not-complete. Each goes after its own confirm.
 */
import { ConnectError } from "@connectrpc/connect";
import type { StopRunResponse } from "@/gen/orbit/v1/run_pb";

/** Where a stop is: not asked, asked and not answered, done, or refused. */
export type StopPhase = "idle" | "stopping" | "stopped" | "failed";

export interface StopState {
  phase: StopPhase;
  /** Why it failed, in the server's words. */
  error?: string;
}

/** The part of a RunService client a stop uses. */
export interface StopClient {
  stopRun(req: { runId: string; finish: boolean }): Promise<StopRunResponse>;
}

/** The run states a run is in flight in: the ones Stop is offered for. */
export function stoppable(state: string | undefined): boolean {
  return state === "starting" || state === "running" || state === "draining";
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
 * stopping while the server answers, then stopped, or failed with the
 * server's reason. A declined confirm sends nothing.
 */
export async function stopRun(client: StopClient, runId: string, set: (s: StopState) => void, mode: StopMode = "cancel", confirm: (text: string) => boolean = () => true): Promise<void> {
  if (!confirm(stopConfirm(mode, runId))) return;
  set({ phase: "stopping" });
  try {
    await client.stopRun({ runId, finish: mode === "finish" });
    set({ phase: "stopped" });
  } catch (err) {
    set({ phase: "failed", error: ConnectError.from(err).rawMessage });
  }
}
