package server

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
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
// the workload (or none, the whole run) and a check its report answers:
// for a population an SLO metric and a comparison, for a conformance
// workload (suite) the conformance gate.
func validateGates(gates []*orbitv1.Gate, workload string, suite bool) error {
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
			if !suite {
				return fmt.Errorf("gate %q: a conformance gate judges a conformance workload; %q is a population", label, workload)
			}
		case *orbitv1.Gate_Slo:
			if suite {
				return fmt.Errorf("gate %q: an slo gate judges a population; %q is a conformance workload (give a conformance gate)", label, workload)
			}
			if _, ok := opText[c.Slo.GetOp()]; !ok {
				return fmt.Errorf("gate %q: give a comparison (op: le, lt, ge or gt), not op %d", label, int32(c.Slo.GetOp()))
			}
			if err := knownMetric(c.Slo.GetMetric()); err != nil {
				return fmt.Errorf("gate %q: %w", label, err)
			}
		default:
			return fmt.Errorf("gate %q: give an slo or conformance check", label)
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
	if ok && slices.Contains(gateProcedures, proc) && strings.HasSuffix(q, "_ms") {
		if _, ok := latencyQuantiles[strings.TrimSuffix(q, "_ms")]; ok {
			return nil
		}
	}
	return fmt.Errorf("unknown metric %q: want %s, %s, or <%s>.<p50|p90|p99|p999|max>_ms",
		m, metricSuccessRatio, metricAchievedRate, strings.Join(gateProcedures, "|"))
}

// gateProcedures are the procedures a population's report times, so the
// ones a latency gate can name: a misspelt one is refused before the run,
// not failed after it for want of samples.
var gateProcedures = []string{engine.ProcedureAttach, engine.ProcedureRegistration, engine.ProcedurePDUSession}

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
		// A run that attempted nothing has no rate to measure, not a rate of 0.
		if f.attempted == 0 {
			return 0, errNoSamples
		}
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
		res := &orbitv1.GateResult{Gate: g}
		if g.GetConformance() != nil {
			res.Passed, res.Observed, res.Detail = judgeSuite(r.GetConformance())
			r.Gates = append(r.Gates, res)
			r.GatesPassed = r.GatesPassed && res.Passed
			continue
		}
		slo := g.GetSlo()
		switch v, err := figs.metric(slo.GetMetric()); {
		case !ok:
			res.Detail = "no report to judge"
		case err != nil:
			res.Detail = fmt.Sprintf("%v for %s", err, slo.GetMetric())
		default:
			res.Observed = v
			res.Passed = compare(slo.GetOp(), v, slo.GetThreshold())
			res.Detail = fmt.Sprintf("%s = %.4g, want %s %.4g", slo.GetMetric(), v, opText[slo.GetOp()], slo.GetThreshold())
		}
		r.Gates = append(r.Gates, res)
		r.GatesPassed = r.GatesPassed && res.Passed
	}
}

// judgeSuite is the conformance gate (ADR-0008 §2): it passes when the
// suite ran to the end and no check FAILed outside the expected-failures
// list or ERRORed, Summary.OK as the CLI's exit code has it. observed is
// how many did. An incomplete suite fails: an unmeasured suite is not a
// passed one.
func judgeSuite(c *orbitv1.ConformanceSummary) (passed bool, observed float64, detail string) {
	if c == nil {
		return false, 0, "no report to judge"
	}
	if c.GetIncomplete() {
		return false, 0, fmt.Sprintf("suite incomplete: %d of %d checks ran", len(c.GetResults()), c.GetTotal())
	}
	var bad []string
	for _, r := range c.GetResults() {
		switch {
		case r.GetVerdict() == "FAIL" && r.GetExpectedFailure() == nil, r.GetVerdict() == "ERROR":
			bad = append(bad, r.GetId()+" "+r.GetVerdict())
		}
	}
	if len(bad) > 0 {
		return false, float64(len(bad)), fmt.Sprintf("%d unexpected: %s", len(bad), strings.Join(bad, ", "))
	}
	return true, 0, fmt.Sprintf("%d checks, no unexpected FAIL or ERROR", len(c.GetResults()))
}
