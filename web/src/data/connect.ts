/**
 * ConnectSource — a TelemetrySource backed by the real ORBIT RunService.
 *
 * It finds a run to watch (the active one, else the most recent), streams
 * RunTelemetry, and maps each TelemetryFrame onto the dashboard's frame model.
 * When no run is active it polls until one appears, so a dashboard left open
 * lights up as soon as a run starts.
 *
 * Honest-scope note: RunTelemetry currently carries only a load run's attach
 * aggregates (attempted/succeeded/failed, rate, procedure latency). Those map
 * to the UE-population funnel, the attach-rate, and the CP-latency panels.
 * Throughput, per-gNB distribution, and the event stream arrive with later
 * build-order steps (richer frames + RunEvents); until then those panels stay
 * empty rather than showing invented data.
 */
import { createClient, type Client } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import {
  RunService,
  RunState,
  EventSeverity as PbSeverity,
  type Run as PbRun,
  type TelemetryFrame as PbFrame,
  type RunEvent as PbEvent,
  type LoadProgress,
} from "@/gen/orbit/v1/run_pb";
import { Emitter, type TelemetrySource } from "./source";
import type { EventSeverity, LatencySummary, SourceState, TelemetryFrame, TestEvent } from "./types";

const POLL_INTERVAL_MS = 2000;
const FRAME_INTERVAL_MS = 500;

export interface ConnectOptions {
  /** Base URL of the orbit server. Defaults to same-origin (embedded UI). */
  baseUrl?: string;
  /** Watch a specific run instead of auto-selecting the active one. */
  runId?: string;
}

export class ConnectSource implements TelemetrySource {
  readonly name = "orbit";

  private readonly client: Client<typeof RunService>;
  private readonly frames = new Emitter<TelemetryFrame>();
  private readonly events = new Emitter<TestEvent>();
  private readonly states = new Emitter<SourceState>();
  private readonly pinnedRunId: string | undefined;

  private abort: AbortController | null = null;
  private conn: SourceState = "disconnected";
  private prev: PbFrame | null = null;
  private started = false;
  // Monotonic across the source's life. RunEvent.seq restarts at 0 per run, so
  // using it as the dashboard event id would collide across runs and duplicate
  // React keys in the event log; this counter never resets.
  private nextEventId = 0;
  // The last terminal run already streamed to completion. Prevents re-streaming
  // a finished run every poll when no live run exists — the dashboard shows its
  // final frame once, then waits for a new or active run.
  private lastTerminalId: string | null = null;

  constructor(opts: ConnectOptions = {}) {
    this.client = createClient(
      RunService,
      createConnectTransport({ baseUrl: opts.baseUrl ?? window.location.origin }),
    );
    this.pinnedRunId = opts.runId;
  }

  start(): void {
    if (this.started) return;
    this.started = true;
    this.abort = new AbortController();
    void this.loop(this.abort.signal);
  }

  stop(): void {
    this.started = false;
    this.abort?.abort();
    this.abort = null;
    this.setConn("disconnected");
  }

  onFrame(fn: (f: TelemetryFrame) => void) {
    return this.frames.subscribe(fn);
  }
  onEvent(fn: (e: TestEvent) => void) {
    return this.events.subscribe(fn);
  }
  onState(fn: (s: SourceState) => void) {
    return this.states.subscribe(fn);
  }
  state(): SourceState {
    return this.conn;
  }

  private setConn(s: SourceState): void {
    if (this.conn === s) return;
    this.conn = s;
    this.states.emit(s);
  }

  /**
   * Top-level loop: pick a run, stream it to completion, then look for the
   * next one. Survives transient errors by backing off and retrying.
   */
  private async loop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      this.setConn("connecting");
      let run: PbRun | null;
      try {
        run = await this.selectRun(signal);
      } catch (err) {
        if (signal.aborted) return;
        this.setConn("error");
        await sleep(POLL_INTERVAL_MS, signal);
        continue;
      }
      if (!run) {
        // Nothing to watch yet; poll.
        this.setConn("disconnected");
        await sleep(POLL_INTERVAL_MS, signal);
        continue;
      }
      if (isTerminalState(run.state) && run.runId === this.lastTerminalId) {
        // Already showed this finished run's final frame; wait for a new or
        // active one rather than reconnecting to it every poll.
        this.setConn("disconnected");
        await sleep(POLL_INTERVAL_MS, signal);
        continue;
      }

