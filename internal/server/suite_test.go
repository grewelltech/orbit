package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/conformance"
	"github.com/bgrewell/orbit/internal/engine"
)

// suitePlan is a one-conformance-workload plan on the plan fixtures'
// target, changed by each mut.
func suitePlan(mut ...func(*orbitv1.RunPlan, *orbitv1.ConformanceWorkload)) *orbitv1.RunPlan {
	cw := &orbitv1.ConformanceWorkload{PerTestTimeoutMs: 2000}
	p := &orbitv1.RunPlan{
		Target:    planTarget(),
		Workloads: []*orbitv1.Workload{{Name: "suite", Kind: &orbitv1.Workload_Conformance{Conformance: cw}}},
	}
	p.Target.Ran.GnbIds = nil
	for _, m := range mut {
		if m != nil {
			m(p, cw)
		}
	}
	return p
}

// A conformance workload builds against the target, refusing what it
// cannot run.
func TestSuiteShapeRefusals(t *testing.T) {
	tests := []struct {
		name    string
		mut     func(*orbitv1.RunPlan, *orbitv1.ConformanceWorkload)
		wantErr string
	}{
		{name: "until forever", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Until = &orbitv1.Until{Stop: &orbitv1.Until_Forever{Forever: true}}
		}, wantErr: "a conformance workload finishes on its own"},
		{name: "an slo gate", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Gates = []*orbitv1.Gate{{Name: "g", Check: &orbitv1.Gate_Slo{Slo: &orbitv1.SLOGate{Metric: metricSuccessRatio, Op: orbitv1.CompareOp_COMPARE_OP_GE}}}}
		}, wantErr: `an slo gate judges a population; "suite" is a conformance workload`},
		{name: "a responder the target lacks", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.Responder = "nope" }, wantErr: `conformance responder: the target has no responder "nope"`},
		{name: "a responder with no control address", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.App = &orbitv1.TargetApp{Responders: []*orbitv1.Responder{{Name: "bare", Token: "t"}}}
			cw.Responder = "bare"
		}, wantErr: `conformance responder "bare" has no control address`},
		{name: "a responder nobody registered", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.App = &orbitv1.TargetApp{Responders: []*orbitv1.Responder{{Name: "bare"}}}
			cw.Responder = "bare"
		}, wantErr: `no responder named "bare" is registered`},
		{name: "nas NIA0", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Nas = &orbitv1.NASAlgorithms{Integrity: []string{"NIA0"}}
		}, wantErr: "conformance.nas.integrity: NIA0 is refused"},
		{name: "nas an unknown name", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Nas = &orbitv1.NASAlgorithms{Ciphering: []string{"NEA9"}}
		}, wantErr: `conformance.nas.ciphering: "NEA9" is not one of NEA0 to NEA3`},
		{name: "nas without a pool", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Subscribers = nil
			cw.Nas = &orbitv1.NASAlgorithms{Integrity: []string{"NIA1"}}
		}, wantErr: "need a subscriber pool with a first_supi"},
		{name: "a bad release", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.TargetRelease = "Rel-x" }, wantErr: "target_release"},
		{name: "IDs past the ID length", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{Bits: 22}
			cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: 1<<22 - 100}}
		}, wantErr: "do not fit 22 bits"},
		{name: "a block narrower than three IDs a check", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 700, Count: 20}
		}, wantErr: fmt.Sprintf("the suite needs a block of %d gNB IDs (3 for each of its %d checks and 1 for its preflight); the target's ran.gnb_ids 700..719 holds 20",
			3*len(conformance.NewRegistry().Tests())+1, len(conformance.NewRegistry().Tests()))},
		{name: "a pin outside the target's block", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 700, Count: 200}
			cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: 890}}
		}, wantErr: "fall outside the target's ran.gnb_ids 700..899"},
		{name: "a fixed N2 port", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.N2SourceIps = []string{"127.0.0.1:5000"}
		}, wantErr: "ran.n2_source_ips"},
		{name: "a fixed port on the second N2 source", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.N2SourceIps = []string{"127.0.0.1", "127.0.0.2:5000"}
		}, wantErr: "ran.n2_source_ips"},
		{name: "a pool without the gNB N3", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Ran.N3Ips = nil }, wantErr: "ran.n3_ips"},
		{name: "traffic without a pool", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Subscribers = nil
			cw.Traffic = "127.0.0.1:9"
		}, wantErr: "need a subscriber pool with a first_supi"},
		{name: "an unknown category", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Categories = []string{"negative-ie", "bogus"}
		},
			wantErr: `unknown conformance category "bogus": want one of `},
		{name: "an unknown category, assigned", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Categories = []string{"bogus"}
			cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}
		}, wantErr: `unknown conformance category "bogus"`},
		{name: "an unknown check", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Checks = []string{"NGAP-NG-RESET-ACK", "NGAP-NO-SUCH-CHECK"}
		}, wantErr: `checks: unknown check "NGAP-NO-SUCH-CHECK"`},
		{name: "a check outside the categories", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Categories = []string{"nas"}
			cw.Checks = []string{"NGAP-NG-RESET-ACK"}
		}, wantErr: "no checks selected: none of the checks [NGAP-NG-RESET-ACK] is in the categories [nas]"},
		{name: "an expected entry for no check", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Expected = &orbitv1.ExpectedFailures{Core: "lab", Entries: []*orbitv1.ExpectedFailure{
				{Check: "NO-SUCH-CHECK", Observed: "x", Finding: "EX-1", Added: "2026-10-27", Reason: "r"}}}
		}, wantErr: `unknown check "NO-SUCH-CHECK"`},
	}
	s := planService(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.buildPlan(suitePlan(tc.mut), engine.EventsNormal)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// A suite leases what its selected checks declare: their gNB IDs from the
// base, its N2 source, the pool's second N2 address only when a selected
// check holds a second association from it, and the UE checks' subscriber
// only when one is selected and the target has a pool.
func TestSuiteLeases(t *testing.T) {
	reg := conformance.NewRegistry()
	tests := []struct {
		name     string
		mut      func(*orbitv1.RunPlan, *orbitv1.ConformanceWorkload)
		cats     []conformance.Category
		wantSUPI bool
		noUE     bool // the suite has no UE (its UE checks SKIP)
		// wantSecond is the second N2 source the suite takes, "" for none.
		wantSecond string
	}{
		{name: "a two-address pool: the second for the check that needs it", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.N2SourceIps = []string{"127.0.0.1-127.0.0.3"}
		}, wantSUPI: true, wantSecond: "127.0.0.2"},
		{name: "a two-address pool, no check that needs the second", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Ran.N2SourceIps = []string{"127.0.0.1", "127.0.0.2"}
			cw.Categories = []string{"negative-ie"}
		}, cats: []conformance.Category{conformance.NegativeIE}},
		{name: "every check", wantSUPI: true},
		{name: "negative-ie only, a valid pool: no SUPI lease", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.Categories = []string{"negative-ie"} },
			cats: []conformance.Category{conformance.NegativeIE}},
		{name: "no pool: no UE", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Subscribers = nil }, noUE: true},
		// Keys with no SUPI ($ORBIT_KI exported) name no UE: the UE checks
		// SKIP, as with no pool, and nothing is refused.
		{name: "a pool with no first_supi: no UE", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Subscribers[0].FirstSupi = ""
		}, noUE: true},
		{name: "a pool with no first_supi, no UE check: no UE", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Subscribers[0].FirstSupi = ""
			cw.Categories = []string{"negative-ie"}
		}, cats: []conformance.Category{conformance.NegativeIE}, noUE: true},
	}
	s := planService(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := s.buildPlan(suitePlan(tc.mut), engine.EventsNormal)
			if err != nil {
				t.Fatal(err)
			}
			var got []uint32
			for _, g := range pr.needs.GNBs {
				got = append(got, g.ID)
			}
			if want := reg.Needs(defaultSuiteGNBBase, false, tc.cats...).GNBIDs; !slices.Equal(got, want) {
				t.Errorf("gNB IDs = %v, want %v", got, want)
			}
			wantN2 := []string{"127.0.0.1"}
			if tc.wantSecond != "" {
				wantN2 = append(wantN2, tc.wantSecond)
			}
			if !slices.Equal(pr.needs.N2IPs, wantN2) {
				t.Errorf("N2 = %v, want %v", pr.needs.N2IPs, wantN2)
			}
			if env := pr.suiteLive.spec.env; env.Bind != "127.0.0.1" || env.SecondBind != tc.wantSecond {
				t.Errorf("suite binds %q and %q, want 127.0.0.1 and %q", env.Bind, env.SecondBind, tc.wantSecond)
			}
			// A selection with no UE check leases no SUPI, even from a
			// valid pool: leases follow the checks' needs, and an unused
			// one could refuse a concurrent run for nothing.
			want := 0
			if tc.wantSUPI {
				want = 1
			}
			if len(pr.needs.SUPIs) != want {
				t.Fatalf("SUPIs = %v, want %d", pr.needs.SUPIs, want)
			}
			if tc.wantSUPI {
				// The checks take more subscribers between them than the
				// pool's ten, so the lease is the whole pool.
				if b := pr.needs.SUPIs[0]; b.First != 208930100007500 || b.Last != b.First+9 || b.PLMN != "208/93" {
					t.Errorf("SUPI block = %+v, want the pool's ten SUPIs", b)
				}
				if got := pr.suiteLive.spec.env.UE.Subscribers; got != 10 {
					t.Errorf("UE subscribers = %d, want the pool's 10", got)
				}
			}
			if pr.suiteLive.spec.env.UE.Configured() == tc.noUE {
				t.Errorf("UE configured = %t", pr.suiteLive.spec.env.UE.Configured())
			}
		})
	}
}

