package load

import (
	"fmt"
	"sort"
	"time"
)

// LatencyBound caps selected percentiles of one procedure's latency. A zero
// field imposes no bound on that percentile.
type LatencyBound struct {
	P50, P99, P999, Max time.Duration
}

// SLO is a pass/fail contract over a load Report: a minimum success rate and
// per-procedure latency bounds. The zero value asserts nothing. It turns a
// load run into an integration-CI gate.
type SLO struct {
	MinSuccessRate float64                 // e.g. 0.99; 0 = not checked
	Latency        map[string]LatencyBound // by procedure name (e.g. "registration")
}

// Check is one evaluated assertion.
type Check struct {
	Name   string
	Pass   bool
	Detail string
}

// Verdict is the overall SLO result: Pass is true only if every Check passed.
type Verdict struct {
	Pass   bool
	Checks []Check
}

// Empty reports whether the SLO asserts nothing.
func (s SLO) Empty() bool { return s.MinSuccessRate <= 0 && len(s.Latency) == 0 }

// Evaluate applies the SLO to a report and returns the verdict.
func (s SLO) Evaluate(rep Report) Verdict {
	v := Verdict{Pass: true}
	if s.MinSuccessRate > 0 {
		rate := 0.0
		if rep.Attempted > 0 {
			rate = float64(rep.Succeeded) / float64(rep.Attempted)
		}
		v.add(Check{
			Name:   "success_rate",
			Pass:   rate >= s.MinSuccessRate,
			Detail: fmt.Sprintf("%.4f (min %.4f)", rate, s.MinSuccessRate),
		})
	}
	names := make([]string, 0, len(s.Latency))
	for n := range s.Latency {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		st, ok := rep.Latencies[name]
		if !ok {
			v.add(Check{Name: name, Pass: false, Detail: "no samples recorded"})
			continue
		}
		b := s.Latency[name]
		for _, c := range []struct {
			q           string
			bound, meas time.Duration
		}{
			{"p50", b.P50, st.P50},
			{"p99", b.P99, st.P99},
			{"p99.9", b.P999, st.P999},
			{"max", b.Max, st.Max},
		} {
			if c.bound == 0 {
				continue
			}
			v.add(Check{
				Name:   name + "." + c.q,
				Pass:   c.meas <= c.bound,
				Detail: fmt.Sprintf("%v (max %v)", c.meas.Round(100*time.Microsecond), c.bound),
			})
		}
	}
	return v
}

func (v *Verdict) add(c Check) {
	v.Checks = append(v.Checks, c)
	if !c.Pass {
		v.Pass = false
	}
}
