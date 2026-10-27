package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/bgrewell/loom/control"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/target"
)

const (
	probeKi    = "000102030405060708090a0b0c0d0e0f"
	probeOPc   = "f0e0d0c0b0a090807060504030201000"
	probeLoom  = "orl1_PROBESECRETPROBESECRET"
	probeAMF   = "192.0.2.10:38412"
	probeFirst = "001010000000001"
)

// probeTarget is a target with a pool and, when resp is set, one responder.
func probeTarget(resp *orbitv1.Responder) *orbitv1.ResolvedTarget {
	t := &orbitv1.ResolvedTarget{
		Name: "lab1",
		Core: &orbitv1.TargetCore{Amf: probeAMF, Plmn: &orbitv1.Plmn{Mcc: "001", Mnc: "01"}, Tac: 1, Dnn: "internet",
			Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}, UePools: []string{"10.250.0.0/16"}, UpfN6: "198.51.100.1", N6Subnet: "198.51.100.0/24"},
		Subscribers: []*orbitv1.SubscriberPool{{Name: "main", FirstSupi: probeFirst, Ki: probeKi, Opc: probeOPc}},
		Ran:         &orbitv1.TargetRAN{N2SourceIps: []string{"192.0.2.50"}, N3Ips: []string{"192.0.2.50"}},
	}
	if resp != nil {
		t.App = &orbitv1.TargetApp{Responders: []*orbitv1.Responder{resp}}
	}
	return t
}

// probeFakes is a probeEnv whose every part answers well, counting the
// registrations it holds.
type probeFakes struct {
	mu           sync.Mutex
	registered   int // UEs registered and not deregistered
	deregistered int
	udpToken     string
}

func (f *probeFakes) env() probeEnv {
	return probeEnv{
		ngSetup: func(_ context.Context, req *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error) {
			return &orbitv1.RunNGSetupResponse{Accepted: true, AmfName: "amf1", LocalAddress: req.GetLocalAddress() + ":40000"}, nil
		},
		register: func(_ context.Context, req *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.registered++
			return &orbitv1.RegisterResponse{Registered: true, SessionActive: req.GetPduSession() != nil, PduAddress: "10.250.0.7", UpfAddress: "192.0.2.20"}, nil
		},
		deregister: func(context.Context, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.registered--
			f.deregistered++
		},
		resolve: func(name string) (join.Responder, error) {
			return join.Responder{Name: name, Control: "203.0.113.20:9551", LoomToken: probeLoom, DataIP: "203.0.113.20", PortLow: 40000, PortHigh: 40999}, nil
		},
		health: func(context.Context, string, string) error { return nil },
		echo:   func(context.Context, string, string) (int, error) { return 3, nil },
		udp: func(_ context.Context, _, _, token, _ string) (uint64, uint32, error) {
			f.mu.Lock()
			f.udpToken = token
			f.mu.Unlock()
			return 40, 40123, nil
		},
	}
}

// probeVia serves svc and runs one probe, returning its steps and error.
func probeVia(t *testing.T, ctx context.Context, svc *targetService, req *orbitv1.ProbeRequest, onStep func(*orbitv1.ProbeStep)) ([]*orbitv1.ProbeStep, error) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewTargetServiceHandler(svc))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	stream, err := orbitv1connect.NewTargetServiceClient(srv.Client(), srv.URL).Probe(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var steps []*orbitv1.ProbeStep
	for stream.Receive() {
		steps = append(steps, stream.Msg())
		if onStep != nil {
			onStep(stream.Msg())
		}
	}
	return steps, stream.Err()
}

func newProbeService(env probeEnv) (*targetService, *engine.RunRegistry) {
	reg := engine.NewRunRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	return &targetService{log: slog.New(slog.NewTextHandler(io.Discard, nil)), reg: reg, hold: reg.Hold, env: env}, reg
}

func statuses(steps []*orbitv1.ProbeStep) string {
	var s []string
	for _, st := range steps {
		s = append(s, st.GetStatus())
	}
	return strings.Join(s, " ")
}

