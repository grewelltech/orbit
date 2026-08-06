/**
 * useTelemetry — ingests a TelemetrySource into bounded buffers and publishes
 * to React at a rate the renderer can actually sustain.
 *
 * The ingest path (source callback → ring buffer) runs at stream rate and
 * touches no React state. A separate animation-frame loop publishes a snapshot
 * only when something changed, so render cost is bounded by frame rate rather
 * than by event rate. Panels that need every sample subscribe imperatively via
 * `subscribeFrames` and push straight into their chart.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Ring } from "@/data/ring";
import type { TelemetrySource } from "@/data/source";
import type { SourceState, TelemetryFrame, TestEvent } from "@/data/types";

/** ~30 minutes at 2 Hz; bounded regardless of run length. */
const HISTORY_CAPACITY = 3600;
// Matches the server's per-run ring (engine.DefaultRunEventCap): holding less
// would silently truncate history the server is still willing to replay.
const EVENT_CAPACITY = 2000;

export interface Telemetry {
  /** Most recent frame, republished at frame rate. */
  latest: TelemetryFrame | null;
  /** Bounded frame history. Stable reference — safe to read imperatively. */
  history: Ring<TelemetryFrame>;
  /** Newest-first event list, bounded. */
  events: TestEvent[];
  state: SourceState;
  /** Per-sample subscription that bypasses React entirely. */
  subscribeFrames: (fn: (f: TelemetryFrame) => void) => () => void;
  /**
   * Bumped when the history ring is replaced wholesale — restored from a
   * refresh, or discarded because it belonged to a different run. Panels seed
   * from the ring on mount, so they must reseed when it changes underneath
   * them rather than carrying another run's points.
   */
  historyEpoch: number;
  /**
   * Drops the events held for display. Local to this view: the server's ring
   * is append-only and its sequence numbers are what make loss detectable, so
   * clearing here must not mean deleting evidence for everyone. A reconnect
   * replays whatever the server still retains.
   */
  clearEvents: () => void;
}

export function useTelemetry(source: TelemetrySource): Telemetry {
  const history = useMemo(() => new Ring<TelemetryFrame>(HISTORY_CAPACITY), []);
  const [historyEpoch, setHistoryEpoch] = useState(0);
  // The run the ring currently holds, so switching runs clears it rather than
  // splicing two runs into one series.
  const heldRunId = useRef<string | undefined>(undefined);
  const eventRing = useMemo(() => new Ring<TestEvent>(EVENT_CAPACITY), []);

  const [latest, setLatest] = useState<TelemetryFrame | null>(null);
  const [events, setEvents] = useState<TestEvent[]>([]);
  const [state, setState] = useState<SourceState>(source.state());

  // Direct subscribers, invoked on the ingest path.
  const passthrough = useRef(new Set<(f: TelemetryFrame) => void>());

  const dirtyFrame = useRef(false);
  const dirtyEvents = useRef(false);

  const subscribeFrames = useCallback((fn: (f: TelemetryFrame) => void) => {
    passthrough.current.add(fn);
    return () => {
      passthrough.current.delete(fn);
    };
  }, []);

  useEffect(() => {
    const offFrame = source.onFrame((f) => {
      // Frames from a different run are a new series, not a continuation of
      // the one on screen.
      if (heldRunId.current !== undefined && heldRunId.current !== f.run.runId) {
        history.clear();
        setHistoryEpoch((n) => n + 1);
      }
      heldRunId.current = f.run.runId;
      history.push(f);
      dirtyFrame.current = true;
      for (const fn of passthrough.current) fn(f);
    });

    const offEvent = source.onEvent((e) => {
      eventRing.push(e);
      dirtyEvents.current = true;
    });

    const offState = source.onState(setState);

    // Publish loop: at most one React update per animation frame, and none at
    // all while nothing is arriving.
    let raf = requestAnimationFrame(function publish() {
      if (dirtyFrame.current) {
        dirtyFrame.current = false;
        const last = history.last();
        if (last) setLatest(last);
      }
      if (dirtyEvents.current) {
        dirtyEvents.current = false;
        setEvents(eventRing.toArray().reverse());
      }
      raf = requestAnimationFrame(publish);
    });

    source.start();

    return () => {
      cancelAnimationFrame(raf);
      offFrame();
      offEvent();
      offState();
      source.stop();
    };
  }, [source, history, eventRing]);

  const clearEvents = useCallback(() => {
    eventRing.clear();
    setEvents([]);
  }, [eventRing]);


  return { latest, history, events, state, subscribeFrames, clearEvents, historyEpoch };
}
