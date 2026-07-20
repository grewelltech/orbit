/**
 * Dashboard telemetry model.
 *
 * PROVISIONAL. This is the shape the UI renders against, chosen so panels can
 * be built before the monitoring API surface is settled. It is deliberately
 * *not* generated from the protos: keeping a hand-written model here means the
 * eventual API can differ in shape without rewriting every panel — only the
 * adapter in `connect.ts` changes.
 *
 * Once the real surface lands, reconcile field-by-field and treat any mismatch
 * as a question about which side is right, not an automatic edit here.
 */

/** Lifecycle of a test run. */
export type RunState = "idle" | "starting" | "running" | "draining" | "complete" | "failed";

export interface RunStatus {
  runId: string;
  /** Scenario or profile name driving the run. */
  scenario: string;
  state: RunState;
  /** Wall-clock start, epoch ms. Null before the run begins. */
  startedAt: number | null;
  elapsedMs: number;
  /** Target UE count for the run, if the scenario declares one. */
  targetUes: number | null;
}

/**
 * UE population by state. These mirror the 5GS registration/session states the
 * gNB+UE simulator tracks; the exact set must be reconciled with the engine.
 */
export interface UeStateCounts {
  deregistered: number;
  registering: number;
  registered: number;
  /** Registered with at least one active PDU session. */
  sessionActive: number;
  failed: number;
}

/** Procedure rates, per second, over the last sampling interval. */
export interface ProcedureRates {
  attachPerSec: number;
  detachPerSec: number;
  handoverPerSec: number;
  /** Share of attempts succeeding, 0–1. */
  attachSuccess: number;
  handoverSuccess: number;
}

/** N3 user-plane throughput, bits per second. */
export interface Throughput {
  uplinkBps: number;
  downlinkBps: number;
  uplinkPps: number;
  downlinkPps: number;
}

/** Latency summary in milliseconds. Percentiles come from the engine's HDR histogram. */
export interface LatencySummary {
  p50: number;
  p90: number;
  p99: number;
  max: number;
}

/** One sampling tick — everything the dashboard needs for a frame. */
export interface TelemetryFrame {
  /** Sample timestamp, epoch ms. */
  t: number;
  run: RunStatus;
  ues: UeStateCounts;
  rates: ProcedureRates;
  throughput: Throughput;
  /** Control-plane procedure latency (registration, session establishment). */
  cpLatency: LatencySummary;
  /** User-plane round-trip latency, when a data path is up. */
  upLatency: LatencySummary | null;
  /** Per-gNB UE distribution, keyed by gNB id. */
  perGnb: Record<string, number>;
}

export type EventSeverity = "info" | "warn" | "error";

/** A discrete occurrence worth showing in the event stream. */
export interface TestEvent {
  /** Monotonic id, for stable list keys and de-duplication. */
  id: number;
  t: number;
  severity: EventSeverity;
  /** Short machine-ish category, e.g. "REGISTER", "HANDOVER", "NGAP". */
  kind: string;
  /** Subject of the event, when it concerns one UE. */
  supi?: string;
  message: string;
}

/** Connection state of the dashboard to its telemetry source. */
export type SourceState = "connecting" | "live" | "stalled" | "disconnected" | "error";
