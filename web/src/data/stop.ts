/**
 * Stopping a run from the run view: RunService.StopRun, as a cancel (the
 * old conformance page could cancel its suite; this keeps that). The full
 * Stop (a finish, on Launch and the run detail) comes with U3.
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

/**
 * Stops run runId (StopRun, a cancel): stopping while the server answers,
 * then stopped, or failed with the server's reason.
 */
export async function stopRun(client: StopClient, runId: string, set: (s: StopState) => void): Promise<void> {
  set({ phase: "stopping" });
  try {
    await client.stopRun({ runId, finish: false });
    set({ phase: "stopped" });
  } catch (err) {
    set({ phase: "failed", error: ConnectError.from(err).rawMessage });
  }
}
