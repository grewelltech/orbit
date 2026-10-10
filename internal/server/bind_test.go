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
	"github.com/bgrewell/orbit/internal/load"
)

// badBind is a local address sctp.NormalizeLocal rejects, so a handler that
// validates bind addresses up front refuses it as InvalidArgument without
// kernel SCTP or a core.
const badBind = "10.0.0.1:0:0"

const wantBindErr = `invalid local address "` + badBind + `"`

func testGNB() *orbitv1.GnbConfig {
	return &orbitv1.GnbConfig{
		Id: 1, IdBits: 24, Name: "orbit-gnb", Mcc: "208", Mnc: "93", Tac: 1,
		Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}},
	}
}

// RunNGSetup validates local_address up front: a malformed one is the
// client's mistake, so InvalidArgument rather than a retryable Unavailable.
func TestRunNGSetupRejectsBadLocalAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewCellServiceHandler(&cellService{log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewCellServiceClient(srv.Client(), srv.URL)

	_, err := c.RunNGSetup(context.Background(), connect.NewRequest(&orbitv1.RunNGSetupRequest{
		AmfAddress: "127.0.0.1:1", LocalAddress: badBind, Gnb: testGNB(),
	}))
	if err == nil || !strings.Contains(err.Error(), wantBindErr) {
		t.Fatalf("err = %v, want containing %q", err, wantBindErr)
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// RegisterUE validates bind_address up front.
func TestRegisterUERejectsBadBindAddress(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: engine.NewManager(log)}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewUEServiceClient(srv.Client(), srv.URL)

	_, err := c.Register(context.Background(), connect.NewRequest(&orbitv1.RegisterRequest{
		AmfAddress:  "127.0.0.1:1",
		BindAddress: badBind,
		Supi:        "208930100000001",
		Gnb:         testGNB(),
		Credentials: &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff", Opc: "000102030405060708090a0b0c0d0e0f"},
	}))
	if err == nil || !strings.Contains(err.Error(), wantBindErr) {
		t.Fatalf("err = %v, want containing %q", err, wantBindErr)
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// RunConformance validates bind_address up front: malformed, or a fixed
// port (the suite opens several associations per check), is InvalidArgument
// before any check runs.
func TestRunConformanceRejectsBadBindAddress(t *testing.T) {
	tests := []struct{ name, bind, wantErr string }{
		{name: "malformed", bind: badBind, wantErr: wantBindErr},
		{name: "fixed port", bind: "10.0.0.1:38412", wantErr: "fixes a port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newConformanceClient(t)
			stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
				AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", BindAddress: tc.bind, PerTestSeconds: 2, Categories: []string{"procedural"},
			}))
			if err != nil {
				t.Fatal(err)
			}
			if stream.Receive() {
				t.Fatal("a check ran with an invalid bind address")
			}
			err = stream.Err()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
			}
		})
	}
}

// A valid port-0 bind reaches every check's dial. The AMF is unroutable, so
// every check ERRORs — on the dial from that bind, not on validation.
func TestRunConformanceAcceptsEphemeralBind(t *testing.T) {
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", Mcc: "001", Mnc: "01", BindAddress: "127.0.0.1", PerTestSeconds: 2, Categories: []string{"procedural"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			results++
			if r.GetVerdict() != "ERROR" {
				t.Errorf("%s: verdict %s against an unroutable AMF", r.GetId(), r.GetVerdict())
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if results == 0 {
		t.Fatal("no results streamed")
	}
}

// StartRun with a load spec applies the none / one / one-per-gNB rule and
// the port rule to bind_addresses and refuses anything else as
// InvalidArgument, before a run is registered.
func TestStartLoadRejectsBadBinds(t *testing.T) {
	tests := []struct {
		name  string
		gnbs  int
		binds []string
		ok    bool
	}{
		{name: "none", gnbs: 2, binds: nil, ok: true},
		{name: "one shared", gnbs: 2, binds: []string{"10.0.0.1"}, ok: true},
		{name: "one each", gnbs: 2, binds: []string{"10.0.0.1", "10.0.0.2"}, ok: true},
		{name: "one each with distinct fixed ports", gnbs: 2, binds: []string{"10.0.0.1:38412", "10.0.0.1:38413"}, ok: true},
		{name: "one fewer", gnbs: 3, binds: []string{"10.0.0.1", "10.0.0.2"}},
		{name: "one more", gnbs: 2, binds: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}},
		{name: "shared fixed port", gnbs: 2, binds: []string{"10.0.0.1:38412"}},
		{name: "duplicate fixed port", gnbs: 2, binds: []string{"10.0.0.1:38412", "10.0.0.1:38412"}},
		{name: "malformed", gnbs: 1, binds: []string{badBind}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := failFastLoadSpec()
			spec.Gnbs = nil
			for i := 0; i < tc.gnbs; i++ {
				g := testGNB()
				g.Id = uint32(i + 1)
				spec.Gnbs = append(spec.Gnbs, g)
			}
			spec.BindAddresses = tc.binds
			_, err := loadRunFunc(log, spec)
			if tc.ok {
				if err != nil {
					t.Fatalf("loadRunFunc: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("bad bind list accepted")
			}
			if !strings.Contains(err.Error(), "bind address") && !strings.Contains(err.Error(), "invalid local address") {
				t.Errorf("error %q does not explain the bind rule", err)
			}
			// Over the wire it is InvalidArgument and registers nothing.
			c, _ := newRunRPCClient(t)
			_, err = c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
				Spec: &orbitv1.StartRunRequest_Load{Load: spec},
			}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("StartRun code = %v, want InvalidArgument", connect.CodeOf(err))
			}
			list, lerr := c.ListRuns(context.Background(), connect.NewRequest(&orbitv1.ListRunsRequest{}))
			if lerr != nil {
				t.Fatal(lerr)
			}
			if n := len(list.Msg.GetRuns()); n != 0 {
				t.Errorf("a rejected StartRun registered %d runs", n)
			}
		})
	}
}

// A load run's gNBs dial from their bind_addresses: the launcher built by
// loadRunFunc carries the normalised list through to the fleet, whose dial
// names the source in its error.
func TestLoadRunFuncPassesBindAddresses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := failFastLoadSpec()
	spec.BindAddresses = []string{"127.0.0.1"}
	fn, err := loadRunFunc(log, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fn(context.Background(), load.NewLiveStats(), func(string, string, string, string) {})
	if err == nil {
		t.Fatal("a load run against an unroutable AMF succeeded")
	}
	if !strings.Contains(err.Error(), `from "127.0.0.1:0"`) {
		t.Errorf("run err = %v, want the dial to name the bind address", err)
	}
}
