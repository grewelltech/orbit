/**
 * The Targets page's probe: a TargetService.Probe stream through the
 * dashboard's transport (its token, and the token prompt on a refusal),
 * ended when the page goes or the target changes.
 */
import { useCallback, useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { TargetService } from "@/gen/orbit/v1/target_pb";
import { orbitTransport } from "../transport";
import { endOnPageHide, IDLE, ProbeRunner, type ProbeRequestInit, type ProbeState } from "./probe";

export function useProbe(targetName: string | undefined, baseUrl?: string) {
  const client = useMemo(() => createClient(TargetService, orbitTransport(baseUrl)), [baseUrl]);
  const [state, setState] = useState<ProbeState>(IDLE);
  const runner = useMemo(() => new ProbeRunner(client, setState), [client]);
  // Leaving the page, or another target, ends the stream; the server then
  // deregisters the UE, stops its flow and releases its leases.
  useEffect(() => {
    setState(IDLE);
    return () => runner.dispose();
  }, [runner, targetName]);
  // A full page load (a link to another page, a reload) unmounts nothing.
  useEffect(() => endOnPageHide(window, runner), [runner]);
  const run = useCallback((req: ProbeRequestInit) => runner.run(req), [runner]);
  const cancel = useCallback(() => runner.cancel(), [runner]);
  return { state, run, cancel };
}
