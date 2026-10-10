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
	"github.com/bgrewell/orbit/internal/engine"
)

// RunConformance has no PLMN default any more: a request without one is the
// client's mistake, refused before any check runs.
func TestRunConformanceRequiresPLMN(t *testing.T) {
	tests := []struct {
		name     string
		mcc, mnc string
		sd       string
		want     string
	}{
		{name: "neither", want: "mcc and mnc are required"},
		{name: "mcc only", mcc: "001", want: "mcc and mnc are required"},
		{name: "bad mnc", mcc: "001", mnc: "1", want: "mnc"},
		{name: "bad sd", mcc: "001", mnc: "01", sd: "xyz", want: "sd"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newConformanceClient(t)
			stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
				AmfAddress: "127.0.0.1:1", Mcc: tc.mcc, Mnc: tc.mnc, Sd: tc.sd, PerTestSeconds: 1,
			}))
			if err != nil {
				t.Fatal(err)
			}
			if stream.Receive() {
				t.Fatal("a check ran without a valid PLMN")
			}
			err = stream.Err()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
			}
		})
	}
}

// With a PLMN and no SD the suite runs with an SD-less slice (the new
// default), not the old hardcoded 010203: no check fails on the slice.
func TestRunConformanceSDDefaultsToNone(t *testing.T) {
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", PerTestSeconds: 1, Categories: []string{"procedural"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			n++
			if strings.Contains(r.GetDetail()+r.GetObserved(), "S-NSSAI") || strings.Contains(r.GetDetail(), "sd ") {
				t.Errorf("%s failed on the slice: %s", r.GetId(), r.GetDetail())
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no checks ran")
	}
}

// Every RPC that takes a gNB config validates its network identity with the
// shared rule, so the API rejects what the CLI rejects: a one-digit MNC, a
// zero TAC, a malformed SD.
func TestGnbConfigIdentityValidatedAtTheAPI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewCellServiceHandler(&cellService{log: log}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewCellServiceClient(srv.Client(), srv.URL)

	tests := []struct {
		name string
		mut  func(*orbitv1.GnbConfig)
		want string
	}{
		{"one-digit mnc", func(g *orbitv1.GnbConfig) { g.Mnc = "1" }, "mnc"},
		{"missing mcc", func(g *orbitv1.GnbConfig) { g.Mcc = "" }, "mcc"},
		{"zero tac", func(g *orbitv1.GnbConfig) { g.Tac = 0 }, "tac"},
		{"bad sd", func(g *orbitv1.GnbConfig) { g.Slices[0].Sd = "0102" }, "sd"},
		{"sst too large", func(g *orbitv1.GnbConfig) { g.Slices[0].Sst = 300 }, "sst"},
		{"no slices", func(g *orbitv1.GnbConfig) { g.Slices = nil }, "S-NSSAI"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := testGNB()
			tc.mut(g)
			_, err := c.RunNGSetup(context.Background(), connect.NewRequest(&orbitv1.RunNGSetupRequest{
				AmfAddress: "127.0.0.1:1", Gnb: g,
			}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
			}
		})
	}
}

// A handover target may omit its identity (inherited from the serving gNB in
// the engine), so the handler must accept an empty PLMN at the boundary; the
// UE here is not registered, so the engine refuses it for that reason, not
// for the missing PLMN.
func TestHandoverAcceptsEmptyTargetIdentity(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := engine.NewManager(log)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: mgr}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewUEServiceClient(srv.Client(), srv.URL)

	for _, call := range []struct {
		name string
		fn   func(context.Context, *connect.Request[orbitv1.HandoverRequest]) (*connect.Response[orbitv1.HandoverResponse], error)
	}{{"n2", c.Handover}, {"xn", c.XnHandover}} {
		t.Run(call.name, func(t *testing.T) {
			_, err := call.fn(context.Background(), connect.NewRequest(&orbitv1.HandoverRequest{
				Supi: "001010000000001", AmfAddress: "127.0.0.1:1",
				TargetGnb: &orbitv1.GnbConfig{Id: 2, Name: "tgt"}, // no PLMN, TAC or slices
			}))
			if err == nil {
				t.Fatal("handover of an unregistered UE succeeded")
			}
			if strings.Contains(err.Error(), "mcc") || connect.CodeOf(err) == connect.CodeInvalidArgument {
				t.Errorf("empty target identity rejected at the boundary: %v", err)
			}
			if !strings.Contains(err.Error(), "not registered") {
				t.Errorf("err = %v, want the not-registered failure", err)
			}
		})
	}
}
