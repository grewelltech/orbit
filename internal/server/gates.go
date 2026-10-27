package server

import (
	"errors"
	"fmt"
	"strings"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// Gate metrics (SLOGate.metric) a population's report answers: the attach
// success ratio, the achieved attach rate, and any procedure's latency
// quantile in milliseconds ("registration.p99_ms").
const (
	metricSuccessRatio = "attach.success_ratio"
	metricAchievedRate = "attach.achieved_rate"
)

// latencyQuantiles are the quantiles a "<procedure>.<q>_ms" metric names.
var latencyQuantiles = map[string]func(*orbitv1.ProcedureLatency) float64{
	"p50":  (*orbitv1.ProcedureLatency).GetP50Ms,
	"p90":  (*orbitv1.ProcedureLatency).GetP90Ms,
	"p99":  (*orbitv1.ProcedureLatency).GetP99Ms,
	"p999": (*orbitv1.ProcedureLatency).GetP999Ms,
	"max":  (*orbitv1.ProcedureLatency).GetMaxMs,
}

// validateGates checks a plan's gates before the run starts: each names
// the workload (or none, the whole run), an SLO metric this report has,
// and a comparison. Conformance gates arrive with conformance workloads.
func validateGates(gates []*orbitv1.Gate, workload string) error {
	names := map[string]bool{}
	for i, g := range gates {
		label := g.GetName()
		if label == "" {
			label = fmt.Sprintf("gates[%d]", i)
		} else if names[label] {
			return fmt.Errorf("gate %q: names must be unique", label)
		}
		names[label] = true
		if w := g.GetWorkload(); w != "" && w != workload {
			return fmt.Errorf("gate %q: no workload %q", label, w)
		}
		switch c := g.GetCheck().(type) {
		case *orbitv1.Gate_Conformance:
			return fmt.Errorf("gate %q: a conformance gate %w", label, errR3)
		case *orbitv1.Gate_Slo:
			if c.Slo.GetOp() == orbitv1.CompareOp_COMPARE_OP_UNSPECIFIED {
				return fmt.Errorf("gate %q: give a comparison (op)", label)
			}
			if err := knownMetric(c.Slo.GetMetric()); err != nil {
				return fmt.Errorf("gate %q: %w", label, err)
			}
		default:
			return fmt.Errorf("gate %q: give an slo check", label)
		}
	}
	return nil
}

func knownMetric(m string) error {
	switch m {
	case metricSuccessRatio, metricAchievedRate:
		return nil
	}
	proc, q, ok := strings.Cut(m, ".")
	if ok && proc != "" && strings.HasSuffix(q, "_ms") {
		if _, ok := latencyQuantiles[strings.TrimSuffix(q, "_ms")]; ok {
			return nil
		}
	}
	return fmt.Errorf("unknown metric %q: want %s, %s, or <procedure>.<p50|p90|p99|p999|max>_ms", m, metricSuccessRatio, metricAchievedRate)
}

// reportFigures is what a gate reads from either report kind.
type reportFigures struct {
	attempted, succeeded uint32
	achievedRate         float64
	latency              []*orbitv1.ProcedureLatency
}

func figuresOf(r *orbitv1.RunReport) (reportFigures, bool) {
	switch k := r.GetReport().(type) {
	case *orbitv1.RunReport_Load:
		l := k.Load
		return reportFigures{l.GetAttempted(), l.GetSucceeded(), l.GetAchievedRate(), l.GetLatency()}, true
	case *orbitv1.RunReport_Fleet:
		f := k.Fleet
		return reportFigures{f.GetAttached() + f.GetAttachFailed(), f.GetAttached(), f.GetAchievedRate(), f.GetLatency()}, true
	}
	return reportFigures{}, false
}

var errNoSamples = errors.New("no samples")

func (f reportFigures) metric(m string) (float64, error) {
	switch m {
	case metricSuccessRatio:
		if f.attempted == 0 {
			return 0, errNoSamples
		}
		return float64(f.succeeded) / float64(f.attempted), nil
	case metricAchievedRate:
		return f.achievedRate, nil
	}
	proc, q, _ := strings.Cut(m, ".")
	get := latencyQuantiles[strings.TrimSuffix(q, "_ms")]
	for _, l := range f.latency {
		if l.GetProcedure() == proc && l.GetCount() > 0 {
			return get(l), nil
		}
	}
	return 0, errNoSamples
}

func compare(op orbitv1.CompareOp, v, threshold float64) bool {
	switch op {
	case orbitv1.CompareOp_COMPARE_OP_LE:
		return v <= threshold
	case orbitv1.CompareOp_COMPARE_OP_LT:
		return v < threshold
	case orbitv1.CompareOp_COMPARE_OP_GE:
		return v >= threshold
	case orbitv1.CompareOp_COMPARE_OP_GT:
		return v > threshold
	}
	return false
}

var opText = map[orbitv1.CompareOp]string{
	orbitv1.CompareOp_COMPARE_OP_LE: "<=", orbitv1.CompareOp_COMPARE_OP_LT: "<",
	orbitv1.CompareOp_COMPARE_OP_GE: ">=", orbitv1.CompareOp_COMPARE_OP_GT: ">",
}

// applyGates judges a workload's report against the plan's gates (those
// naming it or none) and records the results in it. A metric with no
// samples fails its gate: an unmeasured SLO is not a met one.
func applyGates(r *orbitv1.RunReport, gates []*orbitv1.Gate) {
	figs, ok := figuresOf(r)
	r.Gates, r.GatesPassed = nil, true
	for _, g := range gates {
		if w := g.GetWorkload(); w != "" && w != r.GetWorkload() {
			continue
		}
		slo := g.GetSlo()
		res := &orbitv1.GateResult{Gate: g}
		switch v, err := figs.metric(slo.GetMetric()); {
		case !ok:
			res.Detail = "no report to judge"
		case err != nil:
			res.Detail = fmt.Sprintf("%s: %v", slo.GetMetric(), err)
		default:
			res.Observed = v
			res.Passed = compare(slo.GetOp(), v, slo.GetThreshold())
			res.Detail = fmt.Sprintf("%s = %.4g, want %s %.4g", slo.GetMetric(), v, opText[slo.GetOp()], slo.GetThreshold())
		}
		r.Gates = append(r.Gates, res)
		r.GatesPassed = r.GatesPassed && res.Passed
	}
}
