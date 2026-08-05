/**
 * EventStream — the running log of discrete occurrences during a run.
 *
 * Virtualized: a soak test produces far more rows than the DOM should hold, and
 * the list is the one place where unbounded growth would otherwise be visible.
 * Auto-follow releases as soon as the operator scrolls away, so inspecting an
 * older failure isn't fought by incoming rows.
 */
import { useEffect, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Panel } from "@/components/Panel";
import type { EventSeverity, TestEvent } from "@/data/types";
import { clock } from "@/lib/format";

const SEVERITY_INK: Record<EventSeverity, string> = {
  info: "var(--o-ink-3)",
  warn: "var(--o-warn)",
  error: "var(--o-error)",
};

const ROW_HEIGHT = 22;

export interface EventStreamProps {
  /** Newest-first. */
  events: TestEvent[];
  /** Drops the events held for display; the server's ring is untouched. */
  onClear?: () => void;
  className?: string;
}

export function EventStream({ events, onClear, className }: EventStreamProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [follow, setFollow] = useState(true);

  const virtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
  });

  // Newest-first means following is pinning to the top.
  useEffect(() => {
    if (follow && scrollRef.current) scrollRef.current.scrollTop = 0;
  }, [events.length, follow]);

  return (
    <Panel
      title="Event stream"
      live
      flush
      className={className}
      meta={
        <div className="flex items-center gap-1.5">
        {onClear && (
          <button
            type="button"
            onClick={onClear}
            disabled={events.length === 0}
            className="o-label cursor-pointer border px-1.5 py-0.5 transition-colors disabled:cursor-default disabled:opacity-40"
            style={{
              color: "var(--o-ink-3)",
              borderColor: "var(--o-border)",
              borderRadius: "var(--o-radius)",
              transitionDuration: "var(--o-dur-fast)",
            }}
            title="Clear the events shown here. The server keeps its own record."
          >
            clear
          </button>
        )}
        <button
          type="button"
          onClick={() => setFollow((f) => !f)}
          className="o-label cursor-pointer border px-1.5 py-0.5 transition-colors"
          style={{
            color: follow ? "var(--o-accent)" : "var(--o-ink-3)",
            borderColor: follow ? "var(--o-border-accent)" : "var(--o-border)",
            borderRadius: "var(--o-radius)",
            transitionDuration: "var(--o-dur-fast)",
          }}
          aria-pressed={follow}
        >
          {follow ? "following" : "paused"}
        </button>
        </div>
      }
    >
      <div
        ref={scrollRef}
        className="h-full min-h-0 overflow-y-auto"
        onWheel={() => setFollow(false)}
      >
        {events.length === 0 ? (
          <p className="o-label px-3 py-3">awaiting events</p>
        ) : (
          <div style={{ height: virtualizer.getTotalSize(), position: "relative" }}>
            {virtualizer.getVirtualItems().map((row) => {
              const e = events[row.index] as TestEvent;
              return (
                <div
                  key={e.id}
                  className="absolute inset-x-0 flex items-center gap-2 px-3"
                  style={{ height: ROW_HEIGHT, transform: `translateY(${row.start}px)` }}
                >
                  <span className="o-num shrink-0" style={{ fontSize: "var(--o-text-2xs)", color: "var(--o-ink-3)" }}>
                    {clock(e.t)}
                  </span>
                  <span
                    className="o-num w-[4.5rem] shrink-0 truncate"
                    style={{ fontSize: "var(--o-text-2xs)", color: SEVERITY_INK[e.severity], letterSpacing: "0.06em" }}
                  >
                    {e.kind}
                  </span>
                  {e.supi && (
                    <span className="o-num hidden shrink-0 md:inline" style={{ fontSize: "var(--o-text-2xs)", color: "var(--o-ink-3)" }}>
                      {e.supi}
                    </span>
                  )}
                  <span
                    className="truncate"
                    style={{
                      fontSize: "var(--o-text-2xs)",
                      fontFamily: "var(--o-font-mono)",
                      color: e.severity === "info" ? "var(--o-ink-2)" : SEVERITY_INK[e.severity],
                    }}
                  >
                    {e.message}
                  </span>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </Panel>
  );
}
