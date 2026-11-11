package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/target"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// A conformance workload (R3) runs the suite as a run: leased, archived,
// reported and gated like a population. The suite itself is unchanged; the
// run's launcher drives Registry.RunStream and publishes each result into
// a suiteLive, which ConformanceService.RunConformance and GetRun read.

// defaultSuiteGNBBase is the suite's first gNB ID when nothing pins one,
// as RunConformance has always used.
const defaultSuiteGNBBase = 700

// suiteGNBName is the RANNodeName every check's gNB presents.
const suiteGNBName = "orbit-conformance"

// suiteSpec is a conformance workload, built: what the suite runs against
// and how its results are judged.
type suiteSpec struct {
	env     conformance.Env
	perTest time.Duration
	// sel narrows the suite to the checks and groups it names; the zero
	// value runs every check.
	sel      conformance.Selection
	deadline time.Duration // until.duration_ms; 0 = none
	// runStart, when set, is when the run began, which the deadline counts
	// from: in a mixed run a suite that waited gets only the time left.
	// Unset, it counts from the suite's own start.
	runStart func() time.Time
	// expected is the list results are judged against: the workload's own
	// when it gives one (source "plan"), else the server's ("server").
	expected *conformance.ExpectedFailures
	source   string
	total    int
}

// suiteLive is a conformance run's results so far. It is the run's live
// holder (RunRegistry.ConformanceLive) and, once the suite returns, its
// report: a stopped run keeps its partial results here for the archive.
type suiteLive struct {
	spec *suiteSpec

	mu        sync.Mutex
	preflight conformance.Preflight // the run's preflight, once it has run
	results   []conformance.Result
	done      bool          // the suite returned, run to the end or cut off
	wake      chan struct{} // closed and replaced on every change
}

func newSuiteLive(spec *suiteSpec) *suiteLive {
	return &suiteLive{spec: spec, wake: make(chan struct{})}
}

// setPreflight records the run's preflight.
func (l *suiteLive) setPreflight(p conformance.Preflight) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.preflight = p
	l.signalLocked()
}

// preflightSteps is the run's preflight so far, for the wire; nil before
// it has run and for a run with none.
func (l *suiteLive) preflightSteps() []*orbitv1.PreflightStep {
	l.mu.Lock()
	defer l.mu.Unlock()
	return preflightProto(l.preflight)
}

// add records one result.
func (l *suiteLive) add(r conformance.Result) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, r)
	l.signalLocked()
}

// finish marks the suite returned.
func (l *suiteLive) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done = true
	l.signalLocked()
}

func (l *suiteLive) signalLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}

// since returns the results from index i on, whether the suite has
// returned, and a channel closed on the next change.
func (l *suiteLive) since(i int) ([]conformance.Result, bool, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []conformance.Result
	if i < len(l.results) {
		out = append(out, l.results[i:]...)
	}
	return out, l.done, l.wake
}

// snapshot is every result so far, and whether the suite returned.
func (l *suiteLive) snapshot() ([]conformance.Result, bool) {
	rs, done, _ := l.since(0)
	return rs, done
}

// progress is the suite so far, for GetRun and telemetry.
func (l *suiteLive) progress() *orbitv1.ConformanceProgress {
	rs, _ := l.snapshot()
	p := &orbitv1.ConformanceProgress{Tally: &orbitv1.ConformanceTally{}, Ran: uint32(len(rs)), Total: uint32(l.spec.total),
		Preflight: l.preflightSteps()}
	for _, r := range rs {
		addToTally(p.Tally, r)
	}
	if len(rs) > 0 {
		p.LastCheck = rs[len(rs)-1].ID
	}
	return p
}

