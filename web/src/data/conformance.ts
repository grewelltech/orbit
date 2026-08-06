/**
 * Driving the conformance suite from the dashboard.
 *
 * The suite runs server-side and streams one update per completed test, which
 * is what a live view needs: a suite takes tens of seconds, and a client that
 * only saw the final summary could not tell a slow run from a hung one. This
 * hook holds the accumulating results and the running tally, and exposes a
 * `run()` the view calls.
 */
import { useCallback, useMemo, useRef, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  ConformanceService,
  type ConformanceResult,
  type ConformanceTally,
} from "@/gen/orbit/v1/conformance_pb";

export type { ConformanceResult, ConformanceTally };

export interface ConformanceState {
  results: ConformanceResult[];
  tally: ConformanceTally | undefined;
  /** 0..1 while running; undefined when idle. */
  progress: number | undefined;
  running: boolean;
  done: boolean;
  error: string | undefined;
}

export interface ConformanceParams {
  amfAddress: string;
  mcc?: string;
  mnc?: string;
  tac?: number;
  gnbIdBase?: number;
  sst?: number;
  sd?: string;
  upfN3?: string;
  n3Bind?: string;
  categories?: string[];
  perTestSeconds?: number;
}

const EMPTY: ConformanceState = {
  results: [],
  tally: undefined,
  progress: undefined,
  running: false,
  done: false,
  error: undefined,
};

export function useConformance(baseUrl?: string) {
  const client = useMemo(
    () =>
      createClient(
        ConformanceService,
        createConnectTransport({ baseUrl: baseUrl ?? window.location.origin }),
      ),
    [baseUrl],
  );
  const [state, setState] = useState<ConformanceState>(EMPTY);
  // A run in flight, so a second run() cancels the first rather than
  // interleaving two streams into one table.
  const abort = useRef<AbortController | null>(null);

  const run = useCallback(
    async (params: ConformanceParams) => {
      abort.current?.abort();
      const ctrl = new AbortController();
      abort.current = ctrl;
      setState({ ...EMPTY, running: true });

      try {
        const stream = client.runConformance(
          {
            amfAddress: params.amfAddress,
            mcc: params.mcc ?? "",
            mnc: params.mnc ?? "",
            tac: params.tac ?? 0,
            gnbIdBase: params.gnbIdBase ?? 0,
            sst: params.sst ?? 0,
            sd: params.sd ?? "",
            upfN3: params.upfN3 ?? "",
            n3Bind: params.n3Bind ?? "",
            categories: params.categories ?? [],
            perTestSeconds: params.perTestSeconds ?? 0,
          },
          { signal: ctrl.signal },
        );
        for await (const upd of stream) {
          setState((prev) => {
            const results = upd.result ? [...prev.results, upd.result] : prev.results;
            // total is 0 on the final summary update; keep the last real value
            // so the bar lands at 100% instead of snapping back.
            const progress =
              upd.total > 0 ? (upd.index + 1) / upd.total : prev.progress ?? (upd.done ? 1 : undefined);
            return {
              results,
              tally: upd.tally ?? prev.tally,
              progress: upd.done ? 1 : progress,
              running: !upd.done,
              done: upd.done,
              error: undefined,
            };
          });
        }
      } catch (err) {
        if (ctrl.signal.aborted) return; // superseded by a newer run; not an error
        setState((prev) => ({ ...prev, running: false, error: String(err) }));
      }
    },
    [client],
  );

  const cancel = useCallback(() => {
    abort.current?.abort();
    setState((prev) => ({ ...prev, running: false }));
  }, []);

  return { state, run, cancel };
}
