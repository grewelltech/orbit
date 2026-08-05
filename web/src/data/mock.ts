/**
 * MockSource — synthetic ORBIT telemetry for building and demoing the
 * dashboard without a live core.
 *
 * The numbers are shaped to be *plausible*, not authoritative: an attach ramp
 * that saturates, occasional registration failures, handovers once a UE
 * population exists, and throughput that tracks session count. Magnitudes are
 * kept inside SD-Core's documented envelope (~5,000 UEs at ~10 attach/s) so
 * the layout is exercised at realistic scale rather than fantasy scale.
 *
 * Seeded so a reload reproduces the same run — reproducible screenshots, and
 * a stable target when tuning panels.
 */
import { Emitter, type TelemetrySource } from "./source";
import type {
  LatencySummary,
  ProcedureRates,
  RunState,
  SourceState,
  TelemetryFrame,
  TestEvent,
  Throughput,
  UeStateCounts,
} from "./types";

/** mulberry32 — small, fast, seedable PRNG. */
function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const GNBS = ["gnb-01", "gnb-02", "gnb-03", "gnb-04"];
const TICK_MS = 500;

export interface MockOptions {
  seed?: number;
  targetUes?: number;
  /** Attach attempts per second once the ramp is underway. */
  attachRate?: number;
  scenario?: string;
}

export class MockSource implements TelemetrySource {
  readonly name = "mock";

  private readonly frames = new Emitter<TelemetryFrame>();
  private readonly events = new Emitter<TestEvent>();
  private readonly states = new Emitter<SourceState>();

  private timer: ReturnType<typeof setInterval> | null = null;
  private conn: SourceState = "disconnected";

  private readonly rand: () => number;
  private readonly target: number;
  private readonly attachRate: number;
  private readonly scenario: string;

  private t0 = 0;
  private eventId = 0;
  private runState: RunState = "idle";

  // Population, carried across ticks.
  private registering = 0;
  private registered = 0;
  private sessionActive = 0;
  private failed = 0;
  private attachedTotal = 0;
  private handoverTotal = 0;
  private handoverFailed = 0;
  private perGnb: Record<string, number> = Object.fromEntries(GNBS.map((g) => [g, 0]));

  constructor(opts: MockOptions = {}) {
    this.rand = rng(opts.seed ?? 0x0b17);
    this.target = opts.targetUes ?? 1200;
    this.attachRate = opts.attachRate ?? 10;
    this.scenario = opts.scenario ?? "fleet-attach-soak";
  }

  start(): void {
    if (this.timer) return;
    this.setConn("connecting");
    this.t0 = Date.now();
    this.runState = "starting";

    // A brief connecting state makes the header's live indicator meaningful
    // rather than decorative.
    setTimeout(() => {
      this.setConn("live");
      this.runState = "running";
      this.emitEvent("info", "RUN", `run started · scenario ${this.scenario} · target ${this.target} UEs`);
    }, 400);

    this.timer = setInterval(() => this.tick(), TICK_MS);
  }

  stop(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
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
    this.conn = s;
    this.states.emit(s);
  }

  private emitEvent(severity: TestEvent["severity"], kind: string, message: string, supi?: string): void {
    this.events.emit({
      id: ++this.eventId,
      t: Date.now(),
      severity,
      kind,
      message,
      ...(supi ? { supi } : {}),
    });
  }

  private supi(n: number): string {
    // Test IMSI block from the project's testbed coordinates.
    return `208930100007${String(500 + (n % 100)).padStart(3, "0")}`;
  }

