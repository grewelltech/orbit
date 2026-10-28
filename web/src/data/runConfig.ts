/**
 * A run's resolved config for the run view: GetRunConfig, read when the
 * run is picked and again when it ends (the archive then holds it); a
 * refusal (a run the server no longer knows) is kept as its reason.
 */
import { useEffect, useMemo, useState } from "react";
import { ConnectError, createClient } from "@connectrpc/connect";
import { RunService, type GetRunConfigResponse } from "@/gen/orbit/v1/run_pb";
import { orbitTransport } from "./transport";

export function useRunConfig(runId: string | undefined, ended: boolean, baseUrl?: string): { config?: GetRunConfigResponse; error?: string } {
  const client = useMemo(() => createClient(RunService, orbitTransport(baseUrl)), [baseUrl]);
  const [got, setGot] = useState<{ config?: GetRunConfigResponse; error?: string }>({});
  useEffect(() => {
    setGot({});
    if (!runId) return;
    let live = true;
    client
      .getRunConfig({ runId })
      .then((config) => live && setGot({ config }))
      .catch((e) => live && setGot({ error: ConnectError.from(e).rawMessage }));
    return () => {
      live = false;
    };
  }, [client, runId, ended]);
  return got;
}

/**
 * Whether a run view's state is one the run ended in: the config is read
 * again then, from the archive. Cancelled is named though the run view
 * reads a cancelled run as failed, so the two never disagree.
 */
export function runEnded(state: string | undefined): boolean {
  return state === "complete" || state === "failed" || state === "cancelled";
}

/**
 * What the run view says when GetRunConfig refused the run (a run id the
 * server does not know, one starting, one whose archive was evicted), or
 * undefined while there is a config or no answer yet: the refusal in the
 * server's words, not an empty page.
 */
export function runConfigRefusal(got: { config?: unknown; error?: string }): string | undefined {
  return got.config === undefined && got.error ? `no resolved config: ${got.error}` : undefined;
}
