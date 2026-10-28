package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/plan"
	"github.com/bgrewell/orbit/internal/target"
)

// A probe's target is resolved as the CLI loads one: what it leaves unset
// takes the built-in defaults (TAC 1, slice 1, DNN internet), and a field
// the CLI's checks refuse is InvalidArgument "invalid target: <path>:
// <reason>" before any lease is taken or step run.
func TestProbeSentTarget(t *testing.T) {
	tests := []struct {
		name     string
		change   func(*orbitv1.ResolvedTarget)
		wantErr  string // the whole message after the code
		wantReg  func(t *testing.T, ngTAC uint32, pdu *orbitv1.PDUSession)
		wantPass string
	}{
		{name: "unset tac, slices and dnn take the defaults", change: func(x *orbitv1.ResolvedTarget) {
			x.Core.Tac, x.Core.Slices, x.Core.Dnn = 0, nil, ""
		}, wantPass: "OK OK OK OK OK OK OK", wantReg: func(t *testing.T, tac uint32, pdu *orbitv1.PDUSession) {
			if tac != 1 || pdu.GetSst() != 1 || pdu.GetSd() != "" || pdu.GetDnn() != "internet" {
				t.Errorf("NG Setup TAC %d, PDU session %v; want TAC 1, sst 1, dnn internet", tac, pdu)
			}
		}},
		// tac: 0 in a file is a value the CLI accepts, but the wire cannot
		// tell it from unset: it is probed as the default, 1.
		{name: "tac 0 is probed as the default 1", change: func(x *orbitv1.ResolvedTarget) { x.Core.Tac = 0 },
			wantPass: "OK OK OK OK OK OK OK", wantReg: func(t *testing.T, tac uint32, _ *orbitv1.PDUSession) {
				if tac != 1 {
					t.Errorf("NG Setup TAC %d, want 1", tac)
				}
			}},
		{name: "a bad SD", change: func(x *orbitv1.ResolvedTarget) { x.Core.Slices[0].Sd = "xyz" },
			wantErr: `invalid target: core.slices: [1/xyz] SD "xyz" is not 6 hex digits`},
		{name: "a bad MNC", change: func(x *orbitv1.ResolvedTarget) { x.Core.Plmn.Mnc = "1" },
			wantErr: `invalid target: core.plmn.mnc: "1" is not 2 or 3 digits`},
		{name: "a key that is not 32 hex digits, masked", change: func(x *orbitv1.ResolvedTarget) { x.Subscribers[0].Ki = "abc" },
			wantErr: "invalid target: subscribers[main].ki: <redacted> is not 32 hex digits"},
		{name: "two pools named alike", change: func(x *orbitv1.ResolvedTarget) {
			x.Subscribers = append(x.Subscribers, proto.Clone(x.Subscribers[0]).(*orbitv1.SubscriberPool))
		}, wantErr: `invalid target: subscribers[main].name: "main" twice; subscribers need distinct names`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &probeFakes{}
			env := f.env()
			var mu sync.Mutex
			var ngTAC uint32
			var pdu *orbitv1.PDUSession
			ng, reg := env.ngSetup, env.register
			env.ngSetup = func(ctx context.Context, r *orbitv1.RunNGSetupRequest) (*orbitv1.RunNGSetupResponse, error) {
				mu.Lock()
				ngTAC = r.GetGnb().GetTac()
				mu.Unlock()
				return ng(ctx, r)
			}
			env.register = func(ctx context.Context, r *orbitv1.RegisterRequest) (*orbitv1.RegisterResponse, error) {
				if r.GetPduSession() != nil {
					mu.Lock()
					pdu = r.GetPduSession()
					mu.Unlock()
				}
				return reg(ctx, r)
			}
			svc, _ := newProbeService(env)
			held := false
			hold := svc.hold
			svc.hold = func(h string, n engine.Needs) (func(), error) { held = true; return hold(h, n) }
			tg := probeTarget(&orbitv1.Responder{Name: "app1"})
			tc.change(tg)
			steps, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: tg, TargetName: "lab1"}, nil)
			if tc.wantErr != "" {
				if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.HasSuffix(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want InvalidArgument %q", err, tc.wantErr)
				}
				if held || len(steps) != 0 {
					t.Errorf("refused after a lease (%v) or a step (%d)", held, len(steps))
				}
				if strings.Contains(err.Error(), probeKi) || strings.Contains(err.Error(), probeOPc) {
					t.Errorf("a key in %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := statuses(steps); got != tc.wantPass {
				t.Errorf("steps %s, want %s", got, tc.wantPass)
			}
			mu.Lock()
			defer mu.Unlock()
			tc.wantReg(t, ngTAC, pdu)
		})
	}
}

