// Package plan builds a RunPlan (ADR-0008 §2) from a resolved
// configuration: the target and the run-file fields the layers set
// (internal/target). It is the one place every client path makes a plan
// (orbit run, orbit test, the run-starting commands): a pure mapping, so a
// field that reaches the resolver reaches the plan the same way whichever
// command set it.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/target"
)

// Option configures Build.
type Option func(*options)

type options struct {
	readFile func(string) ([]byte, error)
}

// WithReadFile lets Build read the files a run file names, such as a
// conformance workload's expected-failures list given as a path. Without
// it such a path is an error: Build does no I/O of its own.
func WithReadFile(read func(string) ([]byte, error)) Option {
	return func(o *options) { o.readFile = read }
}

// Build turns a resolved configuration into a RunPlan and its provenance
// (keys included in the plan, on the wire only; masked in the
// provenance). It checks what the plan's shape needs, such as one kind per
// workload and one way to stop; the server checks the rest when it builds
// the run.
func Build(res *target.Resolved, opts ...Option) (*orbitv1.RunPlan, []*orbitv1.FieldProvenance, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	b := builder{res: res, o: o}
	p := &orbitv1.RunPlan{Target: TargetProto(res.Target)}
	var err error
	if p.Until, err = b.until(); err != nil {
		return nil, nil, err
	}
	for _, n := range res.Names("workloads") {
		w, err := b.workload(n)
		if err != nil {
			return nil, nil, fmt.Errorf("workloads[%s]: %w", n, err)
		}
		p.Workloads = append(p.Workloads, w)
	}
	for _, n := range res.Names("gates") {
		g, err := b.gate(n)
		if err != nil {
			return nil, nil, fmt.Errorf("gates[%s]: %w", n, err)
		}
		p.Gates = append(p.Gates, g)
	}
	return p, ProvenanceProto(res.Provenance), nil
}

// RunName and EventVerbosity are the run's own settings, which travel in
// StartRunRequest beside the plan.
func RunName(res *target.Resolved) string {
	s, _ := res.Run["run.name"].(string)
	return s
}

// EventVerbosity is run.event_verbosity: normal (the default) or verbose.
func EventVerbosity(res *target.Resolved) (orbitv1.EventVerbosity, error) {
	switch v, _ := res.Run["run.event_verbosity"].(string); v {
	case "", "normal":
		return orbitv1.EventVerbosity_EVENT_VERBOSITY_NORMAL, nil
	case "verbose":
		return orbitv1.EventVerbosity_EVENT_VERBOSITY_VERBOSE, nil
	default:
		return 0, fmt.Errorf("run.event_verbosity %q: want normal or verbose", v)
	}
}

type builder struct {
	res *target.Resolved
	o   options
}

// get is the run-file field at path, ok false when no layer set it.
func (b builder) get(path string) (any, bool) {
	v, ok := b.res.Run[path]
	return v, ok
}

func (b builder) str(path string) string {
	v, _ := b.get(path)
	s, _ := v.(string)
	return s
}

func (b builder) u32(path string) uint32 {
	v, _ := b.get(path)
	n, _ := v.(uint64)
	return uint32(n)
}

func (b builder) u64(path string) uint64 {
	v, _ := b.get(path)
	n, _ := v.(uint64)
	return n
}

func (b builder) f64(path string) float64 {
	v, _ := b.get(path)
	f, _ := v.(float64)
	return f
}

func (b builder) flag(path string) bool {
	v, _ := b.get(path)
	t, _ := v.(bool)
	return t
}

// until is how the run stops: one of until.duration_ms, until.duration
// ("2h"), until.complete and until.forever; none is until.complete's
// default, left unset.
func (b builder) until() (*orbitv1.Until, error) {
	var set []string
	u := &orbitv1.Until{}
	if ms, ok := b.get("until.duration_ms"); ok {
		set = append(set, "duration_ms")
		u.Stop = &orbitv1.Until_DurationMs{DurationMs: ms.(uint64)}
	}
	if d := b.str("until.duration"); d != "" {
		set = append(set, "duration")
		dur, err := time.ParseDuration(d)
		if err != nil || dur <= 0 {
			return nil, fmt.Errorf("until.duration %q: want a duration such as 30m or 2h", d)
		}
		u.Stop = &orbitv1.Until_DurationMs{DurationMs: uint64(dur.Milliseconds())}
	}
	if b.flag("until.complete") {
		set = append(set, "complete")
		u.Stop = &orbitv1.Until_Complete{Complete: true}
	}
	if b.flag("until.forever") {
		set = append(set, "forever")
		u.Stop = &orbitv1.Until_Forever{Forever: true}
	}
	switch len(set) {
	case 0:
		return nil, nil
	case 1:
		return u, nil
	}
	return nil, fmt.Errorf("until: give one of duration, complete and forever, not %s", strings.Join(set, " and "))
}