// Assigned, any selection takes a block as wide as it needs, each check at
// its stable offset within it: base + its registry index, as RunStream
// presents it.
func TestSuiteAssignedSubsets(t *testing.T) {
	reg := conformance.NewRegistry()
	one := reg.Tests()[3]
	tests := []struct {
		name string
		cats []string
	}{
		{name: "nas", cats: []string{"nas"}},
		{name: "mobility", cats: []string{"mobility"}},
		{name: "a single check's category", cats: []string{string(one.Category())}},
		{name: "the full suite"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := planService(t)
			pr, err := s.buildPlan(suitePlan(func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
				cw.Categories = tc.cats
				cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}
			}), engine.EventsNormal)
			if err != nil {
				t.Fatal(err)
			}
			var cats []conformance.Category
			for _, c := range tc.cats {
				cats = append(cats, conformance.Category(c))
			}
			base := pr.suiteLive.spec.env.GNB.ID
			if want := reg.Needs(base, false, cats...).GNBIDs; !slices.Equal(pr.assigned, want) {
				t.Errorf("assigned %v; the checks present %v from base %d", pr.assigned, want, base)
			}
			// The lowest block the range allows: from its first ID, but no
			// lower than the selection's first offset, so the base is >= 0.
			if want := max(engine.DefaultGNBIDFirst, reg.Needs(0, false, cats...).GNBIDs[0]); pr.assigned[0] != want {
				t.Errorf("block starts at %d, want %d", pr.assigned[0], want)
			}
			if span := pr.assigned[len(pr.assigned)-1] - pr.assigned[0]; span > 0x9000+uint32(len(reg.Tests())) {
				t.Errorf("block spans %d IDs, wider than any selection needs", span)
			}
		})
	}
}

