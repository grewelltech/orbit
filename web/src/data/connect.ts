/**
 * ConnectSource — a TelemetrySource backed by the real ORBIT RunService.
 *
 * It finds a run to watch (the active one, else the most recent), streams
 * RunTelemetry, and maps each TelemetryFrame onto the dashboard's frame model.
 * When no run is active it polls until one appears, so a dashboard left open
 * lights up as soon as a run starts.
 *
 * Honest-scope note, by run kind:
 *   - LOAD frames carry attach aggregates (attempted/succeeded/failed, rate,
 *     procedure latency) and the per-gNB spread. They feed the UE funnel, the
 *     attach-rate, CP-latency and per-gNB panels. A load run drives no user
 *     plane, so its throughput stays zero.
 *   - FLEET frames carry the standing population, mobility outcomes, and the
 *     N3 user-plane counters summed across the fleet's tunnels — so the
 *     throughput panel is live for a fleet run. Rates arrive already derived
 *     per-interval by the server.
 * User-plane LATENCY comes from a fleet run's latency probe when the scenario
 * configures one, and stays null otherwise — never a zero that would read as
 * an instant data path.
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
  type FleetProgress,
  type Quantiles as PbQuantiles,
} from "@/gen/orbit/v1/run_pb";
import { Emitter, type TelemetrySource } from "./source";
import type {
  ProcedureLatencySummary,
  EventSeverity,
  Quantiles,
  SourceState,
  TelemetryFrame,
  TestEvent,
} from "./types";

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
        // listRuns failed: this is the real "cannot reach the server" case.
        if (signal.aborted) return;
        this.setConn("disconnected");
        await sleep(POLL_INTERVAL_MS, signal);
        continue;
      }
      if (!run) {
        // Nothing to watch yet; poll. listRuns just succeeded, so the server
        // is reachable — saying "disconnected" here would report a fault that
        // does not exist for as long as the testbed sits idle.
        this.setConn("connected");
        await sleep(POLL_INTERVAL_MS, signal);
        continue;
      }
      if (isTerminalState(run.state) && run.runId === this.lastTerminalId) {
        // Already showed this finished run's final frame; wait for a new or
        // active one rather than reconnecting to it every poll. Still
        // connected — there is simply nothing new to stream.
        this.setConn("connected");
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
        // The pinned run has finished and nothing else will be streamed, but
        // the server is still there.
        this.setConn("connected");
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
    // fromSeq 0: replay the run's retained series before following it live, so
    // a reload — or attaching to a run already under way — starts with its
    // history rather than an empty chart. The server states any frames it could
    // not cover as droppedBefore on the first one.
    for await (const frame of this.client.runTelemetry(
      { runId, intervalMs: FRAME_INTERVAL_MS, fromSeq: 0n },
      { signal },
    )) {
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
    const fp = f.progress.case === "fleet" ? f.progress.value : null;
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

    // Handovers per second, likewise from the delta between frames.
    let handoverPerSec = 0;
    const prevFp = this.prev?.progress.case === "fleet" ? this.prev.progress.value : null;
    if (fp && prevFp) {
      const dt = (elapsedMs - Number(this.prev!.elapsedMs)) / 1000;
      if (dt > 0) handoverPerSec = Math.max(0, (fp.handovers - prevFp.handovers) / dt);
    }

    // A fleet run reports a standing population rather than a stream of
    // attempts, so its attach rate is the delta in `attached` between frames —
    // real during the attach phase, and correctly zero once it is done.
    if (fp && prevFp) {
      const dt = (elapsedMs - Number(this.prev!.elapsedMs)) / 1000;
      if (dt > 0) attachPerSec = Math.max(0, (fp.attached - prevFp.attached) / dt);
    }

    const cp = attachLatency(lp, fp);

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
      // The attach funnel. A load run reports attempts (in-flight = attempted
      // not yet resolved); a fleet run reports a standing population, whose
      // attached UEs all hold a session.
      ues: fleetOrLoadUes(lp, fp),
      rates: {
        attachPerSec,
        detachPerSec: 0,
        handoverPerSec,
        attachSuccess: attachSuccessRatio(lp, fp),
        handoverSuccess:
          fp && fp.handovers + fp.handoverErrors > 0
            ? fp.handovers / (fp.handovers + fp.handoverErrors)
            : 1,
      },
      // A fleet run's N3 user-plane counters. The server derives the rates
      // from consecutive samples on this stream, so they are already
      // per-interval; a load run carries none and stays zero rather than
      // showing invented data.
      throughput: fp
        ? {
            uplinkBps: fp.uplinkBps,
            downlinkBps: fp.downlinkBps,
            uplinkPps: fp.uplinkPps,
            downlinkPps: fp.downlinkPps,
          }
        : { uplinkBps: 0, downlinkBps: 0, uplinkPps: 0, downlinkPps: 0 },
      cpLatency: cp,
      // User-plane RTT, from ICMP echoes over the UEs' own N3 data paths.
      // Absent unless the scenario configures a latency probe — null keeps
      // "not measured" distinct from "measured as zero".
      upLatency: fp?.upLatency
        ? {
            p50: fp.upLatency.p50Ms,
            p90: fp.upLatency.p90Ms,
            p99: fp.upLatency.p99Ms,
            max: fp.upLatency.maxMs,
          }
        : null,
      // Where the population sits: attached UEs per gNB, for either kind.
      perGnb: Object.fromEntries(
        (lp?.perGnb ?? fp?.perGnb ?? []).map((g) => [g.gnb, g.succeeded]),
      ),
      mobility: { handovers: fp?.handovers ?? 0, failed: fp?.handoverErrors ?? 0 },
      cohorts: (fp?.cohorts ?? []).map((c) => ({
        name: c.name,
        app: c.app,
        ues: c.ues,
        failed: c.failed,
        elapsedMs: Number(c.elapsedMs),
        mos: quantiles(c.mos),
        ttfbMs: quantiles(c.ttfbMs),
        goodputMbps: quantiles(c.goodputMbps),
        stallTimeMs: quantiles(c.stallTimeMs),
        rebufferRatio: quantiles(c.rebufferRatio),
        bitrateKbps: quantiles(c.bitrateKbps),
        startupMs: quantiles(c.startupMs),
        farEnd: c.farEnd
          ? {
              available: c.farEnd.available,
              reason: c.farEnd.reason,
              bytes: Number(c.farEnd.bytes),
              packets: Number(c.farEnd.packets),
              bitsPerSec: c.farEnd.bitsPerSec,
              requests: Number(c.farEnd.requests),
              errors: Number(c.farEnd.errors),
            }
          : null,
      })),
      flows: (fp?.flows ?? []).map((f) => ({
        supi: f.supi,
        cohort: f.cohort,
        app: f.app,
        peer: f.peer,
        gnb: f.gnb,
        elapsedMs: Number(f.elapsedMs),
        uplinkBps: f.uplinkBps,
        downlinkBps: f.downlinkBps,
        uplinkBytes: Number(f.uplinkBytes),
        downlinkBytes: Number(f.downlinkBytes),
      })),
      flowsTotal: fp?.flowsTotal ?? 0,
    };
  }
}

/**
 * An absent distribution stays null rather than becoming zeros: the server
 * omits families the cohort's app does not produce, and collapsing that to 0
 * would present "not measured" as "measured perfect".
 */
