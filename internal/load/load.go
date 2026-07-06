package load

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"golang.org/x/time/rate"
)

// Sample is the outcome of one attach attempt: named per-procedure latencies
// (e.g. "attach", "registration", "pdu_session") and an error (nil = success).
type Sample struct {
	Metrics map[string]time.Duration
	Err     error
}

// AttachFunc performs one attach attempt and returns its sample. It is called
// concurrently from a bounded worker pool; index is the 0-based attempt number
// (use it to pick a distinct SUPI/IMSI per UE).
type AttachFunc func(ctx context.Context, index int) Sample

// Observer receives each attempt's Sample as it completes — a live hook for
// metrics exposition (e.g. Prometheus) during long runs, independent of the
// final aggregated Report. Implementations must be safe for concurrent use.
type Observer interface {
	Observe(Sample)
}

// Config parameterises a load run.
type Config struct {
	Total       int  // number of attaches to attempt (ignored if Duration > 0)
	Concurrency int  // max attaches in flight (bounds the burst; D-6). 0 = 64
	Rate        Rate // offered-rate curve; nil = as fast as concurrency allows
	// Duration, if > 0, runs a soak: keep offering attaches until it elapses
	// (Total is ignored), so resource stability can be observed over time.
	Duration time.Duration
	// SampleInterval, if > 0, records a resource sample (goroutines, RSS) at
	// that cadence for the duration of the run (soak resource-trend hook).
	SampleInterval time.Duration
	// Observer, if set, is called once per completed attempt (live metrics).
	Observer Observer
}

// ResourceSample is a point-in-time process resource reading during a run.
type ResourceSample struct {
	At         time.Duration
	Goroutines int
	RSSBytes   uint64
}

// Stats summarises one procedure's latency distribution.
type Stats struct {
	Count               int64
	P50, P90, P99, P999 time.Duration
	Max                 time.Duration
}

// Report is the aggregate result of a load run.
type Report struct {
	Attempted, Succeeded, Failed int
	Duration                     time.Duration
	AchievedRate                 float64          // succeeded per second over the run
	Latencies                    map[string]Stats // per procedure name
	Resources                    []ResourceSample // soak resource trend (if sampled)
}

// hist accumulates latencies for one procedure. hdrhistogram is not
// thread-safe, so recording is mutex-guarded (the critical section is a single
// RecordValue).
type recorder struct {
	mu    sync.Mutex
	hists map[string]*hdr.Histogram
}

func newRecorder() *recorder { return &recorder{hists: map[string]*hdr.Histogram{}} }

func (r *recorder) record(metrics map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, d := range metrics {
		h, ok := r.hists[name]
		if !ok {
			h = hdr.New(1, 300_000_000, 3) // 1µs … 300s, 3 sig figs
			r.hists[name] = h
		}
		v := d.Microseconds()
		if v < 1 {
			v = 1
		}
		_ = h.RecordValue(v)
	}
}

func (r *recorder) stats() map[string]Stats {
	out := make(map[string]Stats, len(r.hists))
	for name, h := range r.hists {
		out[name] = Stats{
			Count: h.TotalCount(),
			P50:   usec(h.ValueAtQuantile(50)),
			P90:   usec(h.ValueAtQuantile(90)),
			P99:   usec(h.ValueAtQuantile(99)),
			P999:  usec(h.ValueAtQuantile(99.9)),
			Max:   usec(h.Max()),
		}
	}
	return out
}

func usec(v int64) time.Duration { return time.Duration(v) * time.Microsecond }

// Run drives cfg.Total attach attempts through fn, paced by cfg.Rate and
// bounded to cfg.Concurrency in flight, aggregating per-procedure latencies.
// It returns once every attempt has completed (or ctx is cancelled).
func Run(ctx context.Context, cfg Config, fn AttachFunc) Report {
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 64
	}

	rec := newRecorder()
	var mu sync.Mutex
	succeeded, failed := 0, 0

	// Offered-rate limiter, updated on a curve if one is configured.
	var lim *rate.Limiter
	if cfg.Rate != nil {
		lim = rate.NewLimiter(rate.Limit(nonZero(cfg.Rate.rps(0))), 1)
	}
	start := time.Now()
	stop := make(chan struct{})
	if lim != nil {
		go func() {
			t := time.NewTicker(100 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case now := <-t.C:
					lim.SetLimit(rate.Limit(nonZero(cfg.Rate.rps(now.Sub(start)))))
				}
			}
		}()
	}

	// Soak resource-trend sampler.
	var resMu sync.Mutex
	var resources []ResourceSample
	if cfg.SampleInterval > 0 {
		go func() {
			t := time.NewTicker(cfg.SampleInterval)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case now := <-t.C:
					resMu.Lock()
					resources = append(resources, ResourceSample{
						At: now.Sub(start), Goroutines: runtime.NumGoroutine(), RSSBytes: rssBytes(),
					})
					resMu.Unlock()
				}
			}
		}()
	}

	// Dispatch: bounded by Total, or by Duration in soak mode.
	soak := cfg.Duration > 0
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := 0; ; i++ {
		if soak {
			if time.Since(start) >= cfg.Duration {
				break
			}
		} else if i >= cfg.Total {
			break
		}
		if lim != nil {
			if err := lim.Wait(ctx); err != nil {
				break
			}
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			s := fn(ctx, idx)
			mu.Lock()
			if s.Err != nil {
				failed++
			} else {
				succeeded++
			}
			mu.Unlock()
			if len(s.Metrics) > 0 {
				rec.record(s.Metrics)
			}
			if cfg.Observer != nil {
				cfg.Observer.Observe(s)
			}
		}(i)
	}
	wg.Wait()
	close(stop)

	dur := time.Since(start)
	rate := 0.0
	if dur > 0 {
		rate = float64(succeeded) / dur.Seconds()
	}
	resMu.Lock()
	res := resources
	resMu.Unlock()
	return Report{
		Attempted:    succeeded + failed,
		Succeeded:    succeeded,
		Failed:       failed,
		Duration:     dur,
		AchievedRate: rate,
		Latencies:    rec.stats(),
		Resources:    res,
	}
}

func nonZero(f float64) float64 {
	if f <= 0 {
		return 0.0001
	}
	return f
}

// rssBytes returns the process resident set size (Linux /proc/self/statm).
func rssBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	resident, _ := strconv.ParseUint(f[1], 10, 64)
	return resident * uint64(os.Getpagesize())
}