// Assigned, a suite takes a base whose every ID is free: two suites at
// once never share one.
func TestSuiteAssignedBases(t *testing.T) {
	s := planService(t)
	assign := func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
		cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}
	}
	a, err := s.buildPlan(suitePlan(assign), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.buildPlan(suitePlan(assign), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.assigned) == 0 || len(a.assigned) != len(a.needs.GNBs) {
		t.Fatalf("assigned %v for needs %v", a.assigned, a.needs.GNBs)
	}
	for _, id := range b.assigned {
		if slices.Contains(a.assigned, id) {
			t.Fatalf("two suites share gNB ID %d", id)
		}
	}
	if a.suiteLive.spec.env.GNB.ID != a.assigned[0] {
		t.Errorf("the suite's base %d is not its first assigned ID %d", a.suiteLive.spec.env.GNB.ID, a.assigned[0])
	}
}

// The conformance gate passes a suite that ran to the end with no FAIL
// outside the expected list and no ERROR; an incomplete suite fails.
func TestJudgeSuite(t *testing.T) {
	res := func(id, verdict string, expected bool) *orbitv1.ConformanceResult {
		r := &orbitv1.ConformanceResult{Id: id, Verdict: verdict}
		if expected {
			r.ExpectedFailure = &orbitv1.ExpectedFailure{Finding: "EX-1"}
		}
		return r
	}
	tests := []struct {
		name       string
		summary    *orbitv1.ConformanceSummary
		wantPass   bool
		wantDetail string
	}{
		{name: "clean", summary: &orbitv1.ConformanceSummary{Total: 3, Results: []*orbitv1.ConformanceResult{
			res("A", "PASS", false), res("B", "SKIP", false), res("C", "DEVIATION", false)}}, wantPass: true, wantDetail: "3 checks, no unexpected FAIL, CRASH, BLOCKED or ERROR"},
		{name: "INFO and WARN do not gate", summary: &orbitv1.ConformanceSummary{Total: 3, Results: []*orbitv1.ConformanceResult{
			res("A", "PASS", false), res("B", "INFO", false), res("C", "WARN", false)}}, wantPass: true, wantDetail: "3 checks, no unexpected FAIL, CRASH, BLOCKED or ERROR"},
		{name: "an INFO beside a FAIL does not hide it", summary: &orbitv1.ConformanceSummary{Total: 2, Results: []*orbitv1.ConformanceResult{
			res("A", "INFO", false), res("B", "FAIL", false)}}, wantDetail: "1 unexpected: B FAIL"},
		{name: "CRASH and BLOCKED gate", summary: &orbitv1.ConformanceSummary{Total: 3, Results: []*orbitv1.ConformanceResult{
			res("A", "CRASH", false), res("B", "BLOCKED", false), res("C", "PASS", false)}}, wantDetail: "2 unexpected: A CRASH, B BLOCKED"},
		{name: "an expected FAIL", summary: &orbitv1.ConformanceSummary{Total: 1, Results: []*orbitv1.ConformanceResult{
			res("A", "FAIL", true)}}, wantPass: true},
		{name: "an unexpected FAIL and an ERROR", summary: &orbitv1.ConformanceSummary{Total: 3, Results: []*orbitv1.ConformanceResult{
			res("A", "FAIL", false), res("B", "ERROR", false), res("C", "PASS", false)}}, wantDetail: "2 unexpected: A FAIL, B ERROR"},
		{name: "incomplete", summary: &orbitv1.ConformanceSummary{Total: 9, Incomplete: true, Results: []*orbitv1.ConformanceResult{
			res("A", "PASS", false), res("B", "PASS", false)}}, wantDetail: "suite incomplete: 2 of 9 checks ran"},
		{name: "no report", wantDetail: "no report to judge"},
		{name: "no checks ran", summary: &orbitv1.ConformanceSummary{}, wantDetail: "no checks ran"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pass, _, detail := judgeSuite(tc.summary)
			if pass != tc.wantPass || tc.wantDetail != "" && detail != tc.wantDetail {
				t.Errorf("judgeSuite = %t, %q; want %t, %q", pass, detail, tc.wantPass, tc.wantDetail)
			}
		})
	}
}

