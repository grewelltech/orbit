package load

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// a fake attach that sleeps to model work and reports a latency metric.
func fakeAttach(work time.Duration, failEvery int) AttachFunc {
	return func(_ context.Context, i int) Sample {
		time.Sleep(work)
		if failEvery > 0 && i%failEvery == 0 {
			return Sample{Err: fmt.Errorf("boom %d", i)}
		}
		return Sample{Metrics: map[string]time.Duration{"attach": work}}
	}
}

func TestRunAggregatesLatencyAndOutcomes(t *testing.T) {
	rep := Run(context.Background(), Config{Total: 100, Concurrency: 16}, fakeAttach(2*time.Millisecond, 0))
	if rep.Attempted != 100 || rep.Succeeded != 100 || rep.Failed != 0 {
		t.Fatalf("attempted/succeeded/failed = %d/%d/%d, want 100/100/0", rep.Attempted, rep.Succeeded, rep.Failed)
	}
	st, ok := rep.Latencies["attach"]
	if !ok || st.Count != 100 {
		t.Fatalf("attach stats missing or wrong count: %+v", st)
	}
	if st.P50 < time.Millisecond || st.P50 > 50*time.Millisecond {
		t.Errorf("implausible P50 %v", st.P50)
	}
}

func TestRunCountsFailures(t *testing.T) {
	rep := Run(context.Background(), Config{Total: 100, Concurrency: 8}, fakeAttach(time.Millisecond, 2))
	// indices 0,2,4,… fail = 50 of 100.
	if rep.Failed != 50 || rep.Succeeded != 50 {
		t.Fatalf("succeeded/failed = %d/%d, want 50/50", rep.Succeeded, rep.Failed)
	}
}

func TestRunRespectsConcurrency(t *testing.T) {
	var inFlight, peak int64
	fn := func(_ context.Context, _ int) Sample {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(3 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return Sample{}
	}
	Run(context.Background(), Config{Total: 200, Concurrency: 10}, fn)
	if peak > 10 {
		t.Fatalf("peak concurrency %d exceeded cap 10", peak)
	}
}

func TestRunHonoursRateCurve(t *testing.T) {
	// 40 attaches at 100/s should take ~0.4s (not near-instant).
	start := time.Now()
	rep := Run(context.Background(), Config{Total: 40, Concurrency: 40, Rate: Constant{RPS: 100}},
		fakeAttach(time.Millisecond, 0))
	elapsed := time.Since(start)
	if elapsed < 250*time.Millisecond {
		t.Fatalf("run finished in %v — rate limiting not applied", elapsed)
	}
	if rep.AchievedRate > 160 {
		t.Errorf("achieved rate %.0f/s far exceeds offered 100/s", rep.AchievedRate)
	}
}

func TestRateCurves(t *testing.T) {
	if got := (LinearRamp{Start: 10, End: 110, Over: time.Second}).rps(500 * time.Millisecond); got < 55 || got > 65 {
		t.Errorf("linear midpoint = %.1f, want ~60", got)
	}
	step := Step{Points: []StepPoint{{At: 0, RPS: 5}, {At: time.Second, RPS: 20}}}
	if got := step.rps(500 * time.Millisecond); got != 5 {
		t.Errorf("step before threshold = %.1f, want 5", got)
	}
	if got := step.rps(2 * time.Second); got != 20 {
		t.Errorf("step after threshold = %.1f, want 20", got)
	}
}

func TestObserverReceivesEverySample(t *testing.T) {
	var got int64
	obs := observerFunc(func(Sample) { atomic.AddInt64(&got, 1) })
	Run(context.Background(), Config{Total: 50, Concurrency: 8, Observer: obs}, fakeAttach(time.Millisecond, 3))
	if got != 50 {
		t.Fatalf("observer saw %d samples, want 50", got)
	}
}

type observerFunc func(Sample)

func (f observerFunc) Observe(s Sample) { f(s) }
