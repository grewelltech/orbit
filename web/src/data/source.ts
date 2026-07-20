/**
 * TelemetrySource — the seam between the dashboard and wherever data comes from.
 *
 * Every panel renders against this interface, so the mock generator and the
 * eventual Connect client are interchangeable. When the monitoring API lands,
 * implementing this interface over it is the only integration work; no panel
 * changes.
 */
import type { SourceState, TelemetryFrame, TestEvent } from "./types";

export interface TelemetrySource {
  /** Human label for the source, shown in the header. */
  readonly name: string;

  /** Begins streaming. Idempotent. */
  start(): void;

  /** Stops streaming and releases resources. Idempotent. */
  stop(): void;

  /**
   * Subscribes to sampling ticks. Returns an unsubscribe function.
   * Implementations deliver at a steady cadence regardless of upstream rate —
   * coalescing bursts rather than forwarding every underlying update.
   */
  onFrame(fn: (frame: TelemetryFrame) => void): () => void;

  /** Subscribes to discrete events. Returns an unsubscribe function. */
  onEvent(fn: (event: TestEvent) => void): () => void;

  /** Subscribes to connection-state changes. Returns an unsubscribe function. */
  onState(fn: (state: SourceState) => void): () => void;

  /** Current connection state. */
  state(): SourceState;
}

/** Minimal multi-subscriber fan-out shared by source implementations. */
export class Emitter<T> {
  private readonly subs = new Set<(v: T) => void>();

  subscribe(fn: (v: T) => void): () => void {
    this.subs.add(fn);
    return () => {
      this.subs.delete(fn);
    };
  }

  emit(v: T): void {
    // Iterating a copy so a subscriber unsubscribing mid-emit can't skip
    // another subscriber.
    for (const fn of [...this.subs]) fn(v);
  }

  get count(): number {
    return this.subs.size;
  }

  clear(): void {
    this.subs.clear();
  }
}
