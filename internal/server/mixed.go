package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue"
)

// A mixed plan (R3b, ADR-0008 §2) is one population and one conformance
// workload in one run: "conformance under load". Each is built as its own
// one-workload plan, on the same target with its resources kept apart
// (the next N2 source, the next SUPI, gNB IDs clear of the population's),
// and the run leases the union.

// phaseFrom is a start condition's phase in the engine's terms.
func phaseFrom(p orbitv1.WorkloadPhase) (engine.Phase, error) {
	switch p {
	case orbitv1.WorkloadPhase_WORKLOAD_PHASE_STARTED:
		return engine.PhaseStarted, nil
	case orbitv1.WorkloadPhase_WORKLOAD_PHASE_ATTACHED:
		return engine.PhaseAttached, nil
	case orbitv1.WorkloadPhase_WORKLOAD_PHASE_COMPLETE:
		return engine.PhaseComplete, nil
	}
	return 0, errors.New("give a phase: started, attached or complete")
}

// buildMixed translates a two-workload plan.
func (s *runService) buildMixed(p *orbitv1.RunPlan, verbosity engine.EventVerbosity) (*planRun, error) {
	var popW, suiteW *orbitv1.Workload
	for _, w := range p.GetWorkloads() {
		switch {
		case w.GetPopulation() != nil && popW == nil:
			popW = w
		case w.GetConformance() != nil && suiteW == nil:
			suiteW = w
		}
	}
	if popW == nil || suiteW == nil {
		return nil, fmt.Errorf("a plan with two workloads %w: give one population and one conformance workload", errNotYet)
	}
	if popW.GetName() == "" || suiteW.GetName() == "" || popW.GetName() == suiteW.GetName() {
		return nil, errors.New("workload names must be set and unique")
	}
	if popW.GetStart() != nil {
		return nil, fmt.Errorf("workload %q: a population starts with the run; give the conformance workload the start condition", popW.GetName())
	}
	var phase engine.Phase
	if st := suiteW.GetStart(); st != nil {
		if st.GetAfter() != popW.GetName() {
			return nil, fmt.Errorf("workload %q: start after %q, which the plan does not have; it can wait on %q", suiteW.GetName(), st.GetAfter(), popW.GetName())
		}
		var err error
		if phase, err = phaseFrom(st.GetPhase()); err != nil {
			return nil, fmt.Errorf("workload %q: %w", suiteW.GetName(), err)
		}
		if phase == engine.PhaseComplete && popW.GetPopulation().GetBehaviors() != nil {
			return nil, fmt.Errorf("workload %q: %q has behaviours, which run until the run ends; start after it is attached or started", suiteW.GetName(), popW.GetName())
		}
	}
	if err := validateMixedGates(p.GetGates(), popW.GetName(), suiteW.GetName()); err != nil {
		return nil, err
	}

	// The population, on its own: as a one-workload plan would run.
	popPlan := &orbitv1.RunPlan{Target: p.GetTarget(), Workloads: []*orbitv1.Workload{popW}, Until: p.GetUntil()}
	pop, err := s.buildPlanAs(popPlan, verbosity, false)
	if err != nil {
		return nil, fmt.Errorf("workload %q: %w", popW.GetName(), err)
	}
	fail := func(err error) (*planRun, error) {
		s.giveBack(pop)
		return nil, err
	}

	// The suite, on the resources the population leaves.
	suiteTarget, err := suiteShare(p.GetTarget(), popW.GetPopulation(), suiteW.GetConformance(), pop)
	if err != nil {
		return fail(err)
	}
	cw := proto.Clone(suiteW.GetConformance()).(*orbitv1.ConformanceWorkload)
	if cw.GetGnbIds() == nil && len(pop.needs.GNBs) > 0 {
		// Clear of the population's IDs: the next base above them.
		var top uint32
		for _, g := range pop.needs.GNBs {
			top = max(top, g.ID)
		}
		cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: top + 1}}
	}
	suiteUntil := p.GetUntil()
	if suiteUntil.GetForever() || suiteUntil.GetComplete() {
		suiteUntil = nil // the suite runs to its end either way
	}
	suitePlan := &orbitv1.RunPlan{Target: suiteTarget, Until: suiteUntil, Workloads: []*orbitv1.Workload{
		{Name: suiteW.GetName(), Kind: &orbitv1.Workload_Conformance{Conformance: cw}}}}
	suite, err := s.buildSuite(suitePlan, suitePlan.GetWorkloads()[0])
	if err != nil {
		return fail(fmt.Errorf("workload %q: %w", suiteW.GetName(), err))
	}
	if err := disjoint(pop.needs, suite.needs); err != nil {
		s.giveBack(suite)
		return fail(err)
	}

	// The suite knows when the population loads the core: from its
	// launcher's start to its return.
	popName := popW.GetName()
	popWork := engine.MixedWorkload{Name: popName, Count: popCount(pop), Load: pop.load, Fleet: pop.fleet}
	if pop.fleet != nil {
		popWork.Load = nil
	}
	var started func() time.Time
	suite.suiteLive.spec.env.Load, started = trackLoad(&popWork)
	// The run's deadline counts from its start, which is the population's
	// (it never waits): a suite that waited gets only the time left.
	suite.suiteLive.spec.runStart = started
	suiteWork := engine.MixedWorkload{Name: suiteW.GetName(), Conformance: suite.suite, SuiteLive: suite.suiteLive}
	if phase != 0 {
		suiteWork.After, suiteWork.Phase = popName, phase
	}

	out := &planRun{workload: popName, amf: pop.amf, plmn: pop.plmn, registered: pop.registered,
		mixed: []engine.MixedWorkload{popWork, suiteWork}, kinds: []engine.RunKind{popWork.Kind(), engine.RunKindConformance}}
	// Plan order, as the reports follow it.
	if p.GetWorkloads()[0] == suiteW {
		out.mixed[0], out.mixed[1] = out.mixed[1], out.mixed[0]
		out.kinds[0], out.kinds[1] = out.kinds[1], out.kinds[0]
	}
	out.needs = engine.Needs{
		SUPIs: append(slices.Clone(pop.needs.SUPIs), suite.needs.SUPIs...),
		GNBs:  append(slices.Clone(pop.needs.GNBs), suite.needs.GNBs...),
		N2IPs: append(slices.Clone(pop.needs.N2IPs), suite.needs.N2IPs...),
	}
	out.assigned = append(slices.Clone(pop.assigned), suite.assigned...)
	out.amfKeys = pop.amfKeys
	if out.amfKeys == nil {
		out.amfKeys = suite.amfKeys
	}
	return out, nil
}

