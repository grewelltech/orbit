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
// Retention is bounded regardless of run length; a late or slow subscriber is
// told how many events before its resume point were evicted (DroppedBefore),
// so loss is detectable rather than silent. Publish never blocks: a full
// subscriber channel drops the live copy, but the event stays in the ring and
// a terminal-time reconciliation (see the server handler) recovers it.
type eventRing struct {
	mu sync.Mutex
	// buf is circular: it grows to cap during fill (head stays 0), and
	// thereafter the oldest slot is overwritten in place. Shifting the whole
	// buffer down on each eviction — the obvious implementation — costs O(cap)
	// per event, which measured 1.2µs at cap 500 and 22µs at cap 10000 and made
	// the ring's size a performance decision. Overwriting in place is O(1), so
	// capacity can be chosen for how much history is useful.
	buf     []RunEvent
	head    int // index of the oldest retained event
	n       int // retained count, <= cap
	cap     int
	nextSeq uint64
	subs    map[int]chan RunEvent
	nextSub int
}

// maxSubChanBuffer bounds a subscriber's channel independently of the ring.
// Sizing it to cap would put a copy of the whole ring behind every subscriber
// (~1 MB each at cap 10000); a subscriber that falls further behind than this
// drops observably and recovers from the ring at terminal, which is the
// designed behaviour anyway.
const maxSubChanBuffer = 1024

func newEventRing(capacity int) *eventRing {
	if capacity < 1 {
		capacity = 1
	}
	return &eventRing{cap: capacity, subs: make(map[int]chan RunEvent)}
}

// at returns the i-th retained event, oldest-first. Callers hold r.mu.
func (r *eventRing) at(i int) RunEvent { return r.buf[(r.head+i)%r.cap] }

// emit assigns the next seq, retains the event (evicting the oldest when full),
// and fans it out live. Safe for concurrent callers.
//
// The fan-out happens under the lock: the sends are non-blocking (a full
// channel drops), so the lock is never held on a blocking operation, and
// serializing against subscribe/cancel means a send can never race a close —
// a subscriber removed from the map before emit runs is simply not sent to.
func (r *eventRing) emit(severity, kind, supi, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := RunEvent{
		Seq: r.nextSeq, Time: time.Now(),
		Severity: severity, Kind: kind, SUPI: supi, Message: message,
	}
	r.nextSeq++
	if r.n < r.cap {
		// Filling: head is still 0, so the next slot is simply the end.
		r.buf = append(r.buf, ev)
		r.n++
	} else {
		// Full: overwrite the oldest and advance. O(1) regardless of capacity.
		r.buf[r.head] = ev
		r.head = (r.head + 1) % r.cap
	}
	for _, ch := range r.subs {
		select {
		case ch <- ev:
		default: // slow subscriber; recovered at terminal from the retained ring
		}
	}
}

// snapshotSince returns retained events with Seq >= fromSeq, oldest-first. Used
// to recover events a slow subscriber's channel dropped, which are still in the
// ring.
func (r *eventRing) snapshotSince(fromSeq uint64) []RunEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RunEvent
	for i := 0; i < r.n; i++ {
		if ev := r.at(i); ev.Seq >= fromSeq {
			out = append(out, ev)
		}
	}
	return out
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
	for i := 0; i < r.n; i++ {
		if ev := r.at(i); ev.Seq >= fromSeq {
			backlog = append(backlog, ev)
		}
	}
	// How many the caller wanted (from fromSeq up to the first retained seq)
	// are gone. The earliest retained is the oldest slot when non-empty.
	var droppedBefore uint64
	if r.n > 0 {
		earliest := r.at(0).Seq
		if earliest > fromSeq {
			droppedBefore = earliest - fromSeq
		}
	} else if r.nextSeq > fromSeq {
		droppedBefore = r.nextSeq - fromSeq
	}

	// Buffer the channel to the ring capacity: a subscriber that keeps up never
	// drops, and one that falls behind drops (observably) rather than blocking
	// emit.
	ch := make(chan RunEvent, min(r.cap, maxSubChanBuffer))
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