// summary is the suite's report: its results, the list they were judged
// against and its stale entries, and whether it ran to the end.
func (l *suiteLive) summary() *orbitv1.ConformanceSummary {
	rs, _ := l.snapshot()
	s := &orbitv1.ConformanceSummary{
		Tally:                  &orbitv1.ConformanceTally{},
		ExpectedFailuresCore:   l.spec.expected.Label(),
		ExpectedFailuresSource: l.spec.source,
		Stale:                  staleProto(l.spec.expected.Stale(rs)),
		Total:                  uint32(l.spec.total),
		Incomplete:             len(rs) < l.spec.total,
		Preflight:              l.preflightSteps(),
	}
	for _, r := range rs {
		addToTally(s.Tally, r)
		s.Results = append(s.Results, resultProto(r))
	}
	return s
}

// suiteRun is the conformance run's launcher: the preflight, then the
// suite, one check at a time, each result judged against the expected list
// and published. At
// the plan's deadline the suite stops where it is and the run completes,
// incomplete; a stop cancels it.
func suiteRun(live *suiteLive) engine.ConformanceRunFunc {
	spec := live.spec
	return func(ctx context.Context, emit engine.RunEventFunc) (any, error) {
		defer live.finish()
		suiteCtx := ctx
		if spec.deadline > 0 {
			at := time.Now().Add(spec.deadline)
			if spec.runStart != nil {
				at = spec.runStart().Add(spec.deadline)
			}
			var cancel context.CancelFunc
			suiteCtx, cancel = context.WithDeadline(ctx, at)
			defer cancel()
		}
		reg := conformance.NewRegistry()
		if err := spec.sel.Apply(reg); err != nil {
			return nil, err // buildSuite refused it already
		}
		reg.RunSuite(suiteCtx, spec.env, spec.perTest, conformance.Hooks{Preflight: func(p conformance.Preflight) {
			live.setPreflight(p)
			if emit == nil {
				return
			}
			for _, step := range p {
				sev := "info"
				if step.Status != conformance.PreflightOK {
					sev = "error"
				}
				emit(sev, "PREFLIGHT", "", fmt.Sprintf("%s %s", step.Step, step.Status))
			}
		}, Result: func(r conformance.Result, _, _ int) {
			one := []conformance.Result{r}
			spec.expected.Apply(one)
			live.add(one[0])
			if emit != nil {
				sev := "info"
				switch one[0].Verdict {
				case conformance.Fail, conformance.Crash, conformance.Blocked, conformance.Error:
					sev = "error"
				}
				emit(sev, "CONFORMANCE", "", fmt.Sprintf("%s %s", one[0].ID, one[0].Verdict))
			}
		}})
		if rs, _ := live.snapshot(); ctx.Err() != nil && len(rs) < spec.total && !engine.Finishing(ctx) {
			return nil, ctx.Err() // stopped: CANCELLED, partial results in live
		}
		// Every check ran, even if a stop came after the last, or the run
		// is being finished: the suite reports what it has (incomplete,
		// when cut short, so its gate fails).
		return live, nil
	}
}