// trackLoad wraps a population's launcher so loaded names it while the
// launcher runs, and "" before and after: the suite's view of whether the
// core is under load. started is when the launcher was first called (the
// zero time before).
func trackLoad(w *engine.MixedWorkload) (loaded func() string, started func() time.Time) {
	var active atomic.Bool
	var at atomic.Pointer[time.Time]
	begin := func() {
		now := time.Now()
		at.CompareAndSwap(nil, &now)
		active.Store(true)
	}
	if fn := w.Fleet; fn != nil {
		w.Fleet = func(ctx context.Context, st *engine.FleetLiveStats, emit engine.RunEventFunc) (engine.FleetReport, error) {
			begin()
			defer active.Store(false)
			return fn(ctx, st, emit)
		}
	}
	if fn := w.Load; fn != nil {
		w.Load = func(ctx context.Context, st *load.LiveStats, emit engine.RunEventFunc) (load.Report, error) {
			begin()
			defer active.Store(false)
			return fn(ctx, st, emit)
		}
	}
	name := w.Name
	return func() string {
			if active.Load() {
				return name
			}
			return ""
		}, func() time.Time {
			if t := at.Load(); t != nil {
				return *t
			}
			return time.Now() // not started: the deadline counts from now
		}
}

// popCount is how many UEs a built population attaches.
func popCount(pr *planRun) int {
	if pr.fleet != nil {
		return pr.built.Count
	}
	return pr.storm.Count
}

