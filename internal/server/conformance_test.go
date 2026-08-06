package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

func newConformanceClient(t *testing.T) orbitv1connect.ConformanceServiceClient {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewConformanceServiceHandler(&conformanceService{log: log}))
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
			AmfAddress:     "127.0.0.1:1",
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
			AmfAddress: "127.0.0.1:1", PerTestSeconds: 2,
		}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			if strings.Contains(r.GetDetail()+r.GetObserved(), "S-NSSAI") {
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
