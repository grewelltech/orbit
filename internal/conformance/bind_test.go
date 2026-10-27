package conformance

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"

	"github.com/bgrewell/orbit/internal/gnb"
)

// Env.Dial dials from Bind: a malformed one fails in sctp.Dial's
// normalisation, before any socket is opened, naming the input.
func TestEnvDialUsesBind(t *testing.T) {
	_, err := Env{AMFAddr: "127.0.0.1:38412", Bind: "10.0.0.1:0:0"}.Dial(context.Background())
	if err == nil || !strings.Contains(err.Error(), `invalid local address "10.0.0.1:0:0"`) {
		t.Fatalf("err = %v, want the normalisation error for the bind", err)
	}
}

func TestValidateBind(t *testing.T) {
	tests := []struct {
		bind    string
		wantErr string
	}{
		{bind: ""},
		{bind: "127.0.0.1"},
		{bind: "127.0.0.1:0"},
		{bind: "::1"},
		{bind: "[::1]:0"},
		{bind: "fe80::1%eth0"},
		{bind: "ran-21"},
		{bind: "10.0.0.1:38412", wantErr: "fixes a port"},
		{bind: "[::1]:38412", wantErr: "fixes a port"},
		{bind: "ran-21:38412", wantErr: "fixes a port"},
		{bind: "10.0.0.1:0:0", wantErr: `invalid local address "10.0.0.1:0:0"`},
	}
	for _, tc := range tests {
		t.Run(tc.bind, func(t *testing.T) {
			err := ValidateBind(tc.bind)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateBind(%q): %v", tc.bind, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateBind(%q) = %v, want containing %q", tc.bind, err, tc.wantErr)
			}
		})
	}
}

// N2Source is the normalised bind when one is given, and the route-selected
// IP for the AMF (bare, no port) when not; a probe that cannot run yields "".
func TestEnvN2Source(t *testing.T) {
	tests := []struct{ name, amf, bind, want string }{
		{"unbound: route-selected ip for a loopback amf", "127.0.0.1:38412", "", "127.0.0.1"},
		{"unbound: unresolvable amf gives nothing", "no-such-amf.invalid:38412", "", ""},
		{"bare ip", "127.0.0.1:38412", "127.0.0.1", "127.0.0.1:0"},
		{"ip with fixed port", "127.0.0.1:38412", "10.0.0.21:38412", "10.0.0.21:38412"},
		{"bare ipv6", "127.0.0.1:38412", "::1", "[::1]:0"},
		{"hostname", "127.0.0.1:38412", "ran-21", "ran-21:0"},
		{"unparseable: reported as written", "127.0.0.1:38412", "10.0.0.1:0:0", "10.0.0.1:0:0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Env{AMFAddr: tc.amf, Bind: tc.bind}).N2Source(); got != tc.want {
				t.Errorf("Env{AMFAddr: %q, Bind: %q}.N2Source() = %q, want %q", tc.amf, tc.bind, got, tc.want)
			}
		})
	}
}

// confirmAlive turns a surviving result into a harness ERROR, never a core
// FAIL, when the liveness probe itself cannot be dialled; a result whose
// association did not survive passes through.
func TestConfirmAliveProbeFailureIsError(t *testing.T) {
	env := Env{AMFAddr: "127.0.0.1:38412", Bind: "10.0.0.1:0:0"} // the probe's dial fails locally
	got := confirmAlive(context.Background(), env, Result{Verdict: Pass, Observed: "ok"}, true, "core died")
	if got.Verdict != Error {
		t.Fatalf("verdict = %s, want ERROR (the probe, not the core, failed)", got.Verdict)
	}
	if !strings.Contains(got.Detail, "liveness probe dial") || !strings.Contains(got.Observed, "ok") {
		t.Errorf("result does not explain the probe failure: %+v", got)
	}
	// A FAIL whose association survived (a silent drop) is probed too.
	gotDrop := confirmAlive(context.Background(), env, Result{Verdict: Fail, Observed: "silently dropped"}, true, "core died")
	if gotDrop.Verdict != Error || !strings.Contains(gotDrop.Observed, "silently dropped") {
		t.Errorf("surviving FAIL with a failed probe = %+v, want ERROR naming the observation", gotDrop)
	}
	for _, v := range []Verdict{Fail, Error, Skip} {
		in := Result{Verdict: v, Observed: "x", Detail: "y"}
		if out := confirmAlive(context.Background(), env, in, false, "core died"); !reflect.DeepEqual(out, in) {
			t.Errorf("confirmAlive changed a %s result: %+v", v, out)
		}
	}
}

