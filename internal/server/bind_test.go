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

// badBind is a local address sctp.Dial rejects during normalisation, before
// any socket is opened. An error carrying it proves a request's bind address
// reached the dialer, without kernel SCTP or a core.
const badBind = "not-an-ip"

const wantBindErr = `invalid local address "` + badBind + `"`

func testGNB() *orbitv1.GnbConfig {
	return &orbitv1.GnbConfig{
		Id: 1, IdBits: 24, Name: "orbit-gnb", Mcc: "208", Mnc: "93", Tac: 1,
		Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}},
	}
}

// RunNGSetup dials from local_address.
func TestRunNGSetupPassesLocalAddress(t *testing.T) {
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
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("code = %v, want Unavailable (a dial failure)", connect.CodeOf(err))
	}
}

// RegisterUE dials from bind_address.
func TestRegisterUEPassesBindAddress(t *testing.T) {
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
}

// RunConformance dials every check from bind_address.
func TestRunConformancePassesBindAddress(t *testing.T) {
	c := newConformanceClient(t)
	stream, err := c.RunConformance(context.Background(), connect.NewRequest(&orbitv1.RunConformanceRequest{
		AmfAddress: "127.0.0.1:1", BindAddress: badBind, PerTestSeconds: 2, Categories: []string{"procedural"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for stream.Receive() {
		upd := stream.Msg()
		if upd.GetDone() {
			continue
		}
		results++
		r := upd.GetResult()
		if r.GetVerdict() != "ERROR" || !strings.Contains(r.GetObserved()+" "+r.GetDetail(), wantBindErr) {
			t.Errorf("%s: verdict %s detail %q; want ERROR naming the bad bind", r.GetId(), r.GetVerdict(), r.GetDetail())
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if results == 0 {
		t.Fatal("no results streamed")
	}
}

// StartRun with a load spec applies the none / one / one-per-gNB rule to
// bind_addresses and refuses anything else as InvalidArgument, before a run
// is registered.
func TestStartLoadRejectsBadBindCount(t *testing.T) {
	tests := []struct {
		name  string
		gnbs  int
		binds []string
		ok    bool
	}{
		{name: "none", gnbs: 2, binds: nil, ok: true},
		{name: "one shared", gnbs: 2, binds: []string{"10.0.0.1"}, ok: true},
		{name: "one each", gnbs: 2, binds: []string{"10.0.0.1", "10.0.0.2"}, ok: true},
		{name: "one fewer", gnbs: 3, binds: []string{"10.0.0.1", "10.0.0.2"}},
		{name: "one more", gnbs: 2, binds: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}},
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
			if tc.ok && err != nil {
				t.Fatalf("loadRunFunc: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("bad bind count accepted")
				}
				if !strings.Contains(err.Error(), "bind addresses") {
					t.Errorf("error %q does not explain the bind rule", err)
				}
				// Over the wire it is InvalidArgument and registers nothing.
				c, _ := newRunRPCClient(t)
				_, err := c.StartRun(context.Background(), connect.NewRequest(&orbitv1.StartRunRequest{
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
			}
		})
	}
}

// A load run's gNBs dial from their bind_addresses: the launcher built by
// loadRunFunc carries them through to the fleet.
func TestLoadRunFuncPassesBindAddresses(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := failFastLoadSpec()
	spec.BindAddresses = []string{badBind}
	fn, err := loadRunFunc(log, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fn(context.Background(), load.NewLiveStats(), func(string, string, string, string) {})
	if err == nil || !strings.Contains(err.Error(), wantBindErr) {
		t.Fatalf("run err = %v, want containing %q", err, wantBindErr)
	}
}