// What the CLI sends, a target it has already resolved, is what the probe
// runs: resolving it again changes nothing, so the CLI's probes go as
// they did before the server resolved sent targets.
func TestProbeCLITargetUnchanged(t *testing.T) {
	dir := filepath.Join("..", "target", "testdata", "roundtrip", "cli")
	env := map[string]string{"LAB_AMF": "10.0.0.1:38412", "ORBIT_KI": probeKi, "ORBIT_OPC": probeOPc,
		"ROAM_KI": probeKi, "ROAM_OPC": probeOPc, "ORBIT_RESPONDER_TOKEN_0": "tok"}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CLI target files in %s: %v", dir, err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fs, err := target.Parse(path, b, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			if err != nil {
				t.Fatal(err)
			}
			// As `orbit target probe` resolves it: defaults < target.
			res, err := target.Resolve(target.DefaultInput(), fs[0].Input)
			if err != nil {
				t.Fatal(err)
			}
			sent := plan.TargetProto(res.Target)
			got, err := sentTarget(sent)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, sent) {
				t.Errorf("resolving the CLI's target again changed it:\n got %v\nwant %v", got, sent)
			}
		})
	}
	// And the steps: a probe of a resolved target, sent as it is and
	// resolved once more first, gives the same steps, word for word.
	run := func(tg *orbitv1.ResolvedTarget) string {
		f := &probeFakes{}
		svc, _ := newProbeService(f.env())
		steps, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: tg, TargetName: "lab1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, s := range steps {
			b.WriteString(s.GetStatus() + " " + s.GetName() + ": " + s.GetDetail() + " / " + s.GetFix() + "\n")
		}
		return b.String()
	}
	tg := probeTarget(&orbitv1.Responder{Name: "app1"})
	tg.Core.Profile = "strict-3gpp" // as the CLI's resolution has it
	again, err := sentTarget(tg)
	if err != nil {
		t.Fatal(err)
	}
	before, after := run(tg), run(again)
	if before != after || !strings.HasPrefix(before, "OK") {
		t.Errorf("steps before:\n%s\nafter:\n%s", before, after)
	}
}

// The server never fills a sent field from its own environment: a key
// sent as the text ${ORBIT_KI} stays that text, which is no key, though
// this server's ORBIT_KI holds a good one.
func TestProbeDoesNotExpandServerEnv(t *testing.T) {
	t.Setenv("ORBIT_KI", probeKi)
	t.Setenv("LAB_AMF", probeAMF)
	f := &probeFakes{}
	svc, _ := newProbeService(f.env())
	tg := probeTarget(&orbitv1.Responder{Name: "app1"})
	tg.Subscribers[0].Ki = "${ORBIT_KI}"
	_, err := probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: tg, TargetName: "lab1"}, nil)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "subscribers[main].ki: <redacted> is not 32 hex digits") {
		t.Fatalf("err = %v, want the unexpanded key refused", err)
	}
	if strings.Contains(err.Error(), probeKi) {
		t.Errorf("the server's ORBIT_KI is in %v", err)
	}
	tg = probeTarget(&orbitv1.Responder{Name: "app1"})
	tg.Core.Amf = "${LAB_AMF}"
	_, err = probeVia(t, context.Background(), svc, &orbitv1.ProbeRequest{Target: tg, TargetName: "lab1"}, nil)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), `core.amf: "${LAB_AMF}"`) {
		t.Fatalf("err = %v, want ${LAB_AMF} refused as written", err)
	}
	if f.registered != 0 || f.deregistered != 0 {
		t.Errorf("a UE was registered (%d/%d)", f.registered, f.deregistered)
	}
}