// populationFields and conformanceFields are the run-file fields below a
// workload that say its kind.
var populationFields = []string{"attach.", "topology.", "behaviors", "pdu_session", "subscriber_pool", "sample_interval_ms"}

// workload is workloads[n]: a conformance workload when it sets any
// conformance field, else a population.
func (b builder) workload(n string) (*orbitv1.Workload, error) {
	base := "workloads[" + n + "]."
	var pop, suite bool
	for p := range b.res.Run {
		rel, ok := strings.CutPrefix(p, base)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(rel, "conformance."):
			suite = true
		case slices.ContainsFunc(populationFields, func(f string) bool { return strings.HasPrefix(rel, f) }):
			pop = true
		}
	}
	if pop && suite {
		return nil, errors.New("sets both population and conformance fields: give one kind per workload")
	}
	w := &orbitv1.Workload{Name: n}
	if v, ok := b.get(base + "start"); ok {
		st, err := startCondition(v)
		if err != nil {
			return nil, err
		}
		w.Start = st
	}
	if suite {
		cw, err := b.conformance(base + "conformance.")
		if err != nil {
			return nil, err
		}
		w.Kind = &orbitv1.Workload_Conformance{Conformance: cw}
		return w, nil
	}
	pw, err := b.population(base)
	if err != nil {
		return nil, err
	}
	w.Kind = &orbitv1.Workload_Population{Population: pw}
	return w, nil
}

func (b builder) population(base string) (*orbitv1.PopulationWorkload, error) {
	a := &orbitv1.AttachProfile{
		Count: b.u32(base + "attach.count"), Rate: b.f64(base + "attach.rate"), Concurrency: b.u32(base + "attach.concurrency"),
		Distribution: b.str(base + "attach.distribution"), DistributionSeed: int64(b.u64(base + "attach.distribution_seed")),
		Seed: b.u64(base + "attach.seed"),
	}
	if r := b.str(base + "attach.ramp"); r != "" {
		ramp, err := Ramp(r)
		if err != nil {
			return nil, err
		}
		a.Ramp = ramp
	}
	switch arr := strings.ToLower(b.str(base + "attach.arrival")); arr {
	case "", "constant", "uniform":
	case "poisson":
		a.Arrival = orbitv1.Arrival_ARRIVAL_POISSON
	default:
		return nil, fmt.Errorf("attach.arrival %q: want uniform or poisson", arr)
	}
	pw := &orbitv1.PopulationWorkload{
		Attach: a, PduSession: b.flag(base + "pdu_session"), SubscriberPool: b.str(base + "subscriber_pool"),
		SampleIntervalMs: b.u32(base + "sample_interval_ms"),
	}
	top := &orbitv1.Topology{GnbCount: b.u32(base + "topology.gnb_count"), Layout: b.str(base + "topology.layout"),
		SpacingM: b.f64(base + "topology.spacing_m")}
	sel, err := b.gnbIDs(base + "topology.gnb_ids.")
	if err != nil {
		return nil, err
	}
	top.GnbIds = sel
	if !proto.Equal(top, &orbitv1.Topology{}) {
		pw.Topology = top
	}
	if v, ok := b.get(base + "behaviors"); ok {
		beh := &orbitv1.Behaviors{}
		if err := subtree(v, beh); err != nil {
			return nil, fmt.Errorf("behaviors: %w", err)
		}
		pw.Behaviors = beh // empty is a population that keeps its UEs
	}
	return pw, nil
}

// gnbIDs is a gnb_ids selection: first pins, assign takes from the
// server's rotation; nil when neither is set.
func (b builder) gnbIDs(base string) (*orbitv1.GNBIDSelection, error) {
	first, hasFirst := b.get(base + "first")
	assign := b.flag(base + "assign")
	switch {
	case hasFirst && assign:
		return nil, errors.New("gnb_ids: give first or assign, not both")
	case hasFirst:
		return &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: uint32(first.(uint64))}}, nil
	case assign:
		return &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}, nil
	}
	return nil, nil
}