// Each step's outcome, the rest SKIPped after a FAIL, what to fix named,
// no UE left registered, and no key or token in any step.
func TestProbeSteps(t *testing.T) {
	static := &orbitv1.Responder{Name: "app1", Control: "203.0.113.20:9551", Token: probeLoom, DataIp: "203.0.113.20"}
	named := &orbitv1.Responder{Name: "app1"}
	unauth := status.Error(codes.Unauthenticated, "invalid auth token")
	tests := []struct {
		name     string
		target   *orbitv1.ResolvedTarget
		coreOnly bool
		change   func(e *probeEnv)
		want     string   // the statuses, in order
		texts    []string // in some step's detail or fix
		absent   []string // in no step's detail or fix
		token    string   // the loom token the UDP check got
	}{
		{name: "all seven pass (registered responder)", target: probeTarget(named), want: "OK OK OK OK OK OK OK", token: probeLoom,
			texts: []string{"responder app1 registered, online, control 203.0.113.20:9551, data 203.0.113.20", "3 of 3 echoes from 203.0.113.20", "40 UDP datagrams arrived at 203.0.113.20:40123"}},
		{name: "all seven pass (static responder)", target: probeTarget(static), want: "OK OK OK OK OK OK OK", token: probeLoom,
			texts: []string{"static responder app1 (not registered)"}},
		{name: "core only", target: probeTarget(nil), coreOnly: true, want: "OK OK OK",
			texts: []string{"UE IP 10.250.0.7 via UPF 192.0.2.20"}},
		{name: "AMF unreachable", target: probeTarget(named), want: "FAIL SKIP SKIP SKIP SKIP SKIP SKIP",
			change: func(e *probeEnv) {
				e.ngSetup = func(context.Context, *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error) {
					return nil, connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
				}
			},
			texts: []string{"AMF 192.0.2.10:38412 is not reachable over SCTP from the orbit server: connection refused", "check core.amf and the N2 route", "after NG Setup failed"}},
		{name: "unknown PLMN", target: probeTarget(named), want: "FAIL SKIP SKIP SKIP SKIP SKIP SKIP",
			change: func(e *probeEnv) {
				e.ngSetup = func(context.Context, *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error) {
					return &orbitv1.RunNGSetupResponse{Cause: "misc(4)"}, nil
				}
			},
			texts: []string{"the AMF rejected NG Setup: misc(4) (unknown PLMN); core.plmn 001/01 or core.tac 1 is not one the AMF serves"}},
		{name: "no pool", target: func() *orbitv1.ResolvedTarget { t := probeTarget(named); t.Subscribers = nil; return t }(),
			want: "OK FAIL SKIP SKIP SKIP SKIP SKIP", texts: []string{"lab1 has no subscribers", "add subscribers[].first_supi, ki and opc to the target"}},
		{name: "no pool, core only: still a FAIL", target: func() *orbitv1.ResolvedTarget { t := probeTarget(nil); t.Subscribers = nil; return t }(),
			coreOnly: true, want: "OK FAIL SKIP", texts: []string{"lab1 has no subscribers"}},
		{name: "registration rejected", target: probeTarget(named), want: "OK FAIL SKIP SKIP SKIP SKIP SKIP",
			change: func(e *probeEnv) {
				e.register = func(context.Context, *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error) {
					return &orbitv1.RegisterResponse{}, nil
				}
			},
			texts: []string{"registering 001010000000001 failed: not registered (reject cause not reported yet)", "subscribers[main]"}},
		{name: "no PDU session accept", target: probeTarget(named), want: "OK OK FAIL SKIP SKIP SKIP SKIP",
			change: func(e *probeEnv) {
				inner := e.register
				e.register = func(ctx context.Context, req *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error) {
					if req.GetPduSession() != nil {
						return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("deadline exceeded"))
					}
					return inner(ctx, req)
				}
			},
			texts: []string{"no PDU session accept within 20s (DNN internet, slice 1/010203)", "core.dnn and core.slices[0]"}},
		{name: "no responder", target: probeTarget(nil), want: "OK OK OK FAIL SKIP SKIP SKIP",
			texts: []string{"no responder in lab1", "orbit join create -t lab1", "--core-only"}},
		{name: "registered responder offline", target: probeTarget(named), want: "OK OK OK FAIL SKIP SKIP SKIP",
			change: func(e *probeEnv) {
				e.resolve = func(name string) (join.Responder, error) { return join.Responder{}, &join.OfflineError{Name: name} }
			},
			texts: []string{"systemctl status orbit-responder"}},
		{name: "responder not registered", target: probeTarget(named), want: "OK OK OK FAIL SKIP SKIP SKIP",
			change: func(e *probeEnv) {
				e.resolve = func(string) (join.Responder, error) { return join.Responder{}, join.ErrNoResponder }
			},
			texts: []string{"responder app1 is not registered with this server", "orbit join create -t lab1 --name app1"}},
		{name: "static responder without data_ip", target: probeTarget(&orbitv1.Responder{Name: "app1", Control: "203.0.113.20:9551"}),
			want: "OK OK OK FAIL SKIP SKIP SKIP", texts: []string{"static responder app1 has no data_ip"}},
		{name: "loom token refused", target: probeTarget(named), want: "OK OK OK OK FAIL SKIP SKIP",
			change: func(e *probeEnv) { e.health = func(context.Context, string, string) error { return unauth } },
			texts:  []string{"the responder refused its loom token", "--reregister"}},
		{name: "control port unreachable", target: probeTarget(named), want: "OK OK OK OK FAIL SKIP SKIP",
			change: func(e *probeEnv) {
				e.health = func(context.Context, string, string) error {
					return status.Error(codes.Unavailable, "connection refused")
				}
			},
			texts: []string{"control port 9551 on 203.0.113.20 is unreachable from this server", "allow tcp 9551 from"}},
		{name: "no echo, no UDP: the route or the firewall", target: probeTarget(named), want: "OK OK OK OK OK FAIL FAIL",
			change: func(e *probeEnv) {
				e.echo = func(context.Context, string, string) (int, error) { return 0, nil }
				e.udp = func(context.Context, string, string, string, string) (uint64, uint32, error) { return 0, 40123, nil }
			},
			texts: []string{"no echo from 203.0.113.20 (3 sent)", "ip route replace 10.250.0.0/16 via 198.51.100.1 dev <n6 interface>",
				"allow icmp echo-request from 10.250.0.0/16", "no UDP datagram arrived at 203.0.113.20:40123", "allow udp 40000-40999 from 10.250.0.0/16 and 198.51.100.0/24"}},
		{name: "UDP but no echo: ICMP blocked", target: probeTarget(named), want: "OK OK OK OK OK FAIL OK", token: probeLoom,
			change: func(e *probeEnv) { e.echo = func(context.Context, string, string) (int, error) { return 0, nil } },
			texts:  []string{"though UDP reaches it: ICMP is blocked on the app host"}},
		{name: "echo but no UDP: the data ports", target: probeTarget(static), want: "OK OK OK OK OK OK FAIL",
			change: func(e *probeEnv) {
				e.udp = func(context.Context, string, string, string, string) (uint64, uint32, error) { return 0, 40123, nil }
			},
			texts: []string{"allow udp its receive ports from"}},
		{name: "the receiver's telemetry ends early: no firewall advice", target: probeTarget(named), want: "OK OK OK OK OK OK FAIL",
			change: func(e *probeEnv) {
				e.udp = func(context.Context, string, string, string, string) (uint64, uint32, error) {
					return 0, 40123, &errTelemetryEnded{err: io.EOF}
				}
			},
			texts:  []string{"the responder's flow telemetry ended before the count was read: EOF"},
			absent: []string{"allow udp", "no UDP datagram arrived"}},
		{name: "the UDP check cannot run", target: probeTarget(named), want: "OK OK OK OK OK OK FAIL",
			change: func(e *probeEnv) {
				e.udp = func(context.Context, string, string, string, string) (uint64, uint32, error) {
					return 0, 0, errors.New("loom: max flows reached")
				}
			},
			texts: []string{"the UDP check could not run: loom: max flows reached"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &probeFakes{}
			env := f.env()
			if tc.change != nil {
				tc.change(&env)
			}
			svc, _ := newProbeService(env)
			steps, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: tc.target, TargetName: "lab1", CoreOnly: tc.coreOnly}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := statuses(steps); got != tc.want {
				t.Errorf("statuses = %q, want %q", got, tc.want)
			}
			var all strings.Builder
			for i, s := range steps {
				if int(s.GetIndex()) != i+1 || s.GetName() != probeStepNames[i] {
					t.Errorf("step %d is %d %q", i+1, s.GetIndex(), s.GetName())
				}
				all.WriteString(s.GetDetail() + "\n" + s.GetFix() + "\n")
			}
			for _, w := range tc.texts {
				if !strings.Contains(all.String(), w) {
					t.Errorf("no step says %q:\n%s", w, all.String())
				}
			}
			for _, w := range tc.absent {
				if strings.Contains(all.String(), w) {
					t.Errorf("a step says %q:\n%s", w, all.String())
				}
			}
			for _, secret := range []string{probeKi, probeOPc, probeLoom, "orl1_"} {
				if strings.Contains(all.String(), secret) {
					t.Errorf("a step shows a secret (%s…)", secret[:5])
				}
			}
			if f.registered != 0 {
				t.Errorf("%d UEs left registered", f.registered)
			}
			if f.udpToken != tc.token {
				t.Errorf("the UDP check got token %q, want %q", f.udpToken, tc.token)
			}
		})
	}
}

