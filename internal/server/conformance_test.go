package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/conformance"
)

func newConformanceClient(t *testing.T) orbitv1connect.ConformanceServiceClient {
	t.Helper()
	return conformanceClientWith(t, nil)
}

// conformanceClientWith serves the conformance service over a fresh run
// service, as New wires it, with x as the server's expected failures.
func conformanceClientWith(t *testing.T, x *conformance.ExpectedFailures) orbitv1connect.ConformanceServiceClient {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runs := planService(t)
	runs.expected = x
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewConformanceServiceHandler(&conformanceService{log: log, expected: x, runs: runs}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orbitv1connect.NewConformanceServiceClient(srv.Client(), srv.URL)
}

func TestListConformanceTests(t *testing.T) {
	c := newConformanceClient(t)
	res, err := c.ListConformanceTests(context.Background(),
		connect.NewRequest(&orbitv1.ListConformanceTestsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.GetTests()) == 0 {
		t.Fatal("no tests registered; the dashboard would render an empty table")
	}
	for _, tst := range res.Msg.GetTests() {
		if tst.GetId() == "" || tst.GetCategory() == "" {
			t.Errorf("test missing id/category: %+v", tst)
		}
	}
}

func TestRunConformanceRequiresAMF(t *testing.T) {
	// A run with no core to point at must be refused, not silently do nothing.
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(),
		connect.NewRequest(&orbitv1.RunConformanceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	stream.Receive()
	err = stream.Err()
	var ce *connect.Error
	if err == nil {
		t.Fatal("expected an error for a missing AMF address")
	}
	if !connectErrorAs(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestRunConformanceStreamsAndSummarises(t *testing.T) {
	// The AMF is unroutable, so every test ERRORs fast — but the stream must
	// still deliver one update per test and a final done=true summary whose
	// tally accounts for all of them. That is the contract the live view relies
	// on, independent of any verdict.
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(),
		connect.NewRequest(&orbitv1.RunConformanceRequest{
			AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01",
			PerTestSeconds: 2,
			Categories:     []string{"procedural"},
		}))
	if err != nil {
		t.Fatal(err)
	}
	var results, summaries int
	var last *orbitv1.ConformanceUpdate
	for stream.Receive() {
		u := stream.Msg()
		if u.GetResult() != nil {
			results++
			if u.GetTotal() == 0 {
				t.Error("a result update must carry a non-zero total for the progress bar")
			}
		}
		if u.GetDone() {
			summaries++
		}
		last = u
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if results == 0 {
		t.Fatal("no per-test updates streamed")
	}
	if summaries != 1 {
		t.Errorf("want exactly one done=true summary, got %d", summaries)
	}
	if last == nil || !last.GetDone() {
		t.Fatal("stream did not end with the summary")
	}
	tally := last.GetTally()
	total := tally.GetPassed() + tally.GetFailed() + tally.GetErrored() + tally.GetSkipped() + tally.GetDeviations()
	if int(total) != results {
		t.Errorf("tally totals %d but %d tests ran — a result was not counted", total, results)
	}
}

func TestRunConformanceBroadcastsASlice(t *testing.T) {
	// The regression: the handler built a gNB with no S-NSSAI, so NG Setup
	// failed and every test ERRORed with "at least one supported S-NSSAI is
	// required" — the whole suite red, with nothing to do with the core.
	// The AMF here is unroutable, so tests still ERROR, but the DETAIL must no
	// longer mention the missing slice.
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(),
		connect.NewRequest(&orbitv1.RunConformanceRequest{
			AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", PerTestSeconds: 2,
		}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			// The gNB config error itself, not any mention of S-NSSAI: a
			// check's spec references may name S-NSSAI.
			if strings.Contains(r.GetDetail()+r.GetObserved(), "supported S-NSSAI is required") {
				t.Fatalf("test %s still fails on the missing slice: %s", r.GetId(), r.GetObserved()+r.GetDetail())
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
}

func connectErrorAs(err error, target **connect.Error) bool {
	ce := new(connect.Error)
	if connect.CodeOf(err) != connect.CodeUnknown {
		*ce = *connect.NewError(connect.CodeOf(err), err)
		*target = ce
		return true
	}
	return false
}

// ListConformanceTests carries each check's structured references and the
// release it requires, with spec_ref the same rendering as each ref.
func TestListConformanceTestsCarriesProvenance(t *testing.T) {
	c := newConformanceClient(t)
	res, err := c.ListConformanceTests(context.Background(), connect.NewRequest(&orbitv1.ListConformanceTestsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tst := range res.Msg.GetTests() {
		if tst.GetMinRelease() < 15 || len(tst.GetSpecRefs()) == 0 {
			t.Errorf("%s: min_release %d, %d spec_refs", tst.GetId(), tst.GetMinRelease(), len(tst.GetSpecRefs()))
			continue
		}
		var rendered []string
		for _, r := range tst.GetSpecRefs() {
			if r.GetSpec() == "" || r.GetClause() == "" || r.GetStrength() == "" || len(r.GetVerified()) == 0 {
				t.Errorf("%s: incomplete ref %+v", tst.GetId(), r)
			}
			rendered = append(rendered, r.GetRendered())
		}
		if got := strings.Join(rendered, "; "); got != tst.GetSpecRef() {
			t.Errorf("%s: spec_ref %q differs from its refs' rendering %q", tst.GetId(), tst.GetSpecRef(), got)
		}
	}
}

// A target release older than every check's requirement SKIPs the whole
// suite without dialling (the AMF here is unroutable), each result carrying
// its provenance and the reason, and every update reports the target.
func TestRunConformanceTargetReleaseSkips(t *testing.T) {
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", PerTestSeconds: 2, TargetRelease: 14,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for stream.Receive() {
		u := stream.Msg()
		if u.GetTargetRelease() != 14 {
			t.Errorf("update target_release = %d, want 14", u.GetTargetRelease())
		}
		r := u.GetResult()
		if r == nil {
			continue
		}
		results++
		if r.GetVerdict() != "SKIP" || !strings.Contains(r.GetDetail(), "target release is Rel-14") {
			t.Errorf("%s: verdict %s detail %q, want SKIP for the target", r.GetId(), r.GetVerdict(), r.GetDetail())
		}
		if len(r.GetSpecRefs()) == 0 || r.GetMinRelease() < 15 || !strings.HasSuffix(r.GetDetail(), r.GetSpecRef()) {
			t.Errorf("%s: result lacks provenance: %+v", r.GetId(), r)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if results == 0 {
		t.Fatal("no results")
	}
}

// target_release 0 is the server default, Rel-18 with TS 29.244 pinned to
// Rel-16, the same run the CLI makes with no flag; an explicit release is
// honoured with the pin when it still narrows. Every update states both.
func TestRunConformanceDefaultTargets(t *testing.T) {
	tests := []struct {
		name       string
		target     uint32
		wantTarget uint32
		wantPins   map[string]uint32
	}{
		{name: "default", target: 0, wantTarget: 18, wantPins: map[string]uint32{"TS 29.244": 16}},
		{name: "explicit", target: 17, wantTarget: 17, wantPins: map[string]uint32{"TS 29.244": 16}},
		{name: "explicit below the pin", target: 15, wantTarget: 15, wantPins: map[string]uint32{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newConformanceClient(t)
			stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
				AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", PerTestSeconds: 2,
				Categories: []string{"procedural"}, TargetRelease: tc.target,
			}))
			if err != nil {
				t.Fatal(err)
			}
			var updates int
			for stream.Receive() {
				u := stream.Msg()
				updates++
				if u.GetTargetRelease() != tc.wantTarget {
					t.Errorf("update target_release = %d, want %d", u.GetTargetRelease(), tc.wantTarget)
				}
				if got := u.GetSpecTargetReleases(); len(got) != len(tc.wantPins) || (len(got) > 0 && !reflect.DeepEqual(got, tc.wantPins)) {
					t.Errorf("update spec_target_releases = %v, want %v", got, tc.wantPins)
				}
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
			if updates == 0 {
				t.Fatal("no updates")
			}
		})
	}
}

// A FAIL the server's list tolerates carries its entry on the wire and is
// counted in the tally's expected_failures as well as failed; other
// verdicts are never counted there. Stale entries carry what the check gave.
func TestConformanceExpectedFailureProto(t *testing.T) {
	var tally orbitv1.ConformanceTally
	tolerated := conformance.Result{ID: "A", Verdict: conformance.Fail, Observed: "o",
		ExpectedFailure: &conformance.ExpectedFailure{Finding: "EX-1", Added: "2026-10-12", Reason: "example"}}
	for _, r := range []conformance.Result{tolerated, {ID: "B", Verdict: conformance.Fail}, {ID: "C", Verdict: conformance.Error}} {
		addToTally(&tally, r)
	}
	if tally.GetFailed() != 2 || tally.GetExpectedFailures() != 1 || tally.GetErrored() != 1 {
		t.Errorf("tally = %+v", &tally)
	}
	if c := cloneTally(&tally); c.GetExpectedFailures() != 1 {
		t.Errorf("cloneTally dropped expected_failures: %+v", c)
	}
	p := resultProto(tolerated)
	if e := p.GetExpectedFailure(); e.GetFinding() != "EX-1" || e.GetAdded() != "2026-10-12" || e.GetReason() != "example" || p.GetVerdict() != "FAIL" {
		t.Errorf("result proto = %+v", p)
	}
	if resultProto(conformance.Result{ID: "B", Verdict: conformance.Fail}).GetExpectedFailure() != nil {
		t.Error("an untolerated FAIL carries an expected_failure")
	}
	st := staleProto([]conformance.StaleExpectation{{Check: "A", Finding: "EX-1", ExpectedObserved: "o", Verdict: conformance.Pass, Observed: "ok"}})
	if len(st) != 1 || st[0].GetVerdict() != "PASS" || st[0].GetExpectedObserved() != "o" || st[0].GetObserved() != "ok" {
		t.Errorf("stale proto = %+v", st)
	}
}

// Every update names the server's list; checks that ERROR are neither
// tolerated nor stale.
func TestRunConformanceCarriesExpectedFailuresCore(t *testing.T) {
	x, err := conformance.LoadExpectedFailures("../conformance/testdata/expected-failures.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := conformanceClientWith(t, x)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", PerTestSeconds: 2, Categories: []string{"negative-ie"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var last *orbitv1.ConformanceUpdate
	for stream.Receive() {
		u := stream.Msg()
		if u.GetExpectedFailuresCore() != "Example core (lab A)" {
			t.Errorf("update expected_failures_core = %q", u.GetExpectedFailuresCore())
		}
		if r := u.GetResult(); r != nil && r.GetExpectedFailure() != nil {
			t.Errorf("%s (%s) tolerated", r.GetId(), r.GetVerdict())
		}
		last = u
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if !last.GetDone() || len(last.GetStale()) != 0 || last.GetTally().GetExpectedFailures() != 0 {
		t.Errorf("final update = %+v", last)
	}
}
