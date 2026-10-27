package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// mixedPlan is a population ("pop", 2 UEs) and a conformance workload
// ("suite") on one target with room for both, changed by each mut.
func mixedPlan(mut ...func(p *orbitv1.RunPlan, pop *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload)) *orbitv1.RunPlan {
	p := loadPlan()
	p.Provenance = nil
	p.Target.Ran.N2SourceIps = []string{"127.0.0.10-127.0.0.20"}
	p.Target.Ran.GnbIds = nil
	cw := &orbitv1.ConformanceWorkload{PerTestTimeoutMs: 2000, Categories: []string{"negative-ie"}}
	p.Workloads = append(p.Workloads, &orbitv1.Workload{Name: "suite", Kind: &orbitv1.Workload_Conformance{Conformance: cw},
		Start: &orbitv1.StartCondition{After: "pop", Phase: orbitv1.WorkloadPhase_WORKLOAD_PHASE_ATTACHED}})
	p.Until = &orbitv1.Until{Stop: &orbitv1.Until_Complete{Complete: true}}
	for _, m := range mut {
		m(p, pop(p), cw)
	}
	return p
}

// A mixed plan is one population and one conformance workload, the suite
// waiting on the population if on anything; resources it cannot keep apart
// are refused.
func TestMixedShapeRefusals(t *testing.T) {
	type mutFn = func(*orbitv1.RunPlan, *orbitv1.PopulationWorkload, *orbitv1.ConformanceWorkload)
	slo := func(w string) *orbitv1.Gate {
		return &orbitv1.Gate{Name: "s", Workload: w, Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE}}}
	}
	conf := func(w string) *orbitv1.Gate {
		return &orbitv1.Gate{Name: "c", Workload: w, Check: &orbitv1.Gate_Conformance{Conformance: &orbitv1.ConformanceGate{}}}
	}
	tests := []struct {
		name    string
		mut     mutFn
		wantErr string
	}{
		{name: "two populations", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Workloads[1] = &orbitv1.Workload{Name: "pop2", Kind: p.Workloads[0].Kind}
		}, wantErr: "give one population and one conformance workload"},
		{name: "duplicate names", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Workloads[1].Name, p.Workloads[1].Start = "pop", nil
		}, wantErr: "names must be set and unique"},
		{name: "a population waiting", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Workloads[0].Start = &orbitv1.StartCondition{After: "suite", Phase: orbitv1.WorkloadPhase_WORKLOAD_PHASE_STARTED}
		}, wantErr: "a population starts with the run"},
		{name: "after an unknown workload", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Workloads[1].Start.After = "nope"
		}, wantErr: `start after "nope", which the plan does not have`},
		{name: "after with no phase", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Workloads[1].Start.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_UNSPECIFIED
		}, wantErr: "give a phase"},
		{name: "after a population with behaviours completes", mut: func(p *orbitv1.RunPlan, pop *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			pop.Behaviors = &orbitv1.Behaviors{Latency: &orbitv1.LatencyProbe{Target: "127.0.0.1"}}
			p.Workloads[1].Start.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_COMPLETE
		}, wantErr: "has behaviours, which run until the run ends"},
		{name: "an slo gate on the suite", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Gates = []*orbitv1.Gate{slo("suite")}
		}, wantErr: "an slo gate judges a population"},
		{name: "a conformance gate on the population", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Gates = []*orbitv1.Gate{conf("pop")}
		}, wantErr: "a conformance gate judges a conformance workload"},
		{name: "a gate on no workload", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Gates = []*orbitv1.Gate{slo("other")}
		}, wantErr: `no workload "other"`},
		{name: "one N2 source for both", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.N2SourceIps = []string{"127.0.0.10"}
		}, wantErr: "ran.n2_source_ips holds 1 addresses: the population's 1 gNBs and the suite take 2"},
		{name: "a pool the population fills", mut: func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
			p.Target.Subscribers[0].Count = 2
		}, wantErr: `subscriber pool "main" holds 2: the population takes 2 and the suite needs one more`},
		{name: "suite IDs over the population's", mut: func(_ *orbitv1.RunPlan, pop *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload) {
			// The suite's first check presents base + its index: pin the
			// population's one gNB there.
			first := conformance.NewRegistry().Needs(700, false, conformance.NegativeIE).GNBIDs[0]
			pop.Topology = &orbitv1.Topology{GnbIds: &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: first}}}
			cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: 700}}
		}, wantErr: "is the population's and the suite's"},
	}
	s := planService(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.buildPlan(mixedPlan(tc.mut), engine.EventsNormal)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// The suite takes the resources the population leaves: the next N2
// source, the SUPI after the population's block, and gNB IDs above its;
// the run leases the union, and keeps the plan's workload order.
func TestMixedResources(t *testing.T) {
	s := planService(t)
	pr, err := s.buildPlan(mixedPlan(func(_ *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload) {
		cw.Categories = nil // every check, so the UE checks' SUPI is leased
	}), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.mixed) != 2 || pr.mixed[0].Name != "pop" || pr.mixed[1].Name != "suite" ||
		pr.mixed[1].After != "pop" || pr.mixed[1].Phase != engine.PhaseAttached || pr.mixed[0].Count != 2 {
		t.Fatalf("workloads = %+v", pr.mixed)
	}
	if !slices.Equal(pr.kinds, []engine.RunKind{engine.RunKindLoad, engine.RunKindConformance}) {
		t.Errorf("kinds = %v", pr.kinds)
	}
	if len(pr.needs.N2IPs) != 2 || !strings.HasPrefix(pr.needs.N2IPs[0], "127.0.0.10") || pr.needs.N2IPs[1] != "127.0.0.11" {
		t.Errorf("N2 = %v, want the population's then the suite's", pr.needs.N2IPs)
	}
	if len(pr.needs.SUPIs) != 2 || pr.needs.SUPIs[1].First != pr.needs.SUPIs[0].Last+1 {
		t.Errorf("SUPIs = %+v, want the suite's after the population's", pr.needs.SUPIs)
	}
	var popTop, suiteLow uint32 = 0, ^uint32(0)
	for i, g := range pr.needs.GNBs {
		if i == 0 {
			popTop = g.ID
		} else {
			suiteLow = min(suiteLow, g.ID)
		}
	}
	if suiteLow <= popTop {
		t.Errorf("suite gNB IDs from %d, population's up to %d", suiteLow, popTop)
	}
	ue := pr.mixed[1].SuiteLive.(*suiteLive).spec.env.UE
	if ue.SUPI != "208930100007502" {
		t.Errorf("suite UE = %s, want the SUPI after the population's two", ue.SUPI)
	}
	// Plan order: the suite first.
	rev, err := s.buildPlan(mixedPlan(func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
		p.Workloads[0], p.Workloads[1] = p.Workloads[1], p.Workloads[0]
	}), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	if rev.mixed[0].Name != "suite" || rev.kinds[0] != engine.RunKindConformance {
		t.Errorf("reversed plan's workloads = %s %v", rev.mixed[0].Name, rev.kinds)
	}
}

// The suite sees the core under load exactly while the population's
// launcher runs.
func TestTrackLoad(t *testing.T) {
	var during string
	var loaded func() string
	w := engine.MixedWorkload{Name: "pop", Load: func(context.Context, *load.LiveStats, engine.RunEventFunc) (load.Report, error) {
		during = loaded()
		return load.Report{}, nil
	}}
	loaded, _ = trackLoad(&w)
	if loaded() != "" {
		t.Error("loaded before the population started")
	}
	if _, err := w.Load(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if during != "pop" || loaded() != "" {
		t.Errorf("during = %q, after = %q; want pop, then none", during, loaded())
	}
}

// A population and a suite under it run as one MIXED run against a mock
// core: listed under each workload's kind, reported per workload with
// run-wide gates judging the workload each fits, and archived.
func TestMixedRunAgainstMock(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", archKi)
	opc, _ := auth.ParseHexKey("OPc", archOpc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpfront.Listen(t, amf)
	dir, runs, _ := suiteServer(t)
	p := mixedPlan(func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = addr
		p.Target.Ran.N2SourceIps = nil // route-selected, for the loopback mock
		p.Gates = []*orbitv1.Gate{
			{Name: "attach", Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE, Threshold: 0.5}}},
			{Name: "suite", Check: &orbitv1.Gate_Conformance{Conformance: &orbitv1.ConformanceGate{}}},
		}
	})
	resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	if k := resp.Msg.GetRun().GetKind(); k != orbitv1.RunKind_RUN_KIND_MIXED {
		t.Errorf("kind = %v", k)
	}
	var r *orbitv1.GetRunReportResponse
	for deadline := time.Now().Add(60 * time.Second); r == nil; time.Sleep(25 * time.Millisecond) {
		got, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
		switch {
		case err == nil:
			r = got.Msg
		case connect.CodeOf(err) != connect.CodeFailedPrecondition || time.Now().After(deadline):
			t.Fatalf("GetRunReport: %v", err)
		}
	}
	ws := r.GetWorkloads()
	if len(ws) != 2 || ws[0].GetLoad() == nil || ws[1].GetConformance() == nil || ws[0].GetLoad().GetSucceeded() != 2 {
		t.Fatalf("workloads = %v", ws)
	}
	if len(ws[0].GetGates()) != 1 || ws[0].GetGates()[0].GetGate().GetName() != "attach" ||
		len(ws[1].GetGates()) != 1 || ws[1].GetGates()[0].GetGate().GetName() != "suite" || len(r.GetGates()) != 2 {
		t.Errorf("gates: pop %v, suite %v, run %v", ws[0].GetGates(), ws[1].GetGates(), r.GetGates())
	}
	for _, k := range []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_MIXED, orbitv1.RunKind_RUN_KIND_LOAD, orbitv1.RunKind_RUN_KIND_CONFORMANCE} {
		l, err := runs.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{Kind: k}))
		if err != nil || len(l.Msg.GetRuns()) != 1 {
			t.Errorf("ListRuns(%v) = %v, %v", k, l.Msg.GetRuns(), err)
		}
	}
	if l, _ := runs.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{Kind: orbitv1.RunKind_RUN_KIND_FLEET})); len(l.Msg.GetRuns()) != 0 {
		t.Errorf("ListRuns(FLEET) found the run")
	}
	g, err := runs.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
	if err != nil || len(g.Msg.GetWorkloads()) != 2 || g.Msg.GetWorkloads()[1].GetConformance().GetRan() == 0 ||
		g.Msg.GetWorkloads()[0].GetState() != engine.WorkloadDone {
		t.Errorf("GetRun workloads = %v, %v", g.Msg.GetWorkloads(), err)
	}
	if got := g.Msg.GetRun().GetWorkloadKinds(); !slices.Equal(got, []orbitv1.RunKind{orbitv1.RunKind_RUN_KIND_LOAD, orbitv1.RunKind_RUN_KIND_CONFORMANCE}) {
		t.Errorf("workload kinds = %v", got)
	}
	a := waitArchive(t, dir, id)
	if len(a.GetReports()) != 2 {
		t.Errorf("archived reports = %d, want 2", len(a.GetReports()))
	}
}