  private tick(): void {
    if (this.runState !== "running") {
      // Still emit frames while starting so panels show a populated shell.
      if (this.runState !== "starting") return;
    }

    const dt = TICK_MS / 1000;
    const now = Date.now();

    // ── attach ramp: rate holds until the population nears target, then eases
    const remaining = this.target - (this.registered + this.sessionActive + this.registering);
    const demand = Math.max(0, Math.min(this.attachRate * dt, remaining));
    const starting = Math.floor(demand + (this.rand() < demand % 1 ? 1 : 0));
    this.registering += starting;

    // ── registering → registered, with a small failure rate
    const completing = Math.min(this.registering, Math.ceil(this.registering * 0.55));
    let succeeded = 0;
    for (let i = 0; i < completing; i++) {
      if (this.rand() < 0.982) succeeded++;
      else {
        this.failed++;
        if (this.rand() < 0.25) {
          this.emitEvent("warn", "REGISTER", "registration rejected · 5GMM cause #22 congestion", this.supi(this.failed));
        }
      }
    }
    this.registering -= completing;
    this.registered += succeeded;
    this.attachedTotal += succeeded;

    // Assign new registrations across gNBs.
    for (let i = 0; i < succeeded; i++) {
      const g = GNBS[Math.floor(this.rand() * GNBS.length)] as string;
      this.perGnb[g] = (this.perGnb[g] ?? 0) + 1;
    }

    // ── PDU session establishment trails registration
    const promoting = Math.min(this.registered, Math.ceil(this.registered * 0.35));
    this.registered -= promoting;
    this.sessionActive += promoting;

    // ── handovers, once there is a population to move
    let handovers = 0;
    if (this.sessionActive > 50) {
      handovers = Math.floor(this.rand() * 4);
      for (let i = 0; i < handovers; i++) {
        this.handoverTotal++;
        const from = GNBS[Math.floor(this.rand() * GNBS.length)] as string;
        let to = GNBS[Math.floor(this.rand() * GNBS.length)] as string;
        if (to === from) to = GNBS[(GNBS.indexOf(from) + 1) % GNBS.length] as string;
        if ((this.perGnb[from] ?? 0) > 0) {
          this.perGnb[from] = (this.perGnb[from] as number) - 1;
          this.perGnb[to] = (this.perGnb[to] ?? 0) + 1;
        }
        if (this.rand() < 0.03) {
          this.handoverFailed++;
          this.emitEvent("error", "HANDOVER", `N2 handover failed ${from} → ${to} · no PathSwitch response`, this.supi(this.handoverTotal));
        } else if (this.rand() < 0.06) {
          this.emitEvent("info", "HANDOVER", `N2 handover ${from} → ${to} complete`, this.supi(this.handoverTotal));
        }
      }
    }

    // Occasional control-plane chatter so the event stream isn't only failures.
    if (this.rand() < 0.05) {
      this.emitEvent("info", "NGAP", `NG Setup response · AMF served GUAMI 208-93 · TAC 1`);
    }

    // ── throughput scales with active sessions, with jitter
    const perUeUlBps = 240_000 * (0.85 + this.rand() * 0.3);
    const perUeDlBps = 1_100_000 * (0.85 + this.rand() * 0.3);
    const throughput: Throughput = {
      uplinkBps: this.sessionActive * perUeUlBps,
      downlinkBps: this.sessionActive * perUeDlBps,
      uplinkPps: (this.sessionActive * perUeUlBps) / (1200 * 8),
      downlinkPps: (this.sessionActive * perUeDlBps) / (1400 * 8),
    };

    // ── latency degrades as the population grows (queueing at the core)
    const load = Math.min(1, this.sessionActive / Math.max(1, this.target));
    const cpLatency = this.latency(18 + load * 55, 1.9);
    const upLatency = this.sessionActive > 0 ? this.latency(6 + load * 14, 1.5) : null;

    const ues: UeStateCounts = {
      deregistered: Math.max(0, this.target - this.registering - this.registered - this.sessionActive - this.failed),
      registering: this.registering,
      registered: this.registered,
      sessionActive: this.sessionActive,
      failed: this.failed,
    };

    const rates: ProcedureRates = {
      attachPerSec: succeeded / dt,
      detachPerSec: 0,
      handoverPerSec: handovers / dt,
      attachSuccess: this.attachedTotal / Math.max(1, this.attachedTotal + this.failed),
      handoverSuccess: 1 - this.handoverFailed / Math.max(1, this.handoverTotal),
    };

    // Run completes once the population is established.
    if (this.runState === "running" && ues.deregistered === 0 && this.registering === 0 && this.registered === 0) {
      this.runState = "complete";
      this.emitEvent("info", "RUN", `run complete · ${this.sessionActive} sessions established`);
    }

    this.frames.emit({
      t: now,
      run: {
        runId: "run-7f3a91",
        scenario: this.scenario,
        state: this.runState,
        startedAt: this.t0,
        elapsedMs: now - this.t0,
        targetUes: this.target,
      },
      ues,
      rates,
      throughput,
      cpLatency,
      upLatency,
      perGnb: { ...this.perGnb },
      // The mock does not synthesise cohort quality or per-flow rows: they
      // would be plausible-looking numbers with nothing behind them, which is
      // exactly what the real source is careful not to report.
      mobility: { handovers: 0, failed: 0 },
      cohorts: [],
      flows: [],
      flowsTotal: 0,
    });
  }

  /** Long-tailed latency summary around a median, in milliseconds. */
  private latency(p50: number, spread: number): LatencySummary {
    const jitter = 0.9 + this.rand() * 0.2;
    const m = p50 * jitter;
    return {
      p50: m,
      p90: m * spread,
      p99: m * spread * (1.6 + this.rand() * 0.5),
      max: m * spread * (2.4 + this.rand() * 1.4),
    };
  }
}
