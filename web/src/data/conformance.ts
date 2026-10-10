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
  /** PLMN. Required by the server, which has no default: pass what the core is configured with. */
  mcc: string;
  mnc: string;
  tac?: number;
  gnbIdBase?: number;
  sst?: number;
  /** Slice differentiator, 6 hex digits or empty for no SD (SD-Core: 010203). */
  sd?: string;
  upfN3?: string;
  n3Bind?: string;
  categories?: string[];
  perTestSeconds?: number;
}

/**
 * The network identity the dashboard sends. Kept as the user typed it (strings,
 * so an MNC's leading zero survives) and persisted between runs.
 */
export interface NetworkIdentity {
  mcc: string;
  mnc: string;
  tac: string;
  sst: string;
  sd: string;
}

/** The identity form starts empty for the PLMN (no guessing) and on the CLI defaults for the rest. */
export const DEFAULT_IDENTITY: NetworkIdentity = { mcc: "", mnc: "", tac: "1", sst: "1", sd: "" };

const IDENTITY_KEY = "orbit.conformance.identity";

/** Loads the last identity the user ran with; the defaults when none or when storage is unavailable. */
export function loadIdentity(): NetworkIdentity {
  try {
    const raw = window.localStorage.getItem(IDENTITY_KEY);
    if (!raw) return DEFAULT_IDENTITY;
    const parsed = JSON.parse(raw) as Partial<NetworkIdentity>;
    return { ...DEFAULT_IDENTITY, ...parsed };
  } catch {
    return DEFAULT_IDENTITY;
  }
}

/** Remembers the identity for the next run; silently a no-op when storage is unavailable. */
export function saveIdentity(id: NetworkIdentity): void {
  try {
    window.localStorage.setItem(IDENTITY_KEY, JSON.stringify(id));
  } catch {
    /* private window, blocked storage: the run still proceeds */
  }
}

/** The server's bounds: TAC is 24 bits and at least 1, SST is one octet. */
export const TAC_MAX = 0xffffff;
export const SST_MAX = 255;

/** A plain decimal integer within [min, max], as typed; the only thing the uint32 field accepts. */
function inRange(s: string, min: number, max: number): boolean {
  if (!/^\d+$/.test(s.trim())) return false;
  const n = Number(s);
  return Number.isInteger(n) && n >= min && n <= max;
}

/**
 * True when the identity can be sent: a PLMN is present and TAC/SST are plain
 * integers within the server's bounds, so the form refuses what the server
 * would refuse.
 */
export function identityComplete(id: NetworkIdentity): boolean {
  return id.mcc.trim() !== "" && id.mnc.trim() !== "" && inRange(id.tac, 1, TAC_MAX) && inRange(id.sst, 0, SST_MAX);
}

/** The RunConformance request for the given parameters, field for field. */
export function buildRequest(params: ConformanceParams) {
  return {
    amfAddress: params.amfAddress,
    mcc: params.mcc,
    mnc: params.mnc,
    tac: params.tac ?? 0,
    gnbIdBase: params.gnbIdBase ?? 0,
    sst: params.sst ?? 0,
    sd: params.sd ?? "",
    upfN3: params.upfN3 ?? "",
    n3Bind: params.n3Bind ?? "",
    categories: params.categories ?? [],
    perTestSeconds: params.perTestSeconds ?? 0,
  };
}

/** Maps the form's identity onto request parameters. */
export function paramsFromIdentity(amfAddress: string, id: NetworkIdentity): ConformanceParams {
  return {
    amfAddress,
    mcc: id.mcc.trim(),
    mnc: id.mnc.trim(),
    tac: Number(id.tac) || 0,
    sst: Number(id.sst) || 0,
    sd: id.sd.trim(),
  };
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
        const stream = client.runConformance(buildRequest(params), { signal: ctrl.signal });
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