// Stopped, a mixed run archives each workload's partial report: the
// suite's results so far, and its gate failed as an incomplete suite's.
func TestMixedStoppedArchivesPartials(t *testing.T) {
	amfAddr := idleAMF(t) // nothing completes, so the stop finds both running
	dir, runs, _ := suiteServer(t)
	p := mixedPlan(func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = amfAddr
		p.Target.Ran.N2SourceIps = nil
		p.Workloads[1].Start.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_STARTED
		cw.PerTestTimeoutMs = 1000
		p.Gates = []*orbitv1.Gate{{Name: "suite", Check: &orbitv1.Gate_Conformance{Conformance: &orbitv1.ConformanceGate{}}}}
	})
	resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	time.Sleep(1500 * time.Millisecond) // a check or so
	if _, err := runs.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id})); err != nil {
		t.Fatal(err)
	}
	a := waitArchive(t, dir, id)
	if len(a.GetReports()) != 2 {
		t.Fatalf("archived reports = %v", a.GetReports())
	}
	if a.GetReports()[0].GetLoad() == nil {
		t.Errorf("the population's partial report is missing: %v", a.GetReports()[0])
	}
	suite := a.GetReports()[1]
	if !suite.GetConformance().GetIncomplete() || suite.GetGatesPassed() {
		t.Errorf("suite report = incomplete %t, gates passed %t", suite.GetConformance().GetIncomplete(), suite.GetGatesPassed())
	}
}