// suiteShare is the target as the suite sees it beside the population: the
// N2 source after the population's gNBs', and, when both draw on one
// subscriber pool, the SUPIs after the population's block.
func suiteShare(t *orbitv1.ResolvedTarget, pop *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload, pr *planRun) (*orbitv1.ResolvedTarget, error) {
	out := proto.Clone(t).(*orbitv1.ResolvedTarget)
	gnbs := max(int(pop.GetTopology().GetGnbCount()), 1)
	if pool := t.GetRan().GetN2SourceIps(); len(pool) > 0 {
		n2, err := target.ExpandPool(pool, gnbs+1)
		if err != nil {
			return nil, fmt.Errorf("ran.n2_source_ips: %w", err)
		}
		if len(n2) <= gnbs {
			return nil, fmt.Errorf("ran.n2_source_ips holds %d addresses: the population's %d gNBs and the suite take %d", len(n2), gnbs, gnbs+1)
		}
		out.Ran.N2SourceIps = []string{n2[gnbs]}
	}
	if len(t.GetSubscribers()) == 0 {
		return out, nil
	}
	popPool, err := subscriberPool(t, pop.GetSubscriberPool())
	if err != nil {
		return nil, err
	}
	suitePool, err := subscriberPool(t, cw.GetSubscriberPool())
	if err != nil {
		return nil, err
	}
	if popPool.GetName() != suitePool.GetName() {
		return out, nil
	}
	used := uint32(popCount(pr))
	if c := popPool.GetCount(); c > 0 && used >= c {
		return nil, fmt.Errorf("subscriber pool %q holds %d: the population takes %d and the suite needs one more", popPool.GetName(), c, used)
	}
	next, err := supiAfter(popPool.GetFirstSupi(), used)
	if err != nil {
		return nil, fmt.Errorf("subscriber pool %q: %w", popPool.GetName(), err)
	}
	for _, sp := range out.GetSubscribers() {
		if sp.GetName() == popPool.GetName() {
			sp.FirstSupi = next
			if sp.GetCount() > 0 {
				sp.Count -= used
			}
		}
	}
	return out, nil
}

// supiAfter is the SUPI n after first, as wide as first's digits.
func supiAfter(first string, n uint32) (string, error) {
	norm, err := ue.NormalizeSUPI(first)
	if err != nil {
		return "", err
	}
	v, err := strconv.ParseUint(norm, 10, 64)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", len(norm), v+uint64(n)), nil
}

// disjoint refuses two workloads' needs that overlap: one run cannot hold
// a resource twice.
func disjoint(a, b engine.Needs) error {
	for _, x := range a.GNBs {
		for _, y := range b.GNBs {
			if x == y {
				return fmt.Errorf("gNB ID %d is the population's and the suite's; pin the suite's gnb_ids clear of the population's", x.ID)
			}
		}
	}
	for _, x := range a.N2IPs {
		for _, y := range b.N2IPs {
			if hx, hy := n2Host(x), n2Host(y); hx != "" && hx == hy {
				return fmt.Errorf("N2 source %s is the population's and the suite's", hx)
			}
		}
	}
	for _, x := range a.SUPIs {
		for _, y := range b.SUPIs {
			if x.PLMN == y.PLMN && x.First <= y.Last && y.First <= x.Last {
				return fmt.Errorf("SUPIs %d..%d are the population's and the suite's", max(x.First, y.First), min(x.Last, y.Last))
			}
		}
	}
	return nil
}

// n2Host is an N2 source's address without its port ("" for none).
func n2Host(a string) string {
	if h, _, err := net.SplitHostPort(a); err == nil {
		return h
	}
	return a
}