function quantiles(q: PbQuantiles | undefined): Quantiles | null {
  return q ? { p5: q.p5, p50: q.p50, p95: q.p95 } : null;
}

/** The UE funnel for whichever kind the frame carries. */
function fleetOrLoadUes(lp: LoadProgress | null, fp: FleetProgress | null) {
  if (lp) {
    return {
      deregistered: 0,
      registering: Math.max(0, lp.attempted - lp.succeeded - lp.failed),
      registered: 0,
      sessionActive: lp.succeeded,
      failed: lp.failed,
    };
  }
  if (fp) {
    // A fleet UE is attached with its PDU session up, so the population sits
    // in sessionActive; nothing is mid-attach once the attach phase is done.
    return {
      deregistered: 0,
      registering: 0,
      registered: 0,
      sessionActive: fp.attached,
      failed: fp.attachFailed,
    };
  }
  return { deregistered: 0, registering: 0, registered: 0, sessionActive: 0, failed: 0 };
}

/** Attach success ratio for whichever kind the frame carries. */
function attachSuccessRatio(lp: LoadProgress | null, fp: FleetProgress | null): number {
  if (lp) return lp.attempted > 0 ? lp.succeeded / lp.attempted : 1;
  if (fp) {
    const total = fp.attached + fp.attachFailed;
    return total > 0 ? fp.attached / total : 1;
  }
  return 1;
}

/**
 * Picks the procedure whose latency the summary tile reports, in ms.
 *
 * Priority matters: on a mobility run handovers are the live control-plane
 * work and attach finished minutes ago, so handovers win. On a static run
 * there are none and attach is all there is.
 *
 * The chosen name travels WITH the numbers. Reporting the value alone let one
 * tile mean "handover latency" in one scenario and "attach latency, measured
 * during the opening burst and unchanged since" in another.
 */
/**
 * The headline control-plane latency. A load run measures attach; a fleet run
 * measures attach during its attach phase and then handovers for the rest of
 * the run, so the panel stays live instead of going flat once the population
 * is up. Preference order picks the procedure that is actually happening.
 */
function attachLatency(
  lp: LoadProgress | null,
  fp: FleetProgress | null,
): ProcedureLatencySummary {
  const empty = { procedure: "", count: 0, p50: 0, p90: 0, p99: 0, max: 0 };
  const rows = lp?.latency ?? fp?.latency ?? [];
  if (rows.length === 0) return empty;
  const pick = (name: string) => rows.find((x) => x.procedure === name);
  const l =
    pick("handover_xn") ?? pick("handover_n2") ?? pick("attach") ?? pick("registration") ?? rows[0];
  if (!l) return empty;
  return {
    procedure: l.procedure,
    count: Number(l.count),
    p50: l.p50Ms,
    p90: l.p90Ms,
    p99: l.p99Ms,
    max: l.maxMs,
  };
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
