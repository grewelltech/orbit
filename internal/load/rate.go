// Package load drives rate-controlled attach storms and reports per-procedure
// latency KPIs. It extends the D-6 bounded-concurrency decision with a ramp
// scheduler (x/time/rate) so a run can hold, ramp, or step the offered attach
// rate while capping the concurrent burst — the foundation for reporting sim
// capacity (against a mock AMF) separately from the core-limited baseline.
package load

import "time"

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
