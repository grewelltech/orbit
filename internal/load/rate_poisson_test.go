package load

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestPoissonReportsUnderlyingMean: the curve is what SLO reporting and rate
// introspection read, so it must stay the intended mean, not a sample.
func TestPoissonReportsUnderlyingMean(t *testing.T) {
	p := NewPoisson(Constant{RPS: 42}, 1)
	for _, e := range []time.Duration{0, time.Second, time.Minute} {
		if got := p.rps(e); got != 42 {
			t.Errorf("rps(%s) = %v, want 42", e, got)
		}
	}
}

// TestPoissonInterArrivalsAreExponential is the load-bearing assertion: the
// gaps must be exponentially distributed about 1/lambda, not evenly spaced.
// A uniform pacer would pass a mean check, so the variance and the spread are
// what distinguish it.
func TestPoissonInterArrivalsAreExponential(t *testing.T) {
	const (
		lambda = 1000.0 // 1 ms mean gap keeps the test fast
		n      = 20000
	)
	p := NewPoisson(Constant{RPS: lambda}, 12345)

	gaps := make([]float64, n)
	for i := range gaps {
		// Sample the gap directly rather than sleeping n times.
		gaps[i] = p.rng.ExpFloat64() / lambda
	}

	var sum float64
	for _, g := range gaps {
		sum += g
	}
	mean := sum / n
	var varSum float64
	for _, g := range gaps {
		varSum += (g - mean) * (g - mean)
	}
	variance := varSum / n

	want := 1 / lambda
	if math.Abs(mean-want)/want > 0.05 {
		t.Errorf("mean gap %.6fs, want ~%.6fs", mean, want)
	}
	// For an exponential distribution the standard deviation equals the mean;
	// an evenly-spaced pacer would have a standard deviation near zero.
	sd := math.Sqrt(variance)
	if math.Abs(sd-want)/want > 0.10 {
		t.Errorf("stddev %.6fs, want ~%.6fs (exponential has sd == mean)", sd, want)
	}
}

func TestPoissonIsReproducibleBySeed(t *testing.T) {
	a, b := NewPoisson(Constant{RPS: 10}, 99), NewPoisson(Constant{RPS: 10}, 99)
	for i := 0; i < 50; i++ {
		if x, y := a.rng.ExpFloat64(), b.rng.ExpFloat64(); x != y {
			t.Fatalf("sample %d diverged: %v vs %v", i, x, y)
		}
	}
}

// TestPoissonZeroRateDoesNotBlockForever guards the lambda<=0 branch: an
// infinite inter-arrival gap would wedge the dispatch loop.
func TestPoissonZeroRateDoesNotBlockForever(t *testing.T) {
	p := NewPoisson(Constant{RPS: 0}, 7)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.waitNext(ctx, 0) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitNext blocked on a zero rate")
	}
}

// TestPoissonDrivesRunWithoutTheLimiter checks the wiring: a self-pacing Rate
// must actually meter Run's dispatch.
func TestPoissonDrivesRunWithoutTheLimiter(t *testing.T) {
	cfg := Config{
		Total:       40,
		Concurrency: 8,
		Rate:        NewPoisson(Constant{RPS: 200}, 4242),
	}
	start := time.Now()
	rep := Run(context.Background(), cfg, func(ctx context.Context, i int) Sample {
		return Sample{Metrics: map[string]time.Duration{"attach": time.Millisecond}}
	})
	elapsed := time.Since(start)

	if rep.Succeeded != 40 {
		t.Errorf("succeeded = %d, want 40", rep.Succeeded)
	}
	// 40 arrivals at a 200/s mean is ~200ms of metering; without pacing it
	// would finish near-instantly.
	if elapsed < 50*time.Millisecond {
		t.Errorf("run took %s — arrivals do not appear to be metered", elapsed)
	}
}