// suiteServer is a full server with a state directory, its run and
// conformance clients.
func suiteServer(t *testing.T) (string, orbitv1connect.RunServiceClient, orbitv1connect.ConformanceServiceClient) {
	t.Helper()
	dir := t.TempDir()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir, Responders: openResponders(t, dir)})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return dir, orbitv1connect.NewRunServiceClient(srv.Client(), srv.URL), orbitv1connect.NewConformanceServiceClient(srv.Client(), srv.URL)
}

// waitArchive reads a run's archive once it is written.
func waitArchive(t *testing.T, dir, id string) *orbitv1.RunArchive {
	t.Helper()
	path := filepath.Join(dir, id+archiveExt)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if a, err := readArchive(path); err == nil {
			return a
		}
	}
	t.Fatal("the run was never archived")
	return nil
}

// RunConformance runs the suite as a CONFORMANCE run: every update names
// it, it is listed, reported and archived with its results, and the UE's
// keys stay out of the archive.
func TestRunConformanceIsARun(t *testing.T) {
	dir, runs, conf := suiteServer(t)
	stream, err := conf.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", Mcc: "208", Mnc: "93", PerTestSeconds: 2, Categories: []string{"negative-ie"},
		Supi: "208930100007500", Credentials: &orbitv1.Credentials{Ki: archKi, Opc: archOpc}, GnbN3: "127.0.0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var id string
	var final *orbitv1.ConformanceUpdate
	n, results, preflightFirst := 0, 0, false
	for stream.Receive() {
		u := stream.Msg()
		if u.GetRunId() == "" || id != "" && u.GetRunId() != id {
			t.Fatalf("update %d names run %q (first %q)", n, u.GetRunId(), id)
		}
		id = u.GetRunId()
		if n == 0 {
			preflightFirst = u.GetResult() == nil && len(u.GetPreflight()) > 0
		}
		if u.GetDone() {
			final = u
		}
		if u.GetResult() != nil {
			results++
		}
		n++
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if final == nil || results < 1 {
		t.Fatalf("%d updates with %d results, final %v", n, results, final)
	}
	// The AMF is not there: the preflight's NG Setup fails, ahead of any
	// result, and every check reports ERROR without running.
	if !preflightFirst {
		t.Error("the first update is not the preflight on its own")
	}
	if p := final.GetPreflight(); len(p) != 1 || p[0].GetStep() != "NG Setup" || p[0].GetStatus() != "failed" {
		t.Errorf("final update's preflight = %v, want the NG Setup failed", p)
	}
	run := waitRunState(t, runs, id, orbitv1.RunState_RUN_STATE_COMPLETE)
	if run.GetKind() != orbitv1.RunKind_RUN_KIND_CONFORMANCE {
		t.Errorf("kind = %v", run.GetKind())
	}
	l, err := runs.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{Kind: orbitv1.RunKind_RUN_KIND_CONFORMANCE}))
	if err != nil || len(l.Msg.GetRuns()) != 1 || l.Msg.GetRuns()[0].GetRunId() != id {
		t.Errorf("ListRuns(CONFORMANCE) = %v, %v", l.Msg.GetRuns(), err)
	}
	g, err := runs.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
	if err != nil || g.Msg.GetConformanceProgress().GetRan() != uint32(results) || g.Msg.GetConformanceProgress().GetTotal() != uint32(results) ||
		len(g.Msg.GetConformanceProgress().GetPreflight()) != 1 {
		t.Errorf("GetRun progress = %v, %v; want %d of %d and the preflight", g.Msg.GetConformanceProgress(), err, results, results)
	}
	r, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Msg.GetConformance(); c == nil || len(c.GetResults()) != results || c.GetIncomplete() || len(r.Msg.GetWorkloads()) != 1 || len(c.GetPreflight()) != 1 {
		t.Errorf("report = %v", r.Msg)
	}
	for _, res := range r.Msg.GetConformance().GetResults() {
		if res.GetVerdict() != "ERROR" || !strings.HasPrefix(res.GetObserved(), conformance.ObservedPreflightFailed+" (NG Setup failed)") {
			t.Errorf("%s = %s %q, want the preflight's ERROR", res.GetId(), res.GetVerdict(), res.GetObserved())
		}
	}
	a := waitArchive(t, dir, id)
	if rs := a.GetReports(); len(rs) != 1 || len(rs[0].GetConformance().GetResults()) != results {
		t.Errorf("archived reports = %v", rs)
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+archiveExt))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{archKi, archOpc} {
		if bytes.Contains(raw, []byte(k)) {
			t.Errorf("the archive holds %q", k)
		}
	}
}

