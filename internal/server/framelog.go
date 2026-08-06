package server

import (
	"sync"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// frameLog is a run's retained telemetry series: a bounded ring of frames with
// live fan-out, the same shape as the engine's event ring and for the same
// reason — a subscriber that arrives late, or comes back after a reload, can
// recover what it missed instead of starting from an empty chart.
//
// Frames were previously generated per subscriber at each stream's own cadence,
// which meant there was no series to retain: two viewers of one run saw
// different samples, and a finished run had nothing to replay at all. One
// sampler now produces the canonical series and everyone reads it.
//
// Retained frames are TRIMMED of their flow list. At a hundred flow rows per
// frame that list dominates the memory a long run would hold, and nothing reads
// it from history — the charts read scalars, and the flow table shows only the
// latest frame, which any subscriber receives within a sampling interval of
// attaching.
type frameLog struct {
	mu   sync.Mutex
	buf  []*orbitv1.TelemetryFrame
	head int // index of the oldest retained frame
	n    int
	cap  int

	nextSeq uint64
	subs    map[int]chan *orbitv1.TelemetryFrame
	nextSub int
	closed  bool
	// sampling guards against starting a second sampler for the same run.
	sampling bool
}

// DefaultFrameCap bounds a run's retained series. At one frame per second this
// is a bit over two hours, which covers any soak worth watching live; a
// retained frame without its flow list measures a few hundred bytes, so a full
// ring costs well under a megabyte per run.
const DefaultFrameCap = 8000

// maxFrameSubBuffer bounds a subscriber's channel independently of the ring, so
// a slow client cannot make the server hold a second copy of the whole series
// per viewer. Falling further behind than this drops frames observably — the
// sequence numbers show the gap — rather than stalling the sampler.
const maxFrameSubBuffer = 256

func newFrameLog(capacity int) *frameLog {
	if capacity < 1 {
		capacity = 1
	}
	return &frameLog{cap: capacity, subs: make(map[int]chan *orbitv1.TelemetryFrame)}
}

// at returns the i-th retained frame, oldest first. Callers hold mu.
func (l *frameLog) at(i int) *orbitv1.TelemetryFrame { return l.buf[(l.head+i)%l.cap] }

// nextSequence reserves the next frame sequence number.
func (l *frameLog) nextSequence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextSeq
}

// append retains a frame and fans it out. The frame is stored trimmed; the
// copy sent live keeps its flows, since a live subscriber wants them.
func (l *frameLog) append(f *orbitv1.TelemetryFrame) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.nextSeq = f.GetFrameSeq() + 1

	stored := trimFrame(f)
	if l.n < l.cap {
		l.buf = append(l.buf, stored)
		l.n++
	} else {
		l.buf[l.head] = stored
		l.head = (l.head + 1) % l.cap
	}

	for _, ch := range l.subs {
		select {
		case ch <- f:
		default: // slow subscriber; the sequence gap makes the loss visible
		}
	}
}

// trimFrame drops what history does not need. The frame is cloned so the live
// fan-out keeps the original intact; proto messages carry a mutex and cannot be
// copied by value.
func trimFrame(f *orbitv1.TelemetryFrame) *orbitv1.TelemetryFrame {
	fleet, ok := f.GetProgress().(*orbitv1.TelemetryFrame_Fleet)
	if !ok || fleet.Fleet == nil || len(fleet.Fleet.GetFlows()) == 0 {
		return f
	}
	cp, _ := proto.Clone(f).(*orbitv1.TelemetryFrame)
	if c, ok := cp.GetProgress().(*orbitv1.TelemetryFrame_Fleet); ok && c.Fleet != nil {
		// The counts stay: "how many flows there were" is part of the history
		// even when the rows themselves are not.
		c.Fleet.Flows = nil
	}
	return cp
}

// frameSubscription is a live feed plus the backlog at subscribe time, taken
// atomically so a frame emitted concurrently is either in the backlog or on the
// channel, never both and never lost.
type frameSubscription struct {
	Backlog []*orbitv1.TelemetryFrame
	// DroppedBefore is how many frames before the first delivered one were
	// evicted — the caller's fromSeq pointed into history the ring no longer
	// holds.
	DroppedBefore uint64
	Ch            <-chan *orbitv1.TelemetryFrame
	cancel        func()
}

func (s *frameSubscription) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// subscribeFrom returns retained frames with Seq >= fromSeq and a live feed.
func (l *frameLog) subscribeFrom(fromSeq uint64) *frameSubscription {
	l.mu.Lock()
	defer l.mu.Unlock()

	var backlog []*orbitv1.TelemetryFrame
	for i := 0; i < l.n; i++ {
		if f := l.at(i); f.GetFrameSeq() >= fromSeq {
			backlog = append(backlog, f)
		}
	}
	var dropped uint64
	if l.n > 0 {
		if earliest := l.at(0).GetFrameSeq(); earliest > fromSeq {
			dropped = earliest - fromSeq
		}
	} else if l.nextSeq > fromSeq {
		dropped = l.nextSeq - fromSeq
	}

	if l.closed {
		// The run is over: hand back the history and a closed channel, so the
		// caller renders it and ends rather than waiting for frames that will
		// never come.
		ch := make(chan *orbitv1.TelemetryFrame)
		close(ch)
		return &frameSubscription{Backlog: backlog, DroppedBefore: dropped, Ch: ch, cancel: func() {}}
	}

	ch := make(chan *orbitv1.TelemetryFrame, maxFrameSubBuffer)
	id := l.nextSub
	l.nextSub++
	l.subs[id] = ch
	return &frameSubscription{
		Backlog: backlog, DroppedBefore: dropped, Ch: ch,
		cancel: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if c, ok := l.subs[id]; ok {
				delete(l.subs, id)
				close(c)
			}
		},
	}
}

// retained returns the frames currently held, oldest first. Used to archive a
// run at the moment it goes terminal; the frames are the trimmed copies the
// ring stores, so the archive inherits that trimming rather than repeating it.
func (l *frameLog) retained() []*orbitv1.TelemetryFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*orbitv1.TelemetryFrame, 0, l.n)
	for i := 0; i < l.n; i++ {
		out = append(out, l.at(i))
	}
	return out
}

// close ends live delivery once the run is terminal. The retained series stays
// readable — that is the point of keeping it.
func (l *frameLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for id, ch := range l.subs {
		delete(l.subs, id)
		close(ch)
	}
}