      this.prev = null;
      try {
        await this.streamRun(run.runId, signal);
      } catch (err) {
        if (signal.aborted) return;
        this.setConn("error");
        await sleep(POLL_INTERVAL_MS, signal);
        continue;
      }
      // The stream ends only when the run reaches a terminal state, so it is
      // now finished — remember it so we don't re-stream it below.
      this.lastTerminalId = run.runId;
      if (this.pinnedRunId) {
        this.setConn("disconnected");
        return;
      }
      await sleep(POLL_INTERVAL_MS, signal);
    }
  }

  /** Returns the run to watch: the pinned one, the active one, else the most
   * recent, or null when there are none. */
  private async selectRun(signal: AbortSignal): Promise<PbRun | null> {
    const { runs } = await this.client.listRuns({}, { signal });
    if (this.pinnedRunId) return runs.find((r) => r.runId === this.pinnedRunId) ?? null;
    // Newest-first from the server; prefer a live run, else the most recent.
    const active = runs.find((r) => r.state === RunState.RUNNING || r.state === RunState.PENDING);
    return active ?? runs[0] ?? null;
  }

  private async streamRun(runId: string, signal: AbortSignal): Promise<void> {
    this.setConn("live");
    // Aggregates and events stream concurrently; both end when the run is
    // terminal. If either fails, abort the sibling so it does not keep running
    // (and get re-streamed as a duplicate) while the loop backs off and
    // re-selects.
    const child = new AbortController();
    const relay = () => child.abort();
    signal.addEventListener("abort", relay, { once: true });
    try {
      await Promise.all([
        this.streamTelemetry(runId, child.signal).catch((e) => {
          child.abort();
          throw e;
        }),
        this.streamEvents(runId, child.signal).catch((e) => {
          child.abort();
          throw e;
        }),
      ]);
    } finally {
      signal.removeEventListener("abort", relay);
    }
  }

  private async streamTelemetry(runId: string, signal: AbortSignal): Promise<void> {
    for await (const frame of this.client.runTelemetry({ runId, intervalMs: FRAME_INTERVAL_MS }, { signal })) {
      const mapped = this.mapFrame(frame);
      if (mapped) this.frames.emit(mapped);
      this.prev = frame;
    }
  }

  private async streamEvents(runId: string, signal: AbortSignal): Promise<void> {
    for await (const ev of this.client.runEvents({ runId, fromSeq: 0n }, { signal })) {
      this.events.emit(this.mapEvent(ev));
    }
  }

  /** Maps a wire RunEvent onto the dashboard's event model, assigning a
   * source-unique id (seq restarts per run and would collide). */
  private mapEvent(ev: PbEvent): TestEvent {
    return {
      id: this.nextEventId++,
      t: Number(ev.unixNano / 1_000_000n),
      severity: severityName(ev.severity),
      kind: ev.kind,
      supi: ev.supi || undefined,
      message: ev.message,
    };
  }

  /** Maps a wire frame onto the dashboard model, or null if it has no progress. */
  private mapFrame(f: PbFrame): TelemetryFrame | null {
    const lp = f.progress.case === "load" ? f.progress.value : null;
    const t = Number(f.unixNano / 1_000_000n);
    const elapsedMs = Number(f.elapsedMs);

    // Interval attach rate from the delta between successive frames — the
    // cumulative achievedRate would understate a rate that is currently high.
    let attachPerSec = lp?.achievedRate ?? 0;
    const prevLp = this.prev?.progress.case === "load" ? this.prev.progress.value : null;
    if (lp && prevLp) {
      const dt = (elapsedMs - Number(this.prev!.elapsedMs)) / 1000;
      if (dt > 0) attachPerSec = Math.max(0, (lp.succeeded - prevLp.succeeded) / dt);
    }

    const cp = attachLatency(lp);

    return {
      t,
      run: {
        runId: f.runId,
        scenario: runKindLabel(f),
        state: runStateName(f.state),
        startedAt: t - elapsedMs,
        elapsedMs,
        targetUes: null,
      },
      // A load run's attach funnel: in-flight = attempted not yet resolved.
      ues: lp
        ? {
            deregistered: 0,
            registering: Math.max(0, lp.attempted - lp.succeeded - lp.failed),
            registered: 0,
            sessionActive: lp.succeeded,
            failed: lp.failed,
          }
        : { deregistered: 0, registering: 0, registered: 0, sessionActive: 0, failed: 0 },
      rates: {
        attachPerSec,
        detachPerSec: 0,
        handoverPerSec: 0,
        attachSuccess: lp && lp.attempted > 0 ? lp.succeeded / lp.attempted : 1,
        handoverSuccess: 1,
      },
      // Not carried by the current frame; left zero rather than invented.
      throughput: { uplinkBps: 0, downlinkBps: 0, uplinkPps: 0, downlinkPps: 0 },
      cpLatency: cp,
      upLatency: null,
      perGnb: {},
    };
  }
}

/** Picks the attach (or registration) procedure latency, in ms. */
function attachLatency(lp: LoadProgress | null): LatencySummary {
  const empty = { p50: 0, p90: 0, p99: 0, max: 0 };
  if (!lp) return empty;
  const l = lp.latency.find((x) => x.procedure === "attach") ?? lp.latency.find((x) => x.procedure === "registration") ?? lp.latency[0];
  if (!l) return empty;
  return { p50: l.p50Ms, p90: l.p90Ms, p99: l.p99Ms, max: l.maxMs };
}

function severityName(s: PbSeverity): EventSeverity {
  switch (s) {
    case PbSeverity.WARN:
      return "warn";
    case PbSeverity.ERROR:
      return "error";
    default:
      return "info";
  }
}

function isTerminalState(s: RunState): boolean {
  return s === RunState.COMPLETE || s === RunState.FAILED || s === RunState.CANCELLED;
}

function runStateName(s: RunState): TelemetryFrame["run"]["state"] {
  switch (s) {
    case RunState.PENDING:
      return "starting";
    case RunState.RUNNING:
      return "running";
    case RunState.DRAINING:
      return "draining";
    case RunState.COMPLETE:
      return "complete";
    case RunState.FAILED:
    case RunState.CANCELLED:
      return "failed";
    default:
      return "idle";
  }
}

function runKindLabel(f: PbFrame): string {
  // The frame doesn't carry the run name; show the id's short form as scenario.
  return f.runId;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    // Symmetric cleanup: the timeout path removes the abort listener, and the
    // abort path clears the timer. Without this the {once} listener lingers on
    // the long-lived per-start signal on every poll, accumulating for the life
    // of the dashboard.
    const onAbort = () => {
      clearTimeout(id);
      resolve();
    };
    const id = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}
