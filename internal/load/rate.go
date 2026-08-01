// Package load drives rate-controlled attach storms and reports per-procedure
// latency KPIs. It extends the D-6 bounded-concurrency decision with a ramp
// scheduler (x/time/rate) so a run can hold, ramp, or step the offered attach
// rate while capping the concurrent burst — the foundation for reporting sim
// capacity (against a mock AMF) separately from the core-limited baseline.
package load

import (
	"context"
	"math/rand/v2"
	"time"
)

// Rate is a load profile: the target attaches-per-second at a given elapsed
// time into the run. A nil Rate means "as fast as concurrency allows".
type Rate interface {
	rps(elapsed time.Duration) float64
}

// Constant offers a fixed rate for the whole run.
type Constant struct{ RPS float64 }

func (c Constant) rps(time.Duration) float64 { return c.RPS }

// LinearRamp ramps linearly from Start to End over Over, then holds End —
// the canonical "find the knee" attach storm.
type LinearRamp struct {
	Start, End float64
	Over       time.Duration
}

func (l LinearRamp) rps(e time.Duration) float64 {
	if l.Over <= 0 || e >= l.Over {
		return l.End
	}
	return l.Start + (l.End-l.Start)*(float64(e)/float64(l.Over))
}

// Step is a staircase: each point's RPS holds until the next point's At time.
type Step struct {
	Points []StepPoint
}

// StepPoint sets the offered rate to RPS from elapsed time At onward.
type StepPoint struct {
	At  time.Duration
	RPS float64
}

func (s Step) rps(e time.Duration) float64 {
	rps := 0.0
	for _, p := range s.Points {
		if e >= p.At {
			rps = p.RPS
		} else {
			break
		}
	}
	return rps
}

// pacer is implemented by Rate models that meter arrivals themselves instead of
// through the runner's smooth limiter. Rate models that only describe a
// mean-rate curve do not implement it.
type pacer interface {
	// waitNext blocks until the next arrival is due, or ctx ends.
	waitNext(ctx context.Context, elapsed time.Duration) error
}

// Poisson turns a mean-rate curve into a Poisson arrival process: inter-arrival
// times are exponentially distributed about the curve's instantaneous mean,
// rather than evenly spaced.
//
// Rationale: x/time/rate meters arrivals uniformly, which is the easy case for
// a device under test — a uniform 100/s never presents the transient bursts a
// real 100/s does. Subscriber attaches arrive independently, so a Poisson
// process is the realistic model, and it exercises queueing that evenly-spaced
// load never reaches at the same mean.
//
// Poisson carries PRNG state, so it must be used as the pointer returned by
// NewPoisson. waitNext is called only from the runner's single dispatch
// goroutine and is not safe for concurrent use.
type Poisson struct {
	mean Rate
	rng  *rand.Rand
}

// NewPoisson wraps a mean-rate curve in a Poisson arrival process. seed of 0
// draws a nondeterministic seed; any other value makes the arrival sequence
// reproducible, which is what the tests rely on.
func NewPoisson(mean Rate, seed uint64) *Poisson {
	if seed == 0 {
		seed = rand.Uint64()
	}
	return &Poisson{mean: mean, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// rps reports the underlying mean, so SLO reporting and rate introspection see
// the intended offered rate rather than a sampled one.
func (p *Poisson) rps(e time.Duration) float64 { return p.mean.rps(e) }

func (p *Poisson) waitNext(ctx context.Context, elapsed time.Duration) error {
	lambda := p.mean.rps(elapsed)
	if lambda <= 0 {
		// A zero mean would make the inter-arrival gap infinite. Park until the
		// curve rises again rather than blocking the run forever.
		return sleepCtx(ctx, 100*time.Millisecond)
	}
	// Inter-arrival of a Poisson process: Exp(lambda), sampled as Exp(1)/lambda.
	gap := time.Duration(p.rng.ExpFloat64() / lambda * float64(time.Second))
	return sleepCtx(ctx, gap)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