// A client that goes away stops its suite: the run ends CANCELLED, and its
// partial results are archived with the conformance gate failed as an
// incomplete suite's.
func TestRunConformanceClientGoneCancels(t *testing.T) {
	amf := silentAMF(t) // checks wait out their timeouts, so the suite is still running
	dir, runs, conf := suiteServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := conf.RunConformance(ctx, connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: amf, Mcc: "208", Mnc: "93", PerTestSeconds: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("no first update: %v", stream.Err())
	}
	id := stream.Msg().GetRunId()
	cancel()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		g, err := runs.GetRun(context.Background(), connect.NewRequest(&orbitv1.GetRunRequest{RunId: id}))
		if err == nil && g.Msg.GetRun().GetState() == orbitv1.RunState_RUN_STATE_CANCELLED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never ended CANCELLED: %v, %v", g, err)
		}
	}
	a := waitArchive(t, dir, id)
	c := a.GetReports()[0].GetConformance()
	if !c.GetIncomplete() || len(c.GetResults()) == 0 || len(c.GetResults()) >= int(c.GetTotal()) {
		t.Errorf("archived summary: %d of %d results, incomplete %t", len(c.GetResults()), c.GetTotal(), c.GetIncomplete())
	}
}

// A suite cut off at the plan's deadline completes incomplete, and its
// conformance gate fails as unmeasured; a plan's own expected list is the
// one recorded.
func TestSuiteDeadlineFailsTheGate(t *testing.T) {
	// An AMF that never answers, so each check waits out its timeout and
	// the deadline, not the suite's speed, decides where it stops: against
	// a dead address every check ERRORs at once and a fast host can finish
	// the suite inside any short deadline.
	amf := idleAMF(t)
	_, runs, _ := suiteServer(t)
	p := suitePlan(func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = amf
		cw.PerTestTimeoutMs = 1000
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 300}}
		p.Gates = []*orbitv1.Gate{{Name: "suite", Check: &orbitv1.Gate_Conformance{Conformance: &orbitv1.ConformanceGate{}}}}
		cw.Expected = &orbitv1.ExpectedFailures{Core: "lab A", Entries: []*orbitv1.ExpectedFailure{
			{Check: "NGAP-UNKNOWN-UE-SURVIVES", Observed: "silently dropped; association survived", Finding: "EX-1", Added: "2026-10-27", Reason: "example"}}}
	})
	resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	var r *orbitv1.GetRunReportResponse
	for deadline := time.Now().Add(30 * time.Second); r == nil; time.Sleep(20 * time.Millisecond) {
		got, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
		switch {
		case err == nil:
			r = got.Msg
		case connect.CodeOf(err) != connect.CodeFailedPrecondition || time.Now().After(deadline):
			t.Fatalf("GetRunReport: %v", err)
		}
	}
	c := r.GetConformance()
	if !c.GetIncomplete() || c.GetExpectedFailuresSource() != "plan" || c.GetExpectedFailuresCore() != "lab A" {
		t.Errorf("summary: incomplete %t, list %q %q", c.GetIncomplete(), c.GetExpectedFailuresSource(), c.GetExpectedFailuresCore())
	}
	if r.GetGatesPassed() || len(r.GetGates()) != 1 || !strings.HasPrefix(r.GetGates()[0].GetDetail(), "suite incomplete: ") {
		t.Errorf("gates = %v, passed %t", r.GetGates(), r.GetGatesPassed())
	}
}

