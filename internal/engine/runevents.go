package engine

import (
	"sync"
	"time"
)

// RunEvent is one discrete occurrence during a run. Seq is monotonic within the
// run, so a consumer can detect a gap (events it never saw) even though it
// cannot recover them — the honest property a bounded ring can offer (ADR-0006).
type RunEvent struct {
	Seq      uint64
	Time     time.Time
	Severity string // "info" | "warn" | "error"
	Kind     string // "RUN" | "ATTACH" | "SLO" | ...
	SUPI     string // empty for run-scoped events
	Message  string
}

// RunEventFunc records a run event. The registry binds one per run and passes
// it to the launcher, so a run's execution can report occurrences without
// knowing about the ring, sequencing, or fan-out.
type RunEventFunc func(severity, kind, supi, message string)

// eventRing is a per-run bounded, sequenced event log with live fan-out.
//
// Retention is bounded regardless of run length; the oldest events are evicted
// and counted, so a late or slow subscriber is told how many it missed rather
// than silently under-reporting. Publish never blocks: a full subscriber
// channel drops, which the seq/dropped accounting then makes visible.
type eventRing struct {
	mu      sync.Mutex
	buf     []RunEvent // retained, oldest-first
	cap     int
	nextSeq uint64
	dropped uint64 // evicted from the front, never delivered to a late subscriber
	subs    map[int]chan RunEvent
	nextSub int
}

func newEventRing(capacity int) *eventRing {
	if capacity < 1 {
		capacity = 1
	}
	return &eventRing{cap: capacity, subs: make(map[int]chan RunEvent)}
}

// emit assigns the next seq, retains the event (evicting the oldest when full),
// and fans it out live. Safe for concurrent callers.
func (r *eventRing) emit(severity, kind, supi, message string) {
	r.mu.Lock()
	ev := RunEvent{
		Seq: r.nextSeq, Time: time.Now(),
		Severity: severity, Kind: kind, SUPI: supi, Message: message,
	}
	r.nextSeq++
	r.buf = append(r.buf, ev)
	if len(r.buf) > r.cap {
		// Evict oldest. Shift rather than reslice so the backing array does not
		// grow unbounded across a long run.
		copy(r.buf, r.buf[1:])
		r.buf = r.buf[:r.cap]
		r.dropped++
	}
	subs := make([]chan RunEvent, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // slow subscriber; it will observe the gap via seq
		}
	}
}

// EventSubscription is a live event feed plus the backlog available at subscribe
// time, taken atomically so no event is missed or duplicated across the seam.
type EventSubscription struct {
	// Backlog holds retained events with Seq >= fromSeq, oldest-first.
	Backlog []RunEvent
	// DroppedBefore is how many events before the first delivered one were
	// evicted — i.e. the caller's fromSeq pointed into already-evicted history.
	DroppedBefore uint64
	Ch            <-chan RunEvent
	cancel        func()
}

// subscribeFrom registers a live subscriber and returns the current backlog in
// one locked step, so an event emitted concurrently is either in the backlog or
// on the channel, never both and never lost.
func (r *eventRing) subscribeFrom(fromSeq uint64) *EventSubscription {
	r.mu.Lock()
	defer r.mu.Unlock()

	var backlog []RunEvent
	for _, ev := range r.buf {
		if ev.Seq >= fromSeq {
			backlog = append(backlog, ev)
		}
	}
	// How many the caller wanted (from fromSeq up to the first retained seq)
	// are gone. earliestRetained is buf[0].Seq when non-empty.
	var droppedBefore uint64
	if len(r.buf) > 0 {
		earliest := r.buf[0].Seq
		if earliest > fromSeq {
			droppedBefore = earliest - fromSeq
		}
	} else if r.nextSeq > fromSeq {
		droppedBefore = r.nextSeq - fromSeq
	}

	// Buffer the channel to the ring capacity: a subscriber that keeps up never
	// drops, and one that falls behind drops (observably) rather than blocking
	// emit.
	ch := make(chan RunEvent, r.cap)
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch

	cancel := func() {
		r.mu.Lock()
		if _, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(ch)
		}
		r.mu.Unlock()
	}
	return &EventSubscription{Backlog: backlog, DroppedBefore: droppedBefore, Ch: ch, cancel: cancel}
}

// close releases the subscription's channel and unsubscribes it.
func (s *EventSubscription) Close() { s.cancel() }