// validateMixedGates checks a mixed plan's gates: a gate naming a
// workload must fit its kind, an SLO gate the population and a
// conformance gate the suite; a run-wide gate judges the workload its
// check fits.
func validateMixedGates(gates []*orbitv1.Gate, popName, suiteName string) error {
	var popGates, suiteGates []*orbitv1.Gate
	for _, g := range gates {
		switch g.GetWorkload() {
		case popName:
			popGates = append(popGates, g)
		case suiteName:
			suiteGates = append(suiteGates, g)
		case "":
			if g.GetConformance() != nil {
				suiteGates = append(suiteGates, g)
			} else {
				popGates = append(popGates, g)
			}
		default:
			return fmt.Errorf("gate %q: no workload %q", g.GetName(), g.GetWorkload())
		}
	}
	if err := validateGates(popGates, popName, false); err != nil {
		return err
	}
	if err := validateGates(suiteGates, suiteName, true); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, g := range gates {
		if n := g.GetName(); n != "" {
			if names[n] {
				return fmt.Errorf("gate %q: names must be unique", n)
			}
			names[n] = true
		}
	}
	return nil
}

// partialMixed is a mixed run that did not complete, as a report for the
// archive: each finished workload's report, the suite's results so far,
// and no report for the rest.
func partialMixed(ps []engine.MixedProgress) *engine.MixedReport {
	rep := &engine.MixedReport{}
	for _, p := range ps {
		res := engine.MixedResult{Name: p.Name, Kind: p.Kind, State: p.State, Detail: p.Detail, Report: p.Report}
		if l, ok := p.Suite.(*suiteLive); ok && res.Report == nil {
			res.Report = l // its results so far
		}
		rep.Workloads = append(rep.Workloads, res)
	}
	return rep
}

// workloadsProto is a mixed run's workloads as they stand.
func workloadsProto(ps []engine.MixedProgress, now time.Time) []*orbitv1.WorkloadProgress {
	out := make([]*orbitv1.WorkloadProgress, 0, len(ps))
	for _, p := range ps {
		w := &orbitv1.WorkloadProgress{Name: p.Name, Kind: runKindProto(p.Kind), State: p.State, Detail: p.Detail}
		switch {
		case p.Load != nil:
			w.Progress = &orbitv1.WorkloadProgress_Load{Load: loadProgressProto(p.Load.Snapshot())}
		case p.Fleet != nil:
			// No rate tracker: rates need consecutive samples of one
			// stream, which a workload's entry does not keep.
			w.Progress = &orbitv1.WorkloadProgress_Fleet{Fleet: fleetProgressProto(p.Fleet.Snapshot(), now, nil)}
		default:
			if l, ok := p.Suite.(*suiteLive); ok {
				w.Progress = &orbitv1.WorkloadProgress_Conformance{Conformance: l.progress()}
			}
		}
		out = append(out, w)
	}
	return out
}

// partialPopulation is a one-population plan run that did not complete, as
// a one-workload mixed report for the archive: its figures so far, from
// its live aggregates, and how it ended. nil for any other run.
func (s *runService) partialPopulation(id string, info engine.RunInfo, plan *orbitv1.RunPlan) *engine.MixedReport {
	ws := plan.GetWorkloads()
	if len(ws) != 1 || ws[0].GetPopulation() == nil {
		return nil
	}
	res := engine.MixedResult{Name: ws[0].GetName(), Kind: info.Kind}
	switch info.Kind {
	case engine.RunKindLoad:
		snap, ok := s.reg.Snapshot(id)
		if !ok {
			return nil
		}
		res.Report = engine.PartialLoadReport(snap)
	case engine.RunKindFleet:
		snap, ok := s.reg.FleetSnapshot(id)
		if !ok {
			return nil
		}
		res.Report = engine.PartialFleetReport(snap)
	default:
		return nil
	}
	switch info.State {
	case engine.RunCancelled:
		res.State, res.Detail = engine.WorkloadStopped, "the run was stopped"
	case engine.RunFailed:
		res.State, res.Detail = engine.WorkloadFailed, info.Err
	default:
		return nil
	}
	return &engine.MixedReport{Workloads: []engine.MixedResult{res}}
}