// The probe leases what it uses: a run holding its gNB ID refuses it,
// naming the holder; once it ends, the leases are free again.
func TestProbeLeases(t *testing.T) {
	f := &probeFakes{}
	svc, reg := newProbeService(f.env())
	// Each resource alone: a run holding just it refuses the probe.
	for _, held := range []struct {
		name  string
		needs engine.Needs
	}{
		{name: "its third gNB ID", needs: engine.Needs{GNBs: []engine.GNBKey{{AMF: probeAMF, PLMN: "001/01", ID: 3}}}},
		{name: "its SUPI", needs: engine.Needs{SUPIs: []engine.SUPIBlock{{PLMN: "001/01", First: 1010000000001, Last: 1010000000001}}}},
		{name: "its N2 source", needs: engine.Needs{N2IPs: []string{"192.0.2.50"}}},
	} {
		t.Run(held.name, func(t *testing.T) {
			release, err := reg.Hold("run-7", held.needs)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			steps, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: probeTarget(nil), CoreOnly: true}, nil)
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "run-7") || len(steps) != 0 {
				t.Fatalf("err = %v after %d steps, want FailedPrecondition naming run-7 before any", err, len(steps))
			}
		})
	}
	if _, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: probeTarget(nil), CoreOnly: true}, nil); err != nil {
		t.Fatal(err)
	}
	// The probe's own leases went with it.
	for _, n := range []engine.Needs{
		{GNBs: []engine.GNBKey{{AMF: probeAMF, PLMN: "001/01", ID: 1}}},
		{N2IPs: []string{"192.0.2.50"}},
		{SUPIs: []engine.SUPIBlock{{PLMN: "001/01", First: 1010000000001, Last: 1010000000001}}},
	} {
		rel, err := reg.Hold("after", n)
		if err != nil {
			t.Errorf("still held after the probe: %v", err)
			continue
		}
		rel()
	}
}