// buildSuite translates a plan's one conformance workload: the target's
// core, N2 source and N3 pool, a subscriber for the UE checks, the gNB IDs
// the selected checks present (pinned, or a base from the rotation), and
// the leases all of that takes.
func (s *runService) buildSuite(p *orbitv1.RunPlan, w *orbitv1.Workload) (*planRun, error) {
	cw := w.GetConformance()
	if err := validateGates(p.GetGates(), w.GetName(), true); err != nil {
		return nil, err
	}
	until := p.GetUntil()
	if until.GetForever() {
		return nil, errors.New("a conformance workload finishes on its own: use until.complete or until.duration, not until.forever")
	}
	// The NAS algorithms the UE advertises, checked as the CLI's
	// --nas-integrity and --nas-ciphering are (suiteNAS).
	if _, _, err := suiteNAS(cw); err != nil {
		return nil, err
	}
	t := p.GetTarget()
	core := t.GetCore()
	if core.GetAmf() == "" || core.GetPlmn().GetMcc() == "" || core.GetPlmn().GetMnc() == "" {
		return nil, errors.New("the target needs core.amf and core.plmn")
	}
	release := conformance.DefaultTargetRelease
	if r := cw.GetTargetRelease(); r != "" {
		var err error
		if release, err = conformance.ParseRelease(r); err != nil {
			return nil, fmt.Errorf("target_release: %w", err)
		}
	}
	slice := gnb.SNSSAI{SST: 1}
	if sl := core.GetSlices(); len(sl) > 0 {
		if err := gnb.ValidateSNSSAI(sl[0].GetSst(), sl[0].GetSd()); err != nil {
			return nil, err
		}
		slice = gnb.SNSSAI{SST: uint8(sl[0].GetSst()), SD: sl[0].GetSd()}
	}
	tac := core.GetTac()
	if tac == 0 {
		tac = 1
	}
	// The suite's associations leave from the pool's first address; the
	// second, when the pool has one, is for the checks that hold two
	// associations from two addresses.
	n2, err := target.ExpandPool(t.GetRan().GetN2SourceIps(), 2)
	if err != nil {
		return nil, fmt.Errorf("ran.n2_source_ips: %w", err)
	}
	var bind, secondBind string
	if len(n2) > 0 {
		bind = n2[0]
	}
	if len(n2) > 1 {
		secondBind = n2[1]
	}
	if err := conformance.ValidateBind(bind); err != nil {
		return nil, fmt.Errorf("ran.n2_source_ips: %w", err)
	}
	env := conformance.Env{
		AMFAddr: core.GetAmf(), Bind: bind, UPFN3: core.GetUpfN3(), N3Bind: cw.GetN3Bind(),
		TargetRelease: release, SpecTargets: conformance.DefaultSpecTargets(),
		GNB: gnb.Config{IDBits: int(gnbBits(t)), Name: suiteGNBName, MCC: core.GetPlmn().GetMcc(),
			MNC: core.GetPlmn().GetMnc(), TAC: tac, Slices: []gnb.SNSSAI{slice}},
	}
	if err := env.GNB.ValidateIdentity(); err != nil {
		return nil, err
	}
	if err := conformance.CategoriesRemoved(cw.GetCategories(), release); err != nil { //nolint:staticcheck // read only to refuse it
		return nil, err
	}
	// The selection narrows the registry for everything below: its count,
	// its needs and the gNB IDs it leases. Group titles are read in the
	// run's target release.
	sel := conformance.Selection{Checks: cw.GetChecks(), Groups: cw.GetGroups(), Tags: cw.GetTags(), Release: release,
		Pack: cw.GetPack(), Profile: core.GetProfile()}
	reg := conformance.NewRegistry()
	if err := reg.UsePack(sel.Pack, sel.Profile); err != nil {
		return nil, fmt.Errorf("pack: %w", err)
	}
	if err := reg.Only(sel.Checks...); err != nil {
		return nil, fmt.Errorf("checks: %w; `orbit conformance --list` names the checks", err)
	}
	if err := reg.Groups(sel.Release, sel.Groups...); err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	if err := reg.Tagged(sel.Tags...); err != nil {
		return nil, fmt.Errorf("tags: %w", err)
	}
	if len(reg.Select()) == 0 {
		return nil, fmt.Errorf("no checks selected: none of the checks %v is in the groups %v with the tags %v", sel.Checks, sel.Groups, sel.Tags)
	}
	// A target that bounds its gNB IDs gets the packed layout, three IDs
	// per check from the base; an unbounded one (the RunConformance
	// wrapper's base 700 among them) keeps the legacy layout.
	lo, hi, ranged, err := gnbRange(t)
	if err != nil {
		return nil, err
	}
	if span := reg.PackedSpan(); ranged && uint64(hi)-uint64(lo)+1 < uint64(span) {
		return nil, fmt.Errorf("the suite needs a block of %d gNB IDs (3 for each of its %d checks and 1 for its preflight); the target's ran.gnb_ids %d..%d holds %d",
			span, (span-1)/3, lo, hi, uint64(hi)-uint64(lo)+1)
	}
	env.PackedGNBIDs = ranged
	needs := reg.Needs(0, ranged)
	// The second address is taken, and leased, only for a selection with a
	// check that uses it.
	if !needs.SecondN2Source {
		secondBind = ""
	}
	if err := conformance.ValidateBind(secondBind); err != nil {
		return nil, fmt.Errorf("ran.n2_source_ips: %w", err)
	}
	env.SecondBind = secondBind
	ueSpec, block, err := suiteUE(t, cw, env.GNB, core.GetDnn(), needs.UEs)
	if err != nil {
		return nil, err
	}
	if block != nil {
		// Each UE check registers its own subscribers, as far as the pool
		// goes: the lease is every check's block, whatever the selection,
		// since a check's block follows from its place in the registry.
		ueSpec.Subscribers = suiteSubscribers(t, cw, reg.SubscriberSpan(), needs.UEs)
		block.Last = block.First + uint64(ueSpec.Subscribers) - 1
	}
	env.UE = ueSpec
	// The responder is the N6-side downlink trigger of the checks that
	// need a CM-IDLE UE paged; without one they SKIP.
	if name := cw.GetResponder(); name != "" {
		r, err := responderFor(t, name)
		if err != nil {
			return nil, fmt.Errorf("conformance responder: %w", err)
		}
		if r.GetControl() == "" {
			return nil, fmt.Errorf("conformance responder %q has no control address", name)
		}
		env.Downlink = engine.ResponderDownlink{Peer: r.GetControl(), Token: r.GetToken()}
	}

	spec := &suiteSpec{env: env, sel: sel, total: len(reg.Select()),
		deadline: time.Duration(until.GetDurationMs()) * time.Millisecond}
	spec.perTest = time.Duration(cw.GetPerTestTimeoutMs()) * time.Millisecond
	spec.expected, spec.source = s.expected, "server"
	if s.expected == nil {
		spec.source = ""
	}
	if x := cw.GetExpected(); x != nil {
		own, err := conformance.NewExpectedFailures("expected", expectedFrom(x))
		if err != nil {
			return nil, err
		}
		spec.expected, spec.source = own, "plan"
	}

	// The gNB IDs: every selected check's IDs in its layout, from one base.
	plmn := core.GetPlmn().GetMcc() + "/" + core.GetPlmn().GetMnc()
	out := &planRun{workload: w.GetName(), amf: core.GetAmf(), plmn: plmn}
	// pattern is the IDs the selected checks present, as offsets from the
	// suite's base; a subset's first is its first check's registry index,
	// not 0, since RunStream adds the full index.
	pattern := needs.GNBIDs
	p0 := pattern[0]
	var base uint32
	if cw.GetGnbIds().GetAssign() {
		if s.rotation == nil {
			return nil, errors.New("gnb_ids.assign: this server has no gNB ID rotation")
		}
		out.amfKeys = s.reg.AMFIdentity(core.GetAmf())
		shifted := make([]uint32, len(pattern))
		for i, off := range pattern {
			shifted[i] = off - p0
		}
		first, ids, err := s.rotation.AssignPattern(out.amfKeys, plmn, max(lo, p0), hi, shifted, s.reg.GNBInUse(core.GetAmf(), plmn))
		if err != nil {
			return nil, err
		}
		base, out.assigned = first-p0, ids
	} else {
		base = cw.GetGnbIds().GetFirst()
		if base == 0 {
			base = t.GetRan().GetGnbIds().GetFirst()
		}
		if base == 0 {
			base = defaultSuiteGNBBase
		}
		low, last := uint64(base)+uint64(p0), uint64(base)+uint64(pattern[len(pattern)-1])
		if ranged && (low < uint64(lo) || last > uint64(hi)) {
			return nil, fmt.Errorf("the suite's gNB IDs %d..%d fall outside the target's ran.gnb_ids %d..%d", low, last, lo, hi)
		}
		if last > uint64(1)<<gnbBits(t)-1 {
			return nil, fmt.Errorf("the suite's gNB IDs %d..%d (its checks, handover targets, liveness probes and preflight) do not fit %d bits", low, last, gnbBits(t))
		}
	}
	env.GNB.ID = base
	spec.env = env

	out.needs = engine.Needs{}
	for _, off := range pattern {
		out.needs.GNBs = append(out.needs.GNBs, engine.GNBKey{AMF: core.GetAmf(), PLMN: plmn, ID: base + off})
	}
	for _, b := range []string{bind, secondBind} {
		if b != "" {
			out.needs.N2IPs = append(out.needs.N2IPs, b)
		}
	}
	if block != nil {
		out.needs.SUPIs = []engine.SUPIBlock{*block}
	}
	out.suiteLive = newSuiteLive(spec)
	out.suite = suiteRun(out.suiteLive)
	return out, nil
}

