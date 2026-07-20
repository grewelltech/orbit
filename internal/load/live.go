package load

import (
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Observers fans one Sample out to several Observers, so a run can feed live
// progress, Prometheus, and anything else at once without Run knowing about
// any of them.
//
// Observe is called on the attempt's own goroutine, so an implementation that
// blocks slows the run it is measuring. Keep them non-blocking.
type Observers []Observer

// Observe forwards to each observer in order. Nil entries are skipped, so a
// caller can build the slice with optional members.
func (o Observers) Observe(s Sample) {
	for _, obs := range o {
		if obs != nil {
			obs.Observe(s)
		}
	}
}

// Snapshot is a point-in-time view of a run in progress. It carries cumulative
// values only; a caller wanting an interval rate takes the difference between
// two snapshots, which keeps Snapshot idempotent and lets several consumers
// sample at their own cadence.
type Snapshot struct {
	Attempted, Succeeded, Failed int
	Elapsed                      time.Duration
	// AchievedRate is succeeded per second since the run began.
	AchievedRate float64
	// Latencies holds the per-procedure distribution so far.
	Latencies map[string]Stats
}

// LiveStats accumulates attempt outcomes as they complete, so a run's progress
// can be read while it is still going. The final Report remains authoritative:
// this is a sampled view, and percentiles here cover only attempts completed
// at the moment of the call.
//
// Safe for concurrent use: Observe is called from the run's worker pool while
// Snapshot is called from whatever is displaying progress.
type LiveStats struct {
	start time.Time

	// mu guards every field below. hdrhistogram is not safe for concurrent
	// read/write, so Snapshot's quantile reads take the same lock as Observe's
	// writes rather than a separate reader lock.
	mu                           sync.Mutex
	attempted, succeeded, failed int
	hists                        map[string]*hdr.Histogram
}

// NewLiveStats starts a live aggregator. Elapsed time is measured from here,
// so construct it immediately before the run.
func NewLiveStats() *LiveStats {
	return &LiveStats{start: time.Now(), hists: map[string]*hdr.Histogram{}}
}

// Observe records one completed attempt. Implements Observer.
func (l *LiveStats) Observe(s Sample) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.attempted++
	if s.Err != nil {
		l.failed++
		// A failed attempt has no meaningful latency to record; counting its
		// partial timings would bias the percentiles toward whatever failed early.
		return
	}
	l.succeeded++

	for name, d := range s.Metrics {
		h, ok := l.hists[name]
		if !ok {
			h = hdr.New(1, 300_000_000, 3) // 1µs … 300s, 3 sig figs — as the final report
			l.hists[name] = h
		}
		v := d.Microseconds()
		if v < 1 {
			v = 1
		}
		_ = h.RecordValue(v)
	}
}

// Snapshot reports progress so far.
func (l *LiveStats) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	elapsed := time.Since(l.start)
	lat := make(map[string]Stats, len(l.hists))
	for name, h := range l.hists {
		lat[name] = Stats{
			Count: h.TotalCount(),
			P50:   usec(h.ValueAtQuantile(50)),
			P90:   usec(h.ValueAtQuantile(90)),
			P99:   usec(h.ValueAtQuantile(99)),
			P999:  usec(h.ValueAtQuantile(99.9)),
			Max:   usec(h.Max()),
		}
	}

	var rate float64
	if elapsed > 0 {
		rate = float64(l.succeeded) / elapsed.Seconds()
	}
	return Snapshot{
		Attempted:    l.attempted,
		Succeeded:    l.succeeded,
		Failed:       l.failed,
		Elapsed:      elapsed,
		AchievedRate: rate,
		Latencies:    lat,
	}
}