// The JSON summary records the N2 source, bound (ip:0) or route-selected
// (bare ip), and omits the key only when there is none to report.
func TestSummaryJSONN2Source(t *testing.T) {
	sum := Summarize(nil)
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "N2Source") {
		t.Errorf("empty N2Source serialised: %s", b)
	}
	tests := []struct{ name, bind, want string }{
		{"bound", "10.0.0.21", "10.0.0.21:0"},
		{"unbound", "", "127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sum.N2Source = (Env{AMFAddr: "127.0.0.1:38412", Bind: tc.bind}).N2Source()
			b, err := json.Marshal(sum)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if got["N2Source"] != tc.want {
				t.Errorf("N2Source in JSON = %v, want %s (got %s)", got["N2Source"], tc.want, b)
			}
		})
	}
}

// A liveness probe the per-test timeout cuts short is a harness ERROR that
// keeps what was observed, never a crash FAIL: here the AMF accepts the
// association and never answers the probe's NG Setup before ctx expires.
func TestConfirmAliveCutShortIsError(t *testing.T) {
	ln, err := isctp.ListenSCTP("sctp", &isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() }) // held open, never answered
		}
	}()
	env := Env{AMFAddr: ln.Addr().String(), GNB: gnb.Config{ID: 1, Name: "g", MCC: "001", MNC: "01", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got := confirmAlive(ctx, env, Result{Verdict: Fail, Observed: "silently dropped; association survived"}, true, "core died")
	if got.Verdict != Error || got.Observed != ObservedProbeCutShort+" (silently dropped; association survived)" ||
		!strings.Contains(got.Detail, "--per-test-timeout") {
		t.Errorf("= %+v, want ERROR cut short", got)
	}
}

// A liveness miss while a population loads the AMF is an ERROR naming the
// population, not the check's FAIL; with no population it stays a FAIL.
func TestConfirmAliveUnderLoadIsError(t *testing.T) {
	ln, err := isctp.ListenSCTP("sctp", &isctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}})
	if err != nil {
		t.Skipf("no kernel SCTP on this host: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close() // no NG Setup answer: the AMF is "dead"
		}
	}()
	tests := []struct {
		name        string
		load        string
		wantVerdict Verdict
		wantPrefix  string
	}{
		{name: "under load", load: "fleet", wantVerdict: Error, wantPrefix: ObservedAMFUnresponsiveUnderLoad},
		{name: "idle core", wantVerdict: Fail, wantPrefix: ObservedAMFUnresponsive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{AMFAddr: ln.Addr().String(), Load: func() string { return tc.load },
				GNB: gnb.Config{ID: 1, Name: "g", MCC: "001", MNC: "01", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1}}}}
			got := confirmAlive(context.Background(), env, Result{Verdict: Pass, Observed: "ok"}, true, "core died")
			if got.Verdict != tc.wantVerdict || !strings.HasPrefix(got.Observed, tc.wantPrefix+" (ok)") {
				t.Fatalf("= %s %q, want %s %q", got.Verdict, got.Observed, tc.wantVerdict, tc.wantPrefix)
			}
			if tc.load != "" && got.Detail != "AMF unresponsive while population fleet was active; not attributable to this check" {
				t.Errorf("detail = %q", got.Detail)
			}
		})
	}
}