// suiteUE is the UE checks' subscriber: the workload's pool (by name, else
// the first) when the target has one, with the gNB N3 address from the
// target's ran.n3_ips; none without a pool (those checks SKIP). block is
// the SUPIs it leases: as many as the most any selected check registers
// at once, none when no selected check needs a UE.
func suiteUE(t *orbitv1.ResolvedTarget, cw *orbitv1.ConformanceWorkload, g gnb.Config, dnn string, ues int) (conformance.UESpec, *engine.SUPIBlock, error) {
	// traffic and nas describe the UE: with none, they are refused, as
	// the CLI's --local refuses --traffic and --nas-* without --supi.
	if cw.GetTraffic() != "" || len(cw.GetNas().GetIntegrity()) > 0 || len(cw.GetNas().GetCiphering()) > 0 {
		hasUE := false
		if sp, err := subscriberPool(t, cw.GetSubscriberPool()); err == nil && len(t.GetSubscribers()) > 0 && sp.GetFirstSupi() != "" {
			hasUE = true
		}
		if !hasUE {
			return conformance.UESpec{}, nil, errors.New("traffic and nas describe the UE checks' UE and need a subscriber pool with a first_supi")
		}
	}
	noUE := func() (conformance.UESpec, *engine.SUPIBlock, error) { return conformance.UESpec{}, nil, nil }
	if ues == 0 {
		// No UE check selected needs a subscriber, so none is leased (no
		// block). A pool that names a valid one still gives the suite its
		// UE spec; anything else is no UE and no error.
		if spec, _, err := suiteUE(t, cw, g, dnn, 1); err == nil {
			return spec, nil, nil
		}
		return conformance.UESpec{}, nil, nil
	}
	if len(t.GetSubscribers()) == 0 {
		// The UE checks SKIP ("no UE configured"), as the CLI's --local
		// does with no --supi.
		return noUE()
	}
	sp, err := subscriberPool(t, cw.GetSubscriberPool())
	if err != nil {
		return conformance.UESpec{}, nil, err
	}
	supi := sp.GetFirstSupi()
	if supi == "" {
		// Keys with no SUPI (say $ORBIT_KI exported) name no UE: the UE
		// checks SKIP, as with no pool.
		return noUE()
	}
	if _, err := ue.ParseIdentity(supi, g.MCC, g.MNC, "0"); err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("subscriber pool %q: %w", sp.GetName(), err)
	}
	if _, err := auth.ParseHexKey("Ki", sp.GetKi()); err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("subscriber pool %q: %w", sp.GetName(), err)
	}
	if _, err := auth.ParseHexKey("OPc", sp.GetOpc()); err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("subscriber pool %q: %w", sp.GetName(), err)
	}
	n3, err := target.ExpandPool(t.GetRan().GetN3Ips(), 1)
	if err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("ran.n3_ips: %w", err)
	}
	if len(n3) == 0 {
		return conformance.UESpec{}, nil, errors.New("the UE checks need the target's ran.n3_ips (the gNB N3 address)")
	}
	gnbN3, err := gnb.CanonicalN3(n3[0])
	if err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("ran.n3_ips: %w", err)
	}
	if tr := cw.GetTraffic(); tr != "" {
		if _, _, err := net.SplitHostPort(tr); err != nil {
			return conformance.UESpec{}, nil, fmt.Errorf("traffic: want host:port: %w", err)
		}
	}
	if dnn == "" {
		dnn = "internet"
	}
	if ues > 0 && sp.GetCount() > 0 && uint32(ues) > sp.GetCount() {
		return conformance.UESpec{}, nil, fmt.Errorf("the selected checks register %d UEs at once; subscriber pool %q holds %d", ues, sp.GetName(), sp.GetCount())
	}
	ia, ea, err := suiteNAS(cw)
	if err != nil {
		return conformance.UESpec{}, nil, err
	}
	spec := conformance.UESpec{SUPI: supi, Ki: sp.GetKi(), OPc: sp.GetOpc(), DNN: dnn, GNBN3: gnbN3, Traffic: cw.GetTraffic(),
		NASIntegrity: ia, NASCiphering: ea}
	norm, err := ue.NormalizeSUPI(supi)
	if err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("subscriber pool %q: %w", sp.GetName(), err)
	}
	first, err := strconv.ParseUint(norm, 10, 64)
	if err != nil {
		return conformance.UESpec{}, nil, fmt.Errorf("subscriber pool %q: first_supi %q: %w", sp.GetName(), supi, err)
	}
	return spec, &engine.SUPIBlock{PLMN: g.MCC + "/" + g.MNC, First: first, Last: first + uint64(ues) - 1}, nil
}

