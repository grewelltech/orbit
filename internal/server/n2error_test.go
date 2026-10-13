package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/sctp"
)

// n2Error gives every N2-opening RPC one code per fact: the caller's bad
// target is InvalidArgument, a cancelled dial Canceled, any other dial
// failure Unavailable whichever timer noticed the silent AMF, and anything
// else Internal, a deadline after a successful dial included. The message is
// the error's, unchanged.
func TestN2Error(t *testing.T) {
	dial := func(cause error) error {
		return &sctp.DialError{Remote: "192.0.2.1:38412", Source: "127.0.0.1:0", Err: fmt.Errorf(`sctp dial "192.0.2.1:38412" from "127.0.0.1:0": %w`, cause)}
	}
	tests := []struct {
		name string
		err  error
		want connect.Code
	}{
		{name: "invalid handover target", err: fmt.Errorf("handover: %w: mcc", engine.ErrInvalidTarget), want: connect.CodeInvalidArgument},
		{name: "refused", err: dial(syscall.ECONNREFUSED), want: connect.CodeUnavailable},
		{name: "no INIT-ACK, kernel gave up", err: dial(syscall.ETIMEDOUT), want: connect.CodeUnavailable},
		{name: "no INIT-ACK, bound fired", err: dial(context.DeadlineExceeded), want: connect.CodeUnavailable},
		{name: "family mismatch", err: dial(errors.New("address family mismatch")), want: connect.CodeUnavailable},
		{name: "cancelled dial", err: dial(context.Canceled), want: connect.CodeCanceled},
		{name: "dial behind Register's wrap", err: fmt.Errorf("SCTP dial: %w", dial(syscall.ECONNREFUSED)), want: connect.CodeUnavailable},
		{name: "dial behind the handover's wrap", err: fmt.Errorf("dial target gNB: %w", dial(context.Canceled)), want: connect.CodeCanceled},
		{name: "deadline after the dial", err: fmt.Errorf("registration: %w", context.DeadlineExceeded), want: connect.CodeInternal},
		{name: "cancel after the dial", err: fmt.Errorf("registration: %w", context.Canceled), want: connect.CodeInternal},
		{name: "engine failure", err: errors.New("UE 001010000000001 is not registered"), want: connect.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := n2Error(tc.err)
			var ce *connect.Error
			if !errors.As(got, &ce) {
				t.Fatalf("n2Error(%v) = %v (%T), not a *connect.Error", tc.err, got, got)
			}
			if ce.Code() != tc.want || ce.Message() != tc.err.Error() {
				t.Errorf("code %v message %q, want %v %q", ce.Code(), ce.Message(), tc.want, tc.err.Error())
			}
		})
	}
	if n2Error(nil) != nil {
		t.Error("n2Error(nil) is not nil")
	}
}

// A refused AMF reads Unavailable from every RPC that dials it, with the
// dial's own text: the regression was Internal from Register.
func TestN2RPCsDialFailureUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cell, _ := newCellClient(t)
	ueClient := newUEClient(t)
	gnb := &orbitv1.GnbConfig{Id: 1, IdBits: 24, Name: "g", Mcc: "208", Mnc: "93", Tac: 1, Slices: []*orbitv1.Snssai{{Sst: 1}}}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"RunNGSetup", func() error {
			_, err := cell.RunNGSetup(ctx, connect.NewRequest(ngsetupReq("127.0.0.1:1", 0, 0)))
			return err
		}},
		{"Register", func() error {
			_, err := ueClient.Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{AmfAddress: "127.0.0.1:1", Supi: "208930100007500", Gnb: gnb,
				Credentials: &orbitv1.Credentials{Ki: "5122250214c33e723a5dd523fc145fc0", Opc: "981d464c7c52eb6e5036234984ad0bcf"}}))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), `sctp dial "127.0.0.1:1"`) {
				t.Errorf("err = %v (code %v), want Unavailable with the dial text", err, connect.CodeOf(err))
			}
		})
	}
}

// With a UE registered on the mock AMF, a handover whose target gNB cannot
// reach its AMF is Unavailable (was Internal); a registration that dials
// fine and then fails is still Internal.
func TestN2HandoverAndPostDialFailures(t *testing.T) {
	front := newAMFFront(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := engine.NewManager(log)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(&ueService{log: log, mgr: mgr, apps: mgr}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := orbitv1connect.NewUEServiceClient(http.DefaultClient, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gnb := func(id uint32) *orbitv1.GnbConfig {
		return &orbitv1.GnbConfig{Id: id, IdBits: 24, Name: "orbit-gnb", Mcc: "208", Mnc: "93", Tac: 1,
			Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}}
	}

	// Wrong keys: the association and NG Setup succeed, authentication
	// does not, and the registration times out after the dial.
	_, err := c.Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{
		AmfAddress: front.addr, Supi: "208930100007501", Gnb: gnb(9), TimeoutMs: 2000,
		Credentials: &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff", Opc: "000102030405060708090a0b0c0d0e0f"},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("post-dial registration failure: err = %v (code %v), want Internal", err, connect.CodeOf(err))
	}

	const supi = "208930100007500"
	if _, err := c.Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{
		AmfAddress: front.addr, Supi: supi, Gnb: gnb(1),
		Credentials: &orbitv1.Credentials{Ki: "5122250214c33e723a5dd523fc145fc0", Opc: "981d464c7c52eb6e5036234984ad0bcf"},
	})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, call := range []struct {
		name string
		fn   func(context.Context, *connect.Request[orbitv1.HandoverRequest]) (*connect.Response[orbitv1.HandoverResponse], error)
	}{{"Handover", c.Handover}, {"XnHandover", c.XnHandover}} {
		t.Run(call.name, func(t *testing.T) {
			_, err := call.fn(ctx, connect.NewRequest(&orbitv1.HandoverRequest{Supi: supi, AmfAddress: "127.0.0.1:1", TargetGnb: gnb(2)}))
			if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), `dial target gNB: sctp dial "127.0.0.1:1"`) {
				t.Errorf("err = %v (code %v), want Unavailable with the dial text", err, connect.CodeOf(err))
			}
		})
	}
}

// Every server call that can dial an AMF hands its error to n2Error: the
// first error return after the call goes through it, so no handler keeps a
// private mapping around a dialling path.
func TestN2CallSitesUseN2Error(t *testing.T) {
	callRE := regexp.MustCompile(`\b(mgr\.Register|mgr\.Handover|mgr\.XnHandover|sctp\.Dial)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !callRE.MatchString(l) {
				continue
			}
			found++
			ok := false
			for _, next := range lines[i+1 : min(i+12, len(lines))] {
				if strings.Contains(next, "return") {
					ok = strings.Contains(next, "n2Error(err)")
					break
				}
			}
			if !ok {
				t.Errorf("%s:%d: %s does not return its error through n2Error", f, i+1, strings.TrimSpace(l))
			}
		}
	}
	if found < 4 {
		t.Errorf("found %d dialling call sites, want at least 4 (RunNGSetup, Register, Handover, XnHandover)", found)
	}
}
