/**
 * Probing a target from the page: when keys may be sent, the probe's
 * steps as they stream in, and the stream's lifetime (a new probe, Cancel
 * or leaving the page ends it, and the server then deregisters the UE,
 * stops its flow and releases its leases).
 */
import { Code, ConnectError } from "@connectrpc/connect";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { ProbeRequestSchema, ProbeStep } from "@/gen/orbit/v1/target_pb";

/** A probe request as the client takes it. */
export type ProbeRequestInit = MessageInitShape<typeof ProbeRequestSchema>;

/**
 * Why keys may not be sent from this page, or undefined when they may: on
 * the server's own host, always (a server that wants a token answers
 * Unauthenticated, and the token prompt opens); from another host, only
 * over https:// and with an API token in this tab. The server's
 * keysInterceptor is the real check.
 */
export function keysBlocked(loc: { protocol: string; hostname: string }, token: string): string | undefined {
  const loopback = /^(localhost|127(\.\d{1,3}){3}|\[?::1\]?)$/i.test(loc.hostname);
  if (loopback) return undefined;
  if (loc.protocol !== "https:") {
    return "subscriber keys would cross the network in the clear: serve with TLS (orbit serve --tls auto) and open the dashboard over https://, or use it on the server's own host";
  }
  if (!token) return "no API token in this tab: from another host, keys go only with the server's token (orbit serve --token-file)";
  return undefined;
}

/**
 * The probe panel's line over the ${VAR}s it asks for: their values are
 * kept where the target's keys are (this tab, or this browser when the
 * target remembers its keys), as SecretField says.
 */
export function varsNote(remembered: boolean): string {
  const where = remembered ? "remembered in this browser" : "held in this tab";
  return `the target names these; their values are ${where} (keys are set in the target's fields):`;
}

/** Where a probe is. */
export type ProbePhase = "idle" | "running" | "done" | "failed" | "cancelled";

export interface ProbeState {
  phase: ProbePhase;
  steps: ProbeStep[];
  /** Why the probe did not run or broke off (not a FAIL step: those are steps). */
  error?: string;
  /** The field the server refused, from "invalid target: <path>: <reason>". */
  field?: { path: string; reason: string };
}

export const IDLE: ProbeState = { phase: "idle", steps: [] };

/** The field an InvalidArgument names, from the server's "invalid target: <path>: <reason>". */
export function refusedField(message: string): { path: string; reason: string } | undefined {
  const m = /invalid target: ([^\s:]+): (.*)$/.exec(message);
  return m ? { path: m[1] ?? "", reason: m[2] ?? "" } : undefined;
}

/** The state after a step arrives: in index order, a later copy of an index replacing it. */
export function withStep(s: ProbeState, step: ProbeStep): ProbeState {
  const steps = s.steps.filter((x) => x.index !== step.index).concat(step).sort((a, b) => a.index - b.index);
  return { ...s, steps };
}

/** The state when the stream ends: done, or why not. */
export function ended(s: ProbeState, err: unknown, aborted: boolean): ProbeState {
  if (aborted) return { ...s, phase: "cancelled" };
  if (err === undefined) return { ...s, phase: "done" };
  const ce = ConnectError.from(err);
  const field = ce.code === Code.InvalidArgument ? refusedField(ce.rawMessage) : undefined;
  return { ...s, phase: "failed", error: ce.rawMessage, ...(field ? { field } : {}) };
}

/** "N of 7 steps passed (M skipped)", or the core-only line, or undefined while there is nothing to sum up. */
export function summary(s: ProbeState, coreOnly: boolean): string | undefined {
  if (s.phase !== "done" || s.steps.length === 0) return undefined;
  const ok = s.steps.filter((x) => x.status === "OK").length;
  const skipped = s.steps.filter((x) => x.status === "SKIP").length;
  if (coreOnly && ok === s.steps.length) return "core OK; user plane not checked (core only)";
  return `${ok} of ${s.steps.length} steps passed${skipped > 0 ? ` (${skipped} skipped)` : ""}`;
}

/** The part of a TargetService client a probe uses. */
export interface ProbeStreamer {
  probe(req: ProbeRequestInit, opts: { signal: AbortSignal }): AsyncIterable<ProbeStep>;
}

/** One page's probes: at most one stream; a new one, cancel or dispose aborts the one before. */
export class ProbeRunner {
  private abort: AbortController | null = null;

  constructor(
    private readonly client: ProbeStreamer,
    private readonly setState: (f: (prev: ProbeState) => ProbeState) => void,
  ) {}

  /** The current probe's signal, for tests. */
  get signal(): AbortSignal | undefined {
    return this.abort?.signal;
  }

  async run(req: ProbeRequestInit): Promise<void> {
    this.abort?.abort();
    const ctrl = new AbortController();
    this.abort = ctrl;
    // A stream a newer probe or dispose superseded writes nothing more: the
    // state is the newer one's (or the next target's).
    const set = (f: (prev: ProbeState) => ProbeState) => {
      if (this.abort === ctrl) this.setState(f);
    };
    set(() => ({ phase: "running", steps: [] }));
    try {
      for await (const step of this.client.probe(req, { signal: ctrl.signal })) {
        set((prev) => withStep(prev, step));
      }
      set((prev) => ended(prev, undefined, ctrl.signal.aborted));
    } catch (err) {
      set((prev) => ended(prev, err, ctrl.signal.aborted));
    }
  }

  cancel(): void {
    this.abort?.abort();
  }

  dispose(): void {
    this.abort?.abort();
    this.abort = null;
  }
}

/**
 * Ends runner's probe when the page goes by a full page load (a link to
 * another page, a reload), which unmounts nothing: the server then
 * releases the probe's leases at once, not when its step times out.
 * Returns the remover.
 */
export function endOnPageHide(win: Pick<EventTarget, "addEventListener" | "removeEventListener">, runner: Pick<ProbeRunner, "dispose">): () => void {
  const end = () => runner.dispose();
  win.addEventListener("pagehide", end);
  return () => win.removeEventListener("pagehide", end);
}

/**
 * The probe's page lifecycle, as useProbe wires it: a full page load ends
 * the stream (endOnPageHide), and a page the back/forward cache restores
 * (pageshow, persisted), whose stream that ended, shows the probe idle
 * (reset) rather than as it was left, steps mid-run. Returns the remover.
 */
export function probePageLifecycle(win: Pick<EventTarget, "addEventListener" | "removeEventListener">, runner: Pick<ProbeRunner, "dispose">, reset: () => void): () => void {
  const removeHide = endOnPageHide(win, runner);
  const show = (e: Event) => {
    if ((e as Partial<PageTransitionEvent>).persisted === true) reset();
  };
  win.addEventListener("pageshow", show);
  return () => {
    removeHide();
    win.removeEventListener("pageshow", show);
  };
}