// Two workloads' needs that share a resource are refused, whatever its
// spelling.
func TestDisjoint(t *testing.T) {
	gnb := func(id uint32) engine.GNBKey { return engine.GNBKey{AMF: "amf", PLMN: "208/93", ID: id} }
	tests := []struct {
		name    string
		a, b    engine.Needs
		wantErr string
	}{
		{name: "apart", a: engine.Needs{GNBs: []engine.GNBKey{gnb(1)}, N2IPs: []string{"10.0.0.1:0"}, SUPIs: []engine.SUPIBlock{{PLMN: "208/93", First: 1, Last: 2}}},
			b: engine.Needs{GNBs: []engine.GNBKey{gnb(2)}, N2IPs: []string{"10.0.0.2"}, SUPIs: []engine.SUPIBlock{{PLMN: "208/93", First: 3, Last: 3}}}},
		{name: "a shared gNB ID", a: engine.Needs{GNBs: []engine.GNBKey{gnb(1)}}, b: engine.Needs{GNBs: []engine.GNBKey{gnb(1)}}, wantErr: "gNB ID 1"},
		{name: "one N2 source, two spellings", a: engine.Needs{N2IPs: []string{"10.0.0.1:0"}}, b: engine.Needs{N2IPs: []string{"10.0.0.1"}}, wantErr: "N2 source 10.0.0.1"},
		{name: "route-selected for both", a: engine.Needs{N2IPs: []string{""}}, b: engine.Needs{N2IPs: []string{""}}},
		{name: "overlapping SUPIs", a: engine.Needs{SUPIs: []engine.SUPIBlock{{PLMN: "208/93", First: 1, Last: 5}}},
			b: engine.Needs{SUPIs: []engine.SUPIBlock{{PLMN: "208/93", First: 5, Last: 5}}}, wantErr: "SUPIs 5..5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := disjoint(tc.a, tc.b)
			if (tc.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// The run's deadline counts from the run's start: a suite that waited on a
// slow attach gets only the time left, and is cut off incomplete.
func TestSuiteDeadlineFromRunStart(t *testing.T) {
	s := planService(t)
	pr, err := s.buildPlan(mixedPlan(func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = "127.0.0.1:1"
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 1000}}
		cw.Categories = nil
	}), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	live := pr.mixed[1].SuiteLive.(*suiteLive)
	if live.spec.runStart == nil || live.spec.deadline != time.Second {
		t.Fatalf("suite deadline %v, runStart set %t; want the run's 1s from its start", live.spec.deadline, live.spec.runStart != nil)
	}
	// The population started 10s ago (a slow attach the suite waited
	// out): the run's deadline has passed before the suite begins.
	live.spec.runStart = func() time.Time { return time.Now().Add(-10 * time.Second) }
	start := time.Now()
	if _, err := pr.mixed[1].Conformance(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the suite ran %v past the run's deadline", took)
	}
	if c := live.summary(); !c.GetIncomplete() || len(c.GetResults()) >= int(c.GetTotal()) {
		t.Errorf("summary: %d of %d, incomplete %t", len(c.GetResults()), c.GetTotal(), c.GetIncomplete())
	}
}

// A mixed plan on a target that bounds its gNB IDs keeps both workloads
// inside the block: the suite packed above the population's IDs.
func TestMixedInABoundedBlock(t *testing.T) {
	s := planService(t)
	pr, err := s.buildPlan(mixedPlan(func(p *orbitv1.RunPlan, _ *orbitv1.PopulationWorkload, cw *orbitv1.ConformanceWorkload) {
		p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 5000, Count: 200}
		cw.Categories = nil
	}), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range pr.needs.GNBs {
		if g.ID < 5000 || g.ID > 5199 {
			t.Errorf("gNB ID %d outside the block", g.ID)
		}
	}
	if !pr.mixed[1].SuiteLive.(*suiteLive).spec.env.PackedGNBIDs {
		t.Error("the suite is not packed in a bounded block")
	}
}

// A suite stopped after its last check ran is done, not stopped.
func TestSuiteStoppedAfterTheLastCheckIsDone(t *testing.T) {
	s := planService(t)
	pr, err := s.buildPlan(suitePlan(func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = "127.0.0.1:1"
		cw.Categories = []string{"procedural"}
	}), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopAtEnd := func(sev, kind, supi, msg string) {
		if rs, _ := pr.suiteLive.snapshot(); len(rs) == pr.suiteLive.spec.total {
			cancel() // the stop lands as the last result is published
		}
	}
	rep, err := pr.suite(ctx, stopAtEnd)
	if err != nil || rep == nil {
		t.Fatalf("suite = %v, %v; want done", rep, err)
	}
}

// A stopped workload's partial report is kept, but its gates fail: it
// measured a run cut short, whatever its figures say.
func TestMixedStoppedWorkloadFailsItsGates(t *testing.T) {
	gate := &orbitv1.Gate{Name: "reg", Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{Metric: "registration.p99_ms", Op: orbitv1.CompareOp_COMPARE_OP_LE, Threshold: 500}}}
	partial := &load.Report{Attempted: 10, Succeeded: 10, Latencies: map[string]load.Stats{"registration": {Count: 10, P99: time.Millisecond}}}
	tests := []struct {
		name     string
		state    string
		wantPass bool
	}{
		{name: "done", state: engine.WorkloadDone, wantPass: true},
		{name: "stopped", state: engine.WorkloadStopped, wantPass: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := mixedRunReport(engine.MixedResult{Name: "pop", Kind: engine.RunKindLoad, State: tc.state, Detail: "the run was stopped", Report: partial},
				[]*orbitv1.Gate{gate})
			if rr.GetLoad() == nil {
				t.Fatal("the report was dropped")
			}
			if len(rr.GetGates()) != 1 || rr.GetGates()[0].GetPassed() != tc.wantPass || rr.GetGatesPassed() != tc.wantPass {
				t.Errorf("gates = %v, passed %t; want %t", rr.GetGates(), rr.GetGatesPassed(), tc.wantPass)
			}
			if !tc.wantPass && rr.GetGates()[0].GetDetail() != "workload pop did not complete (stopped): the run was stopped" {
				t.Errorf("detail = %q", rr.GetGates()[0].GetDetail())
			}
		})
	}
}