// suiteSubscribers is how many of the pool's subscribers a suite whose
// checks take span between them is spread over: span, or the pool's count
// when that is smaller (the checks then share), and never fewer than ues,
// the most one check registers at once. A pool that states no count gives
// only ues: the suite cannot know how far it reaches.
func suiteSubscribers(t *orbitv1.ResolvedTarget, cw *orbitv1.ConformanceWorkload, span, ues int) int {
	sp, err := subscriberPool(t, cw.GetSubscriberPool())
	if err != nil || sp.GetCount() == 0 {
		return ues
	}
	return max(ues, min(span, int(sp.GetCount())))
}

// expectedFrom is a plan's expected-failures list in the suite's form.
func expectedFrom(x *orbitv1.ExpectedFailures) conformance.ExpectedFailures {
	out := conformance.ExpectedFailures{Core: x.GetCore(), Version: "plan"}
	for _, e := range x.GetEntries() {
		out.Entries = append(out.Entries, conformance.ExpectedFailureEntry{
			Check: e.GetCheck(), Verdict: conformance.Verdict(e.GetVerdict()), Observed: e.GetObserved(), Finding: e.GetFinding(), Added: e.GetAdded(), Reason: e.GetReason()})
	}
	return out
}

// suiteLive is a conformance run's live results, while the registry keeps
// the run; nil otherwise.
func (s *runService) suiteLive(id string) *suiteLive {
	if v, ok := s.reg.ConformanceLive(id); ok {
		if l, ok := v.(*suiteLive); ok {
			return l
		}
	}
	return nil
}

// suiteNAS is a conformance workload's NAS algorithms as the UE security
// capability's bitmaps, checked as the CLI's --nas-integrity and
// --nas-ciphering are (conformance.ParseNASAlgorithms: NIA0 and unknown
// names refused). Unset lists are the defaults, 128-NIA2 and NEA0.
func suiteNAS(cw *orbitv1.ConformanceWorkload) (ia, ea uint8, err error) {
	var integrity, ciphering []string
	if l := cw.GetNas().GetIntegrity(); len(l) > 0 {
		integrity = l
	}
	if l := cw.GetNas().GetCiphering(); len(l) > 0 {
		ciphering = l
	}
	if ia, err = conformance.ParseNASAlgorithms("conformance.nas.integrity", "NIA", integrity, 1); err != nil {
		return 0, 0, err
	}
	if ea, err = conformance.ParseNASAlgorithms("conformance.nas.ciphering", "NEA", ciphering, 0); err != nil {
		return 0, 0, err
	}
	return ia, ea, nil
}
