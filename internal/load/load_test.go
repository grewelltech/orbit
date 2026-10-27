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

func TestSoakRunsForDurationWithResourceSamples(t *testing.T) {
	rep := Run(context.Background(), Config{
		Concurrency: 16, Rate: Constant{RPS: 200},
		Duration: 500 * time.Millisecond, SampleInterval: 100 * time.Millisecond,
	}, fakeAttach(time.Millisecond, 0))
	if rep.Duration < 450*time.Millisecond {
		t.Fatalf("soak ran %v, expected ~500ms", rep.Duration)
	}
	if rep.Succeeded < 50 {
		t.Fatalf("soak did too little work: %d attaches", rep.Succeeded)
	}
	if len(rep.Resources) < 3 {
		t.Fatalf("expected resource samples over the soak, got %d", len(rep.Resources))
	}
	if rep.Resources[0].Goroutines == 0 {
		t.Error("resource sample has no goroutine count")
	}
}

// A resource trend stays within MaxResourceSamples however long the run,
// evenly spaced over all of it.
func TestResourceTrendBounded(t *testing.T) {
	tests := []struct {
		name       string
		ticks      int
		wantLen    int
		wantStride int
	}{
		{name: "under the cap", ticks: 10, wantLen: 10, wantStride: 1},
		{name: "one short of the cap", ticks: MaxResourceSamples - 1, wantLen: MaxResourceSamples - 1, wantStride: 1},
		{name: "at the cap, halved", ticks: MaxResourceSamples, wantLen: MaxResourceSamples / 2, wantStride: 2},
		{name: "a forever soak", ticks: 50 * MaxResourceSamples, wantLen: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newResourceTrend()
			for i := 1; i <= tc.ticks; i++ {
				at := time.Duration(i) * time.Second
				r.add(func() ResourceSample { return ResourceSample{At: at} })
			}
			n := len(r.samples)
			if n >= MaxResourceSamples || n == 0 {
				t.Fatalf("%d samples, want 1..%d", n, MaxResourceSamples-1)
			}
			if tc.wantLen >= 0 && (n != tc.wantLen || r.stride != tc.wantStride) {
				t.Errorf("%d samples at stride %d, want %d at %d", n, r.stride, tc.wantLen, tc.wantStride)
			}
			// Evenly spaced at the stride, from the run's start.
			for i, s := range r.samples {
				if want := time.Duration((i+1)*r.stride) * time.Second; s.At != want {
					t.Fatalf("sample %d at %v, want %v (stride %d)", i, s.At, want, r.stride)
				}
			}
		})
	}
}