// Two suites at once on the same pinned gNB IDs: the second is refused,
// naming the lease, where before both dialled the AMF with one identity.
func TestSuiteLeaseConflict(t *testing.T) {
	s := planService(t)
	s.archive = &archiveStore{}
	amf := idleAMF(t) // the first suite's checks wait out their timeouts
	plan := suitePlan(func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Core.Amf = amf })
	m := &orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: plan}}
	first, _, err := s.startPlan(m, engine.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.startPlan(m, engine.RunOptions{})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("second suite: %v, want FAILED_PRECONDITION naming %s", err, first.ID)
	}
	_, _ = s.reg.Stop(first.ID)
}

// A typo'd category through RunConformance is a clean INVALID_ARGUMENT
// naming it, not a dropped stream.
func TestRunConformanceUnknownCategory(t *testing.T) {
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", Mcc: "208", Mnc: "93", Categories: []string{"typo"},
	}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `unknown conformance category "typo"`) {
		t.Fatalf("err = %v, want INVALID_ARGUMENT naming the category", err)
	}
}

// A suite stopped by another client ends its stream with an error saying
// so, not a summary that reads finished.
func TestRunConformanceStoppedElsewhere(t *testing.T) {
	amf := silentAMF(t)
	_, runs, conf := suiteServer(t)
	stream, err := conf.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: amf, Mcc: "208", Mnc: "93", PerTestSeconds: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("no first update: %v", stream.Err())
	}
	id := stream.Msg().GetRunId()
	if _, err := runs.StopRun(context.Background(), connect.NewRequest(&orbitv1.StopRunRequest{RunId: id})); err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if stream.Msg().GetDone() {
			t.Fatal("a stopped suite sent a final summary")
		}
	}
	if err := stream.Err(); connect.CodeOf(err) != connect.CodeCanceled || !strings.Contains(err.Error(), "run "+id+" was stopped") {
		t.Fatalf("stream err = %v, want CANCELED naming the run", err)
	}
}

