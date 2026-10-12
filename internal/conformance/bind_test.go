package conformance

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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

// confirmAlive turns a PASS into a harness ERROR, never a core FAIL, when the
// liveness probe itself cannot be dialled; non-PASS results pass through.
func TestConfirmAliveProbeFailureIsError(t *testing.T) {
	env := Env{AMFAddr: "127.0.0.1:38412", Bind: "10.0.0.1:0:0"} // the probe's dial fails locally
	got := confirmAlive(context.Background(), env, Result{Verdict: Pass, Observed: "ok"}, "core died")
	if got.Verdict != Error {
		t.Fatalf("verdict = %s, want ERROR (the probe, not the core, failed)", got.Verdict)
	}
	if !strings.Contains(got.Detail, "liveness probe dial") || !strings.Contains(got.Observed, "ok") {
		t.Errorf("result does not explain the probe failure: %+v", got)
	}
	// DEVIATION is a surviving outcome too, so the probe applies to it.
	gotDev := confirmAlive(context.Background(), env, Result{Verdict: Deviation, Observed: "silently dropped"}, "core died")
	if gotDev.Verdict != Error || !strings.Contains(gotDev.Observed, "silently dropped") {
		t.Errorf("DEVIATION with a failed probe = %+v, want ERROR naming the observation", gotDev)
	}
	for _, v := range []Verdict{Fail, Error, Skip} {
		in := Result{Verdict: v, Observed: "x", Detail: "y"}
		if out := confirmAlive(context.Background(), env, in, "core died"); !reflect.DeepEqual(out, in) {
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
