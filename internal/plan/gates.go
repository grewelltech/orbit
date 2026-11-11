package plan

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/target"
)

// gate is gates[n]: a conformance gate (conformance: true, with an optional
// policy: default or spec) or an SLO gate.
// An SLO gate's metric and comparison may come from its name alone, as the
// --slo-* flags set only a threshold: GateName's metric, compared the way
// the metric reads (a ratio or rate at least the threshold, a latency at
// most).
func (b builder) gate(n string) (*orbitv1.Gate, error) {
	base := "gates[" + n + "]."
	workload, metricFromName := splitGateName(n)
	if w := b.str(base + "workload"); w != "" {
		workload = w
	}
	g := &orbitv1.Gate{Name: n, Workload: workload}
	_, hasSLO := b.get(base + "slo.threshold")
	if b.flag(base + "conformance") {
		if hasSLO || b.str(base+"slo.metric") != "" {
			return nil, errors.New("give a conformance gate or an slo gate, not both")
		}
		policy, err := conformance.ParseGatePolicy(b.str(base + "policy"))
		if err != nil {
			return nil, err
		}
		cg := &orbitv1.ConformanceGate{}
		if policy != conformance.GateDefault {
			cg.Policy = string(policy)
		}
		g.Check = &orbitv1.Gate_Conformance{Conformance: cg}
		return g, nil
	}
	if b.str(base+"policy") != "" {
		return nil, errors.New("policy belongs to a conformance gate (conformance: true)")
	}
	if !hasSLO {
		return nil, errors.New("give slo.threshold (with slo.metric and slo.op, or a name that says the metric), or conformance: true")
	}
	metric := b.str(base + "slo.metric")
	if metric == "" {
		metric = metricFromName
	}
	if metric == "" {
		return nil, fmt.Errorf("give slo.metric: the gate's name does not say one")
	}
	op, err := compareOp(b.str(base+"slo.op"), metric)
	if err != nil {
		return nil, err
	}
	th, err := threshold(b.str(base+"slo.threshold"), metric)
	if err != nil {
		return nil, err
	}
	g.Check = &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{Metric: metric, Op: op, Threshold: th}}
	return g, nil
}

// splitGateName reads a gate's name back into its workload and metric
// (target.GateName): "pop/registration_p99_ms" is pop's
// registration.p99_ms. A name that is not a metric's gives "".
func splitGateName(n string) (workload, metric string) {
	name := n
	if w, rest, ok := strings.Cut(n, "/"); ok {
		workload, name = w, rest
	}
	switch name {
	case "attach_success_ratio":
		return workload, "attach.success_ratio"
	case "attach_achieved_rate":
		return workload, "attach.achieved_rate"
	}
	// <procedure>_<p50|p90|p99|p999|max>_ms; the procedure may hold
	// underscores of its own (pdu_session_p99_ms).
	rest, ok := strings.CutSuffix(name, "_ms")
	if !ok {
		return workload, ""
	}
	i := strings.LastIndex(rest, "_")
	if i <= 0 {
		return workload, ""
	}
	switch q := rest[i+1:]; q {
	case "p50", "p90", "p99", "p999", "max":
		return workload, rest[:i] + "." + q + "_ms"
	}
	return workload, ""
}

// compareOp is a gate's comparison: <=, <, >=, > (or le, lt, ge, gt);
// when unset, at least the threshold for a ratio or rate, at most for a
// latency.
func compareOp(op, metric string) (orbitv1.CompareOp, error) {
	switch strings.ToLower(op) {
	case "<=", "le":
		return orbitv1.CompareOp_COMPARE_OP_LE, nil
	case "<", "lt":
		return orbitv1.CompareOp_COMPARE_OP_LT, nil
	case ">=", "ge":
		return orbitv1.CompareOp_COMPARE_OP_GE, nil
	case ">", "gt":
		return orbitv1.CompareOp_COMPARE_OP_GT, nil
	case "":
		if strings.HasSuffix(metric, "_ms") {
			return orbitv1.CompareOp_COMPARE_OP_LE, nil
		}
		return orbitv1.CompareOp_COMPARE_OP_GE, nil
	}
	return 0, fmt.Errorf("slo.op %q: want <=, <, >= or >", op)
}

// threshold is a gate's threshold: a number, or for a latency metric a
// duration (500ms, 3s) in milliseconds.
func threshold(s, metric string) (float64, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(metric, "_ms") {
		if d, err := time.ParseDuration(s); err == nil {
			return float64(d) / float64(time.Millisecond), nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		want := "a number"
		if strings.HasSuffix(metric, "_ms") {
			want = "a number of milliseconds or a duration such as 500ms"
		}
		return 0, fmt.Errorf("slo.threshold %q: want %s", s, want)
	}
	return v, nil
}

// ParseGate reads a --gate flag into settings on the gates list (the flag
// layer): name=metric<op>value (reg=registration.p99_ms<=500ms),
// metric<op>value, its name then GateName's, "conformance" (a conformance
// gate on the run, default policy), "spec" (one with the spec policy), and
// name=conformance or name=spec.
func ParseGate(spec string) ([]target.Setting, error) {
	s, err := parseGate(spec)
	return s, target.MaskErr(err)
}

func parseGate(spec string) ([]target.Setting, error) {
	src := target.Source{Name: "--gate"}
	name, expr := "", spec
	if n, rest, ok := strings.Cut(spec, "="); ok && !strings.ContainsAny(n, "<>") {
		name, expr = strings.TrimSpace(n), strings.TrimSpace(rest)
	}
	if strings.ContainsAny(name, ".[]") {
		return nil, fmt.Errorf("--gate %q: a gate's name may not hold '.', '[' or ']'", spec)
	}
	if expr == "conformance" || expr == string(conformance.GateSpec) {
		if name == "" {
			name = "conformance"
		}
		out := []target.Setting{{Path: "gates[" + name + "].conformance", Value: "true", Source: src}}
		if expr == string(conformance.GateSpec) {
			out = append(out, target.Setting{Path: "gates[" + name + "].policy", Value: string(conformance.GateSpec), Source: src})
		}
		return out, nil
	}
	for _, op := range []string{"<=", ">=", "<", ">"} {
		metric, value, ok := strings.Cut(expr, op)
		if !ok {
			continue
		}
		metric, value = strings.TrimSpace(metric), strings.TrimSpace(value)
		if metric == "" || value == "" {
			break
		}
		if name == "" {
			name = target.GateName("", metric)
		}
		if strings.ContainsAny(name, ".[]") {
			return nil, fmt.Errorf("--gate %q: a gate's name may not hold '.', '[' or ']'", spec)
		}
		base := "gates[" + name + "]."
		return []target.Setting{
			{Path: base + "slo.metric", Value: metric, Source: src},
			{Path: base + "slo.op", Value: op, Source: src},
			{Path: base + "slo.threshold", Value: value, Source: src},
		}, nil
	}
	return nil, fmt.Errorf("--gate %q: want [name=]metric<op>value (op <=, <, >= or >), conformance or spec", spec)
}