// A suite skipped because its population ended first has no report, and
// its conformance gate fails naming why.
func TestMixedSkippedSuiteFailsItsGate(t *testing.T) {
	gate := &orbitv1.Gate{Name: "suite", Check: &orbitv1.Gate_Conformance{Conformance: &orbitv1.ConformanceGate{}}}
	rr := mixedRunReport(engine.MixedResult{Name: "suite", Kind: engine.RunKindConformance, State: engine.WorkloadSkipped,
		Detail: "pop ended without reaching attached"}, []*orbitv1.Gate{gate})
	if len(rr.GetGates()) != 1 || rr.GetGatesPassed() || rr.GetGates()[0].GetPassed() ||
		rr.GetGates()[0].GetDetail() != "workload suite did not complete (skipped): pop ended without reaching attached" {
		t.Errorf("gates = %v, passed %t", rr.GetGates(), rr.GetGatesPassed())
	}
}

// StopRun on a MIXED run whose population's partial figures would pass its
// SLO: the archived gate fails, and so do the run's gates.
func TestMixedStopFailsPassingPartials(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", archKi)
	opc, _ := auth.ParseHexKey("OPc", archOpc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpfront.Listen(t, amf)
	dir, runs, _ := suiteServer(t)
	p := mixedPlan(func(p *orbitv1.RunPlan, pop *orbitv1.PopulationWorkload, _ *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = addr
		p.Target.Ran.N2SourceIps = nil
		p.Target.Subscribers[0].Count = 1000
		pop.Attach = &orbitv1.AttachProfile{Count: 900, Rate: 20, Concurrency: 2} // still attaching at the stop
		p.Workloads[1].Start.Phase = orbitv1.WorkloadPhase_WORKLOAD_PHASE_COMPLETE
		p.Gates = []*orbitv1.Gate{{Name: "attach", Workload: "pop", Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{
			Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE, Threshold: 0.5}}}}
	})
	resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	// Let some attaches succeed, so the partial ratio would pass.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		g, err := runs.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
		if err == nil && len(g.Msg.GetWorkloads()) == 2 && g.Msg.GetWorkloads()[0].GetLoad().GetSucceeded() >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the population never attached: %v, %v", g, err)
		}
	}
	if _, err := runs.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id})); err != nil {
		t.Fatal(err)
	}
	a := waitArchive(t, dir, id)
	pop := a.GetReports()[0]
	if pop.GetLoad() == nil || pop.GetLoad().GetSucceeded() < 3 {
		t.Fatalf("the population's partial report = %v", pop.GetLoad())
	}
	if len(pop.GetGates()) != 1 || pop.GetGates()[0].GetPassed() || pop.GetGatesPassed() ||
		pop.GetGates()[0].GetDetail() != "workload pop did not complete (stopped): the run was stopped" {
		t.Errorf("population gates = %v, passed %t", pop.GetGates(), pop.GetGatesPassed())
	}
	if _, passed := gatesOf(a.GetReports()); passed {
		t.Error("the run's gates read passed on a stopped run")
	}
}