// A probe cancelled mid-way (the client gone) still deregisters its UE.
func TestProbeCancelledDeregisters(t *testing.T) {
	f := &probeFakes{}
	env := f.env()
	env.udp = func(ctx context.Context, _, _, _, _ string) (uint64, uint32, error) {
		<-ctx.Done()
		return 0, 0, ctx.Err()
	}
	svc, _ := newProbeService(env)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = probeVia(t, ctx, svc, &orbitv1.ProbeRequest{Target: probeTarget(&orbitv1.Responder{Name: "app1"})}, func(s *orbitv1.ProbeStep) {
		if s.GetIndex() == 5 {
			cancel()
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		left := f.registered
		f.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d UEs left registered after the cancel", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The UDP check against a real loom agent: datagrams to its receiver are
// counted, a token it does not hold is refused, and the receiver is
// destroyed on every path, a cancel included (the agent takes one flow at
// a time, so a receiver left behind fails the next check).
func TestLoomUDPCheck(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := control.NewServer("test", control.WithAuthToken(probeLoom), control.WithMaxFlows(1), control.WithTelemetryInterval(100*time.Millisecond))
	gs := control.NewGRPCServer(srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	addr := lis.Addr().String()
	udp := func(ctx context.Context, target string) error {
		c, err := net.Dial("udp", target)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		for i := 0; i < 20; i++ {
			if _, err := c.Write(make([]byte, 200)); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}
	silent := func(context.Context, string) error { return nil }
	var cancelled atomic.Bool
	cancelling := func(ctx context.Context, _ string) error {
		cancelled.Store(true)
		return context.Canceled
	}
	tests := []struct {
		name    string
		send    func(context.Context, string) error
		token   string
		wantN   bool
		wantErr bool
	}{
		{name: "datagrams counted", send: udp, token: probeLoom, wantN: true},
		{name: "again: the first receiver is gone", send: udp, token: probeLoom, wantN: true},
		{name: "nothing sent: none counted", send: silent, token: probeLoom},
		{name: "the sender fails", send: cancelling, token: probeLoom, wantErr: true},
		{name: "after the failure: its receiver is gone", send: udp, token: probeLoom, wantN: true},
		{name: "a wrong token", send: udp, token: "orl1_WRONG", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			n, port, err := loomUDPCheck(ctx, tc.send, addr, tc.token, "127.0.0.1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if (n > 0) != tc.wantN {
				t.Errorf("counted %d, want any: %v", n, tc.wantN)
			}
			if !tc.wantErr && port == 0 {
				t.Error("no receiver port")
			}
		})
	}
	if !cancelled.Load() {
		t.Error("the failing sender never ran")
	}
}

// Each association takes its own gNB ID: the range's first, first+1 and
// first+2 (a core may not cleanly re-key a reused ID); with no range, the
// default first.
func TestProbeGNBIDs(t *testing.T) {
	tests := []struct {
		name  string
		first uint32
		want  []uint32
	}{
		{name: "the target's range", first: 100, want: []uint32{100, 101, 102}},
		{name: "the default range", first: 0, want: []uint32{target.DefaultGNBID, target.DefaultGNBID + 1, target.DefaultGNBID + 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &probeFakes{}
			env := f.env()
			var mu sync.Mutex
			var ids []uint32
			ng, reg := env.ngSetup, env.register
			env.ngSetup = func(ctx context.Context, req *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error) {
				mu.Lock()
				ids = append(ids, req.GetGnb().GetId())
				mu.Unlock()
				return ng(ctx, req)
			}
			env.register = func(ctx context.Context, req *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error) {
				mu.Lock()
				ids = append(ids, req.GetGnb().GetId())
				mu.Unlock()
				return reg(ctx, req)
			}
			svc, _ := newProbeService(env)
			tg := probeTarget(nil)
			if tc.first != 0 {
				tg.Ran.GnbIds = &orbitv1.GNBIDRange{First: tc.first}
			}
			if _, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: tg, CoreOnly: true}, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Errorf("gNB IDs %v, want %v", ids, tc.want)
			}
		})
	}
}

// A step timeout is bounded, since the probe holds its leases for it: 0 is
// the default, 2m is the most, and more is refused before anything runs.
func TestProbeStepTimeoutBound(t *testing.T) {
	tests := []struct {
		name     string
		ms       uint32
		wantCode connect.Code // 0: no error
	}{
		{name: "0 is the default", ms: 0},
		{name: "2m", ms: 120000},
		{name: "past 2m", ms: 120001, wantCode: connect.CodeInvalidArgument},
		{name: "about 49 days", ms: 1<<32 - 1, wantCode: connect.CodeInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &probeFakes{}
			svc, _ := newProbeService(f.env())
			steps, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: probeTarget(nil), CoreOnly: true, StepTimeoutMs: tc.ms}, nil)
			switch {
			case tc.wantCode == 0 && err != nil:
				t.Fatal(err)
			case tc.wantCode != 0 && connect.CodeOf(err) != tc.wantCode:
				t.Fatalf("err = %v, want %v", err, tc.wantCode)
			}
			if tc.wantCode != 0 && len(steps) != 0 {
				t.Errorf("%d steps ran before the refusal", len(steps))
			}
		})
	}
}

// The agent gone mid-check: its telemetry ends before a count, which is
// the check not running (errTelemetryEnded), not "no datagram arrived".
func TestLoomUDPCheckTelemetryEnds(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := control.NewGRPCServer(control.NewServer("test", control.WithAuthToken(probeLoom), control.WithTelemetryInterval(100*time.Millisecond)))
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	gone := func(context.Context, string) error {
		gs.Stop()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = loomUDPCheck(ctx, gone, lis.Addr().String(), probeLoom, "127.0.0.1")
	var ended *errTelemetryEnded
	if !errors.As(err, &ended) {
		t.Fatalf("err = %v, want the telemetry ended", err)
	}
}
