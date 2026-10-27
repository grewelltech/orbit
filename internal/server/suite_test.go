package server

import (
	"bytes"
	"context"
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
		{name: "a responder", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.Responder = "n6" }, wantErr: "responder is not supported yet"},
		{name: "nas algorithms", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			cw.Nas = &orbitv1.NASAlgorithms{Integrity: []string{"NIA2"}}
		}, wantErr: "nas algorithms are not supported"},
		{name: "a bad release", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.TargetRelease = "Rel-x" }, wantErr: "target_release"},
		{name: "IDs past the ID length", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{Bits: 22}
			cw.GnbIds = &orbitv1.GNBIDSelection{Select: &orbitv1.GNBIDSelection_First{First: 1<<22 - 100}}
		}, wantErr: "do not fit 22 bits"},
		{name: "IDs outside the target's block", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.GnbIds = &orbitv1.GNBIDRange{First: 700, Count: 100}
		}, wantErr: "fall outside the target's ran.gnb_ids 700..799"},
		{name: "a fixed N2 port", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) {
			p.Target.Ran.N2SourceIps = []string{"127.0.0.1:5000"}
		}, wantErr: "ran.n2_source_ips"},
		{name: "a pool without the gNB N3", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Ran.N3Ips = nil }, wantErr: "ran.n3_ips"},
		{name: "traffic without a pool", mut: func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
			p.Target.Subscribers = nil
			cw.Traffic = "127.0.0.1:9"
		}, wantErr: "needs a subscriber pool"},
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
// base, its one N2 source, and the UE checks' subscriber only when one is
// selected and the target has a pool.
func TestSuiteLeases(t *testing.T) {
	reg := conformance.NewRegistry()
	tests := []struct {
		name     string
		mut      func(*orbitv1.RunPlan, *orbitv1.ConformanceWorkload)
		cats     []conformance.Category
		wantSUPI bool
	}{
		{name: "every check", wantSUPI: true},
		{name: "procedural only: no UE", mut: func(_ *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) { cw.Categories = []string{"procedural"} },
			cats: []conformance.Category{conformance.Procedural}},
		{name: "no pool: no UE", mut: func(p *orbitv1.RunPlan, _ *orbitv1.ConformanceWorkload) { p.Target.Subscribers = nil }},
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
			if want := reg.Needs(defaultSuiteGNBBase, tc.cats...).GNBIDs; !slices.Equal(got, want) {
				t.Errorf("gNB IDs = %v, want %v", got, want)
			}
			if !slices.Equal(pr.needs.N2IPs, []string{"127.0.0.1"}) {
				t.Errorf("N2 = %v", pr.needs.N2IPs)
			}
			if tc.wantSUPI != (len(pr.needs.SUPIs) == 1) {
				t.Fatalf("SUPIs = %v, want one: %t", pr.needs.SUPIs, tc.wantSUPI)
			}
			if tc.wantSUPI {
				if b := pr.needs.SUPIs[0]; b.First != 208930100007500 || b.Last != b.First || b.PLMN != "208/93" {
					t.Errorf("SUPI block = %+v, want the pool's first SUPI alone", b)
				}
			}
			if pr.suiteLive.spec.env.UE.Configured() == (tc.name == "no pool: no UE") {
				t.Errorf("UE configured = %t", pr.suiteLive.spec.env.UE.Configured())
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
			res("A", "PASS", false), res("B", "SKIP", false), res("C", "DEVIATION", false)}}, wantPass: true, wantDetail: "3 checks, no unexpected FAIL or ERROR"},
		{name: "an expected FAIL", summary: &orbitv1.ConformanceSummary{Total: 1, Results: []*orbitv1.ConformanceResult{
			res("A", "FAIL", true)}}, wantPass: true},
		{name: "an unexpected FAIL and an ERROR", summary: &orbitv1.ConformanceSummary{Total: 3, Results: []*orbitv1.ConformanceResult{
			res("A", "FAIL", false), res("B", "ERROR", false), res("C", "PASS", false)}}, wantDetail: "2 unexpected: A FAIL, B ERROR"},
		{name: "incomplete", summary: &orbitv1.ConformanceSummary{Total: 9, Incomplete: true, Results: []*orbitv1.ConformanceResult{
			res("A", "PASS", false), res("B", "PASS", false)}}, wantDetail: "suite incomplete: 2 of 9 checks ran"},
		{name: "no report", wantDetail: "no report to judge"},
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
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", prometheus.NewRegistry(), Options{StateDir: dir})
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
	n := 0
	for stream.Receive() {
		u := stream.Msg()
		if u.GetRunId() == "" || id != "" && u.GetRunId() != id {
			t.Fatalf("update %d names run %q (first %q)", n, u.GetRunId(), id)
		}
		id = u.GetRunId()
		if u.GetDone() {
			final = u
		}
		n++
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if final == nil || n < 2 {
		t.Fatalf("%d updates, final %v", n, final)
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
	if err != nil || g.Msg.GetConformanceProgress().GetRan() != uint32(n-1) || g.Msg.GetConformanceProgress().GetTotal() != uint32(n-1) {
		t.Errorf("GetRun progress = %v, %v; want %d of %d", g.Msg.GetConformanceProgress(), err, n-1, n-1)
	}
	r, err := runs.GetRunReport(context.Background(), connect.NewRequest(&orbitv1.GetRunReportRequest{RunId: id}))
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Msg.GetConformance(); c == nil || len(c.GetResults()) != n-1 || c.GetIncomplete() || len(r.Msg.GetWorkloads()) != 1 {
		t.Errorf("report = %v", r.Msg)
	}
	a := waitArchive(t, dir, id)
	if rs := a.GetReports(); len(rs) != 1 || len(rs[0].GetConformance().GetResults()) != n-1 {
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
	amf := idleAMF(t) // checks wait out their timeouts, so the suite is still running
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
	_, runs, _ := suiteServer(t)
	p := suitePlan(func(p *orbitv1.RunPlan, cw *orbitv1.ConformanceWorkload) {
		p.Target.Core.Amf = "127.0.0.1:1"
		p.Until = &orbitv1.Until{Stop: &orbitv1.Until_DurationMs{DurationMs: 1}}
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