// A one-population plan stopped mid-run archives a judged partial report
// from its live figures, its gates failed as did-not-complete, though the
// partial ratio would pass; GetRunReport still refuses a CANCELLED run.
func TestPopulationStopArchivesJudgedPartial(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", archKi)
	opc, _ := auth.ParseHexKey("OPc", archOpc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpfront.Listen(t, amf)
	dir, runs, _ := suiteServer(t)
	p := loadPlan(func(p *orbitv1.RunPlan) {
		p.Target.Core.Amf = addr
		p.Target.Subscribers[0].Count = 1000
		pop(p).Attach = &orbitv1.AttachProfile{Count: 900, Rate: 20, Concurrency: 2}
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_Complete{Complete: true}}
		p.Gates = []*orbitv1.Gate{{Name: "attach", Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{
			Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE, Threshold: 0.5}}}}
	})
	resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		g, err := runs.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
		if err == nil && g.Msg.GetLoadProgress().GetSucceeded() >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the population never attached: %v, %v", g, err)
		}
	}
	if _, err := runs.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id})); err != nil {
		t.Fatal(err)
	}
	a := waitArchive(t, dir, id)
	if len(a.GetReports()) != 1 {
		t.Fatalf("archived reports = %v, want the population's partial", a.GetReports())
	}
	rr := a.GetReports()[0]
	if rr.GetLoad().GetSucceeded() < 3 || rr.GetGatesPassed() || len(rr.GetGates()) != 1 ||
		rr.GetGates()[0].GetDetail() != "workload pop did not complete (stopped): the run was stopped" {
		t.Errorf("report = %v", rr)
	}
	if _, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("GetRunReport on a CANCELLED run = %v, want FAILED_PRECONDITION", err)
	}
}