// The UE checks' SUPI lease is the pool's first SUPI, and only a pool that
// holds one can supply it.
func TestSuiteSUPIWithinPool(t *testing.T) {
	s := planService(t)
	pr, err := s.buildPlan(suitePlan(func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Subscribers[0].Count = 1 }), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	if b := pr.needs.SUPIs; len(b) != 1 || b[0].Last != b[0].First {
		t.Errorf("SUPI lease = %v, want one SUPI", b)
	}
}

// A target that bounds its gNB IDs (200 here) runs the whole suite in the
// packed layout: every ID it leases is inside the block, pinned or
// assigned, and the suite runs to the end.
func TestSuiteInABoundedBlock(t *testing.T) {
	reg := conformance.NewRegistry()
	block := func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
		p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 5000, Count: 200}
		p.Target.Core.Amf = "127.0.0.1:1"
	}
	for _, assign := range []bool{false, true} {
		s := planService(t)
		pr, err := s.buildPlan(suitePlan(block, func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			if assign {
				cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_Assign{Assign: true}}
			}
		}), engine.EventsNormal)
		if err != nil {
			t.Fatalf("assign %t: %v", assign, err)
		}
		env := pr.suiteLive.spec.env
		if !env.PackedGNBIDs {
			t.Errorf("assign %t: not packed in a bounded block", assign)
		}
		want := reg.Needs(env.GNB.ID, true).GNBIDs
		var got []uint32
		for _, g := range pr.needs.GNBs {
			got = append(got, g.ID)
			if g.ID < 5000 || g.ID > 5199 {
				t.Errorf("assign %t: gNB ID %d outside the block", assign, g.ID)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("assign %t: leased %v, the suite presents %v", assign, got, want)
		}
	}
	// And it runs, every check to a verdict.
	_, runs, _ := suiteServer(t)
	resp, err := runs.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{Spec: &orbitv1.StartRunRequest_Plan{Plan: suitePlan(block)}}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetRun().GetRunId()
	waitRunState(t, runs, id, orbitv1.RunState_RUN_STATE_COMPLETE)
	r, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Msg.GetConformance(); c.GetIncomplete() || len(c.GetResults()) != len(reg.Tests()) {
		t.Errorf("summary: %d of %d results, incomplete %t", len(c.GetResults()), c.GetTotal(), c.GetIncomplete())
	}
}

// A conformance workload's NAS algorithms reach the suite's UE as the UE
// security capability's bitmaps (bit 8 is algorithm 0), as --local's
// --nas-integrity and --nas-ciphering give them; unset is the default.
func TestSuiteNASAlgorithms(t *testing.T) {
	tests := []struct {
		name   string
		nas    *orbitv1.NASAlgorithms
		ia, ea uint8
	}{
		{name: "the default", nas: nil},
		{name: "NIA1,NIA2 and NEA0,NEA2", nas: &orbitv1.NASAlgorithms{Integrity: []string{"NIA1", "NIA2"}, Ciphering: []string{"NEA0", "NEA2"}},
			ia: 0x40 | 0x20, ea: 0x80 | 0x20},
		{name: "integrity only, any case", nas: &orbitv1.NASAlgorithms{Integrity: []string{"nia3"}}, ia: 0x10},
	}
	s := planService(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := s.buildPlan(suitePlan(func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.Nas = tc.nas }), engine.EventsNormal)
			if err != nil {
				t.Fatal(err)
			}
			u := pr.suiteLive.spec.env.UE
			if u.NASIntegrity != tc.ia || u.NASCiphering != tc.ea {
				t.Errorf("UE NAS = %#02x/%#02x, want %#02x/%#02x", u.NASIntegrity, u.NASCiphering, tc.ia, tc.ea)
			}
		})
	}
}

