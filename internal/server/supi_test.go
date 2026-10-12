package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
)

// newUEClient serves a ueService over a fresh Manager and returns a Connect
// client for it.
func newUEClient(t *testing.T) orbitv1connect.UEServiceClient {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := engine.NewManager(log)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: mgr, apps: mgr}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return orbitv1connect.NewUEServiceClient(http.DefaultClient, srv.URL)
}

// Every UE RPC that takes a SUPI rejects a malformed one as InvalidArgument
// with the shared message, before the Manager is consulted (a NotFound or
// FailedPrecondition here would mean the bogus key reached the session map).
func TestUERPCsRejectMalformedSUPI(t *testing.T) {
	const bad = "IMSI-208930100007500"
	c := newUEClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gnb := &orbitv1.GnbConfig{Id: 1, IdBits: 24, Name: "g", Mcc: "208", Mnc: "93", Tac: 1, Slices: []*orbitv1.Snssai{{Sst: 1}}}
	tests := []struct {
		name string
		call func() error
	}{
		{"Register", func() error {
			_, err := c.Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{AmfAddress: "127.0.0.1:1", Supi: bad, Gnb: gnb,
				Credentials: &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff", Opc: "000102030405060708090a0b0c0d0e0f"}}))
			return err
		}},
		{"Deregister", func() error {
			_, err := c.Deregister(ctx, connect.NewRequest(&orbitv1.DeregisterRequest{Supi: bad}))
			return err
		}},
		{"Status", func() error {
			_, err := c.Status(ctx, connect.NewRequest(&orbitv1.StatusRequest{Supi: bad}))
			return err
		}},
		{"Ping", func() error {
			_, err := c.Ping(ctx, connect.NewRequest(&orbitv1.PingRequest{Supi: bad, Destination: "8.8.8.8", Count: 1}))
			return err
		}},
		{"Traffic", func() error {
			_, err := c.Traffic(ctx, connect.NewRequest(&orbitv1.TrafficRequest{Supi: bad, Target: "8.8.8.8:9"}))
			return err
		}},
		{"Latency", func() error {
			_, err := c.Latency(ctx, connect.NewRequest(&orbitv1.LatencyRequest{Supi: bad, Target: "8.8.8.8:9"}))
			return err
		}},
		{"DataStats", func() error {
			_, err := c.DataStats(ctx, connect.NewRequest(&orbitv1.DataStatsRequest{Supi: bad}))
			return err
		}},
		{"Handover", func() error {
			_, err := c.Handover(ctx, connect.NewRequest(&orbitv1.HandoverRequest{Supi: bad, AmfAddress: "127.0.0.1:1", TargetGnb: gnb}))
			return err
		}},
		{"XnHandover", func() error {
			_, err := c.XnHandover(ctx, connect.NewRequest(&orbitv1.HandoverRequest{Supi: bad, AmfAddress: "127.0.0.1:1", TargetGnb: gnb}))
			return err
		}},
		{"StartApp", func() error {
			_, err := c.StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{Supi: bad, App: "voip"}))
			return err
		}},
		{"StateStream filter", func() error {
			stream, err := c.StateStream(ctx, connect.NewRequest(&orbitv1.StateStreamRequest{Supi: bad}))
			if err != nil {
				return err
			}
			stream.Receive()
			return stream.Err()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("code = %v (err %v), want InvalidArgument", connect.CodeOf(err), err)
			}
			if !strings.Contains(err.Error(), "want 5-15 digits or imsi-<digits>") {
				t.Errorf("error %q does not carry the shared SUPI message", err)
			}
		})
	}
}

// The #114 scenario end to end: a UE registered with the imsi- spelling
// passes NAS integrity (the server normalised before deriving keys), is
// stored and reported as digits, and is found by Status with either
// spelling; Deregister with the prefixed form removes it.
func TestRegisterPrefixedSUPIFoundByEitherForm(t *testing.T) {
	front := newAMFFront(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := engine.NewManager(log)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: mgr, apps: mgr}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewUEServiceClient(http.DefaultClient, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const digits = "208930100007500"
	res, err := c.Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{
		AmfAddress: front.addr, Supi: "imsi-" + digits,
		Gnb: &orbitv1.GnbConfig{Id: 1, IdBits: 24, Name: "orbit-gnb", Mcc: "208", Mnc: "93", Tac: 1,
			Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}},
		Credentials: &orbitv1.Credentials{Ki: "5122250214c33e723a5dd523fc145fc0", Opc: "981d464c7c52eb6e5036234984ad0bcf"},
	}))
	if err != nil {
		t.Fatalf("Register with the imsi- form: %v", err)
	}
	if !res.Msg.GetRegistered() || res.Msg.GetSupi() != digits {
		t.Fatalf("Register = registered %v supi %q, want registered as %q", res.Msg.GetRegistered(), res.Msg.GetSupi(), digits)
	}
	for _, form := range []string{digits, "imsi-" + digits} {
		st, err := c.Status(ctx, connect.NewRequest(&orbitv1.StatusRequest{Supi: form}))
		if err != nil {
			t.Fatalf("Status(%q): %v", form, err)
		}
		if st.Msg.GetStatus().GetSupi() != digits {
			t.Errorf("Status(%q) reports SUPI %q, want %q", form, st.Msg.GetStatus().GetSupi(), digits)
		}
	}
	list, err := c.List(ctx, connect.NewRequest(&orbitv1.ListRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(list.Msg.GetUes()); n != 1 {
		t.Errorf("List has %d UEs, want 1 (one session, not one per spelling)", n)
	}
	// Deregister with the prefixed form reaches the same session: the mock
	// AMF assigns no 5G-GUTI, so the deregistration itself cannot complete
	// here (a mock limit), but the engine must be answering about this UE,
	// never "not registered". An unknown SUPI shows the other outcome.
	_, err = c.Deregister(ctx, connect.NewRequest(&orbitv1.DeregisterRequest{Supi: "imsi-" + digits}))
	if err != nil && strings.Contains(err.Error(), "not registered") {
		t.Errorf("Deregister with the imsi- form did not find the session: %v", err)
	}
	_, err = c.Deregister(ctx, connect.NewRequest(&orbitv1.DeregisterRequest{Supi: "208930100007501"}))
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("Deregister of an unknown SUPI: err = %v, want not registered", err)
	}
}

// A load run's base_imsi accepts the imsi- spelling and rejects a malformed
// value before anything is launched, through the engine's LoadSpec.Validate
// (the same rule the in-process load applies, not a server-side copy).
func TestLoadRunFuncNormalizesBaseIMSI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := failFastLoadSpec()
	spec.BaseImsi = "imsi-" + spec.GetBaseImsi()
	if _, err := loadRunFunc(log, spec); err != nil {
		t.Errorf("imsi- base_imsi rejected: %v", err)
	}
	spec.BaseImsi = "IMSI-208930100000000"
	_, err := loadRunFunc(log, spec)
	if err == nil || !strings.Contains(err.Error(), `base IMSI: invalid SUPI "IMSI-208930100000000": want 5-15 digits or imsi-<digits>`) {
		t.Errorf("malformed base_imsi: err = %v, want the shared message", err)
	}
}