func (b builder) conformance(base string) (*orbitv1.ConformanceWorkload, error) {
	cw := &orbitv1.ConformanceWorkload{
		TargetRelease: b.str(base + "target_release"), PerTestTimeoutMs: b.u32(base + "per_test_timeout_ms"),
		Responder: b.str(base + "responder"), Traffic: b.str(base + "traffic"),
		SubscriberPool: b.str(base + "subscriber_pool"), N3Bind: b.str(base + "n3_bind"),
	}
	if v, ok := b.get(base + "categories"); ok {
		cw.Categories = v.([]string)
	}
	sel, err := b.gnbIDs(base + "gnb_ids.")
	if err != nil {
		return nil, err
	}
	cw.GnbIds = sel
	if v, ok := b.get(base + "nas"); ok {
		cw.Nas = &orbitv1.NASAlgorithms{}
		if err := subtree(v, cw.Nas); err != nil {
			return nil, fmt.Errorf("conformance.nas: %w", err)
		}
	}
	if v, ok := b.get(base + "expected"); ok {
		x, err := b.expected(v)
		if err != nil {
			return nil, fmt.Errorf("conformance.expected: %w", err)
		}
		cw.Expected = x
	}
	return cw, nil
}

// expected is a conformance workload's expected-failures list: a path to
// a list file (docs/conformance/expected-failures.md), read through
// WithReadFile, or the list inline. Either is checked as the server checks
// its own list.
func (b builder) expected(v any) (*orbitv1.ExpectedFailures, error) {
	var doc []byte
	where := "inline"
	switch x := v.(type) {
	case string:
		if b.o.readFile == nil {
			return nil, fmt.Errorf("%q: this command does not read files", x)
		}
		data, err := b.o.readFile(x)
		if err != nil {
			return nil, err
		}
		doc, where = data, x
	default:
		data, err := yaml.Marshal(x)
		if err != nil {
			return nil, err
		}
		doc = data
	}
	var list conformance.ExpectedFailures
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if list.Version == "" {
		list.Version = "plan" // inline lists need not say; the server checks the rest
	}
	checked, err := conformance.NewExpectedFailures(where, list)
	if err != nil {
		return nil, err
	}
	out := &orbitv1.ExpectedFailures{Core: checked.Core}
	for _, e := range checked.Entries {
		out.Entries = append(out.Entries, &orbitv1.ExpectedFailure{
			Check: e.Check, Observed: e.Observed, Finding: e.Finding, Added: e.Added, Reason: e.Reason})
	}
	return out, nil
}

// startCondition is a workload's start: {after: <workload>, phase:
// started | attached | complete}.
func startCondition(v any) (*orbitv1.StartCondition, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("start: want {after: <workload>, phase: started|attached|complete}")
	}
	st := &orbitv1.StartCondition{}
	for k, val := range m {
		s, _ := val.(string)
		switch k {
		case "after":
			st.After = s
		case "phase":
			switch strings.ToLower(s) {
			case "started":
				st.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_STARTED
			case "attached":
				st.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_ATTACHED
			case "complete":
				st.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_COMPLETE
			default:
				return nil, fmt.Errorf("start.phase %q: want started, attached or complete", s)
			}
		default:
			return nil, fmt.Errorf("start: unknown key %q", k)
		}
	}
	return st, nil
}

// subtree fills m from a whole-value run-file field (YAML as written): the
// proto's own field names, snake_case or lowerCamel.
func subtree(v any, m proto.Message) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(data, m)
}

// Ramp is a ramp's text, start:end:seconds (the --ramp form).
func Ramp(s string) (*orbitv1.Ramp, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("attach.ramp must be start:end:seconds, got %q", s)
	}
	start, e1 := strconv.ParseFloat(parts[0], 64)
	end, e2 := strconv.ParseFloat(parts[1], 64)
	secs, e3 := strconv.ParseUint(parts[2], 10, 32)
	if e1 != nil || e2 != nil || e3 != nil {
		return nil, fmt.Errorf("invalid attach.ramp %q", s)
	}
	return &orbitv1.Ramp{Start: start, End: end, Seconds: uint32(secs)}, nil
}