// A conformance workload's responder becomes the suite's downlink trigger,
// with the target responder's control address and token; without one the
// suite has no trigger and the checks that need one SKIP.
func TestSuiteResponderIsTheDownlinkTrigger(t *testing.T) {
	s := planService(t)
	tests := []struct {
		name      string
		responder string
		want      engine.DownlinkTrigger
	}{
		{"named", "n6", engine.ResponderDownlink{Peer: "127.0.0.1:9", Token: archToken}},
		{"none", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := s.buildPlan(suitePlan(func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.Responder = tc.responder }), engine.EventsNormal)
			if err != nil {
				t.Fatal(err)
			}
			if got := pr.suiteLive.spec.env.Downlink; got != tc.want {
				t.Errorf("Downlink = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The suite is spread over as many subscribers as its checks take, within
// the pool; a pool with no count gives only what one check holds at once.
func TestSuiteSubscribers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     uint32
		span, ues int
		want      int
	}{
		{name: "pool larger than the span", count: 50, span: 24, ues: 1, want: 24},
		{name: "pool smaller than the span", count: 10, span: 24, ues: 1, want: 10},
		{name: "pool of one", count: 1, span: 24, ues: 1, want: 1},
		{name: "no count", count: 0, span: 24, ues: 1, want: 1},
		{name: "never fewer than one check holds", count: 50, span: 1, ues: 2, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := suitePlan(func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Subscribers[0].Count = tc.count })
			if got := suiteSubscribers(p.GetTarget(), p.GetWorkloads()[0].GetConformance(), tc.span, tc.ues); got != tc.want {
				t.Errorf("suiteSubscribers = %d, want %d", got, tc.want)
			}
		})
	}
}

// A workload's checks narrow the suite to the named checks, on top of its
// categories and in any case: the suite counts, runs and leases gNB IDs
// for those alone, each with the IDs it has in the full suite, and a
// selection with a UE check has the full suite's subscriber pool.
func TestSuiteChecksSelection(t *testing.T) {
	reg := conformance.NewRegistry()
	full, err := planService(t).buildPlan(suitePlan(), engine.EventsNormal)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		cats   []string
		checks []string
		want   []string // the checks the suite runs, in run order
	}{
		{name: "one check", checks: []string{"NGAP-NG-RESET-ACK"}, want: []string{"NGAP-NG-RESET-ACK"}},
		{name: "two checks, any case, in run order", checks: []string{"ngap-path-switch-seccap-mismatch", "NGAP-NG-RESET-ACK"},
			want: []string{"NGAP-NG-RESET-ACK", "NGAP-PATH-SWITCH-SECCAP-MISMATCH"}},
		{name: "on top of a category", cats: []string{"nas"}, checks: []string{"NAS-T3560-AUTH-RETRANSMIT", "NGAP-NG-RESET-ACK"},
			want: []string{"NAS-T3560-AUTH-RETRANSMIT"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := planService(t).buildPlan(suitePlan(func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
				cw.Categories, cw.Checks = tc.cats, tc.checks
			}), engine.EventsNormal)
			if err != nil {
				t.Fatal(err)
			}
			spec := pr.suiteLive.spec
			if spec.total != len(tc.want) || !slices.Equal(spec.checks, tc.checks) {
				t.Errorf("total %d, checks %v; want %d and %v", spec.total, spec.checks, len(tc.want), tc.checks)
			}
			only := conformance.NewRegistry()
			if err := only.Only(tc.want...); err != nil {
				t.Fatal(err)
			}
			base := spec.env.GNB.ID
			var leased []uint32
			for _, g := range pr.needs.GNBs {
				leased = append(leased, g.ID)
			}
			if want := only.Needs(base, false).GNBIDs; !slices.Equal(leased, want) {
				t.Errorf("leased gNB IDs %v, want the selected checks' %v", leased, want)
			}
			if base != full.suiteLive.spec.env.GNB.ID {
				t.Errorf("base %d, the full suite's %d", base, full.suiteLive.spec.env.GNB.ID)
			}
			// A selection with a UE check spreads the checks over the same
			// pool as the full suite; one without leases no subscriber.
			want := 0
			if only.Needs(base, false).UEs > 0 {
				want = full.suiteLive.spec.env.UE.Subscribers
			}
			if got := spec.env.UE.Subscribers; got != want || got > reg.SubscriberSpan() {
				t.Errorf("subscriber pool of %d, want %d", got, want)
			}
		})
	}
}