// A soak (until.forever) carries gates, judged when it is finished:
// StopRun{finish} ends it COMPLETE and stopped early, its gates judged on
// the results so far, where a plain StopRun cancels it and fails them.
func TestForeverGatesJudgedOnFinish(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", archKi)
	opc, _ := auth.ParseHexKey("OPc", archOpc)
	amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203"})
	if err != nil {
		t.Fatal(err)
	}
	addr := sctpfront.Listen(t, amf)
	for _, finish := range []bool{true, false} {
		dir, runs, _ := suiteServer(t)
		p := loadPlan(func(p *orbitv1.RunPlan) {
			p.Target.Core.Amf = addr
			p.Target.Subscribers[0].Count = 10
			pop(p).Attach = &orbitv1.AttachProfile{Count: 10, Rate: 20, Concurrency: 2}
			p.Until = &orbitv1.Until{Stop: &orbitv1.Until_Forever{Forever: true}}
			p.Gates = []*orbitv1.Gate{{Name: "attach", Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{
				Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE, Threshold: 0.5}}}}
		})
		resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
		if err != nil {
			t.Fatalf("finish %t: a forever plan with gates was refused: %v", finish, err)
		}
		id := resp.Msg.GetRun().GetRunId()
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			g, err := runs.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
			if err == nil && g.Msg.GetLoadProgress().GetSucceeded() >= 3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the soak never attached: %v, %v", g, err)
			}
		}
		if _, err := runs.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id, Finish: finish})); err != nil {
			t.Fatal(err)
		}
		if !finish {
			waitRunState(t, runs, id, orbitv1.RunState_RUN_STATE_CANCELLED)
			a := waitArchive(t, dir, id)
			if rs := a.GetReports(); len(rs) != 1 || rs[0].GetGatesPassed() {
				t.Errorf("cancelled soak: reports %v, want its gate failed", rs)
			}
			continue
		}
		run := waitRunState(t, runs, id, orbitv1.RunState_RUN_STATE_COMPLETE)
		if !run.GetStoppedEarly() {
			t.Error("a finished run is not marked stopped early")
		}
		r, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
		if err != nil {
			t.Fatal(err)
		}
		if !r.Msg.GetGatesPassed() || len(r.Msg.GetGates()) != 1 || len(r.Msg.GetWorkloads()) != 1 || !r.Msg.GetWorkloads()[0].GetStoppedEarly() {
			t.Errorf("finished soak: gates %v passed %t, workloads %v", r.Msg.GetGates(), r.Msg.GetGatesPassed(), r.Msg.GetWorkloads())
		}
		if l := r.Msg.GetLoad(); l.GetDistinctSupis() == 0 || l.GetDistinctSupis() > 10 || l.GetAttempted() < l.GetDistinctSupis() {
			t.Errorf("distinct SUPIs %d of attempted %d, want 1..10", l.GetDistinctSupis(), l.GetAttempted())
		}
	}
}
