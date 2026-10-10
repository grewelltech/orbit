package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

func TestNetIdentityResolve(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		envMCC, envMNC string
		required       bool
		wantMCC        string
		wantMNC        string
		wantErr        string
	}{
		{name: "flags only", args: []string{"--mcc", "001", "--mnc", "01"}, required: true, wantMCC: "001", wantMNC: "01"},
		{name: "env only", envMCC: "208", envMNC: "93", required: true, wantMCC: "208", wantMNC: "93"},
		{name: "both: flags win", args: []string{"--mcc", "001", "--mnc", "01"}, envMCC: "208", envMNC: "93", required: true, wantMCC: "001", wantMNC: "01"},
		{name: "three-digit mnc from env keeps leading zero", envMCC: "001", envMNC: "001", required: true, wantMCC: "001", wantMNC: "001"},
		{name: "neither, required", required: true, wantErr: "--mcc/--mnc or set $ORBIT_MCC/$ORBIT_MNC"},
		{name: "neither, optional: inherit", required: false, wantMCC: "", wantMNC: ""},
		{name: "only mcc, optional", args: []string{"--mcc", "001"}, required: false, wantErr: "go together"},
		{name: "only mnc from env, required", envMNC: "01", required: true, wantErr: "go together"},
		{name: "invalid env mcc", envMCC: "1", envMNC: "01", required: true, wantErr: "mcc"},
		{name: "invalid flag mnc", args: []string{"--mcc", "001", "--mnc", "1"}, required: true, wantErr: "mnc"},
		{name: "tac zero", args: []string{"--mcc", "001", "--mnc", "01", "--tac", "0"}, required: true, wantErr: "tac"},
		{name: "tac too large", args: []string{"--mcc", "001", "--mnc", "01", "--tac", "16777216"}, required: true, wantErr: "tac"},
		{name: "sst too large", args: []string{"--mcc", "001", "--mnc", "01", "--sst", "256"}, required: true, wantErr: "sst"},
		{name: "sd not hex", args: []string{"--mcc", "001", "--mnc", "01", "--sd", "zzzzzz"}, required: true, wantErr: "sd"},
		{name: "sd 5 digits", args: []string{"--mcc", "001", "--mnc", "01", "--sd", "01020"}, required: true, wantErr: "sd"},
		{name: "empty dnn", args: []string{"--mcc", "001", "--mnc", "01", "--dnn", ""}, required: true, wantErr: "--dnn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_MCC", tc.envMCC)
			t.Setenv("ORBIT_MNC", tc.envMNC)
			var id netIdentity
			fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
			opts := []netIDOption{withDNN()}
			if !tc.required {
				opts = append(opts, withInherit())
			}
			addNetIdentityFlags(fs, &id, opts...)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := id.resolve(tc.required)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolve = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if id.MCC != tc.wantMCC || id.MNC != tc.wantMNC {
				t.Errorf("plmn = %s/%s, want %s/%s", id.MCC, id.MNC, tc.wantMCC, tc.wantMNC)
			}
		})
	}
}

func TestNetIdentityDefaults(t *testing.T) {
	var id netIdentity
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	addNetIdentityFlags(fs, &id, withDNN())
	if id.TAC != 1 || id.SST != 1 || id.SD != "" || id.DNN != "internet" {
		t.Errorf("defaults = tac %d sst %d sd %q dnn %q, want 1 1 \"\" internet", id.TAC, id.SST, id.SD, id.DNN)
	}
	var inherit netIdentity
	fs2 := pflag.NewFlagSet("t", pflag.ContinueOnError)
	addNetIdentityFlags(fs2, &inherit, withInherit())
	if inherit.TAC != 0 || inherit.SST != 0 || inherit.SD != "" {
		t.Errorf("inherit defaults = tac %d sst %d sd %q, want all unset", inherit.TAC, inherit.SST, inherit.SD)
	}
}

func TestNetIdentityString(t *testing.T) {
	tests := []struct {
		name string
		args []string
		opts []netIDOption
		want string
	}{
		{name: "full with dnn", args: []string{"--mcc", "001", "--mnc", "01", "--sd", "010203"}, opts: []netIDOption{withDNN()}, want: "plmn=001/01 tac=1 snssai=1/010203 dnn=internet"},
		{name: "no sd, no dnn", args: []string{"--mcc", "001", "--mnc", "01"}, want: "plmn=001/01 tac=1 snssai=1"},
		{name: "inherit everything", opts: []netIDOption{withInherit()}, want: "plmn=(inherited) tac=(inherited) snssai=(inherited)"},
		{name: "inherit plmn, explicit slice", args: []string{"--sst", "2", "--sd", "abcdef"}, opts: []netIDOption{withInherit()}, want: "plmn=(inherited) tac=(inherited) snssai=2/abcdef"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var id netIdentity
			fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
			addNetIdentityFlags(fs, &id, tc.opts...)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := id.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every command that dials a core (takes --amf) exposes the same five identity
// flags with identical help, none required by cobra (the env fallback and
// the handover inherit rule both need the flag to be optional), and the same
// defaults — except the handover commands, which deliberately default nothing
// so unset fields inherit from the serving gNB.
func TestEveryAMFCommandHasIdentityFlags(t *testing.T) {
	want := map[string]struct{ help, def string }{
		"mcc": {mccFlagHelp, ""},
		"mnc": {mncFlagHelp, ""},
		"tac": {tacFlagHelp, "1"},
		"sst": {sstFlagHelp, "1"},
		"sd":  {sdFlagHelp, ""},
	}
	inherits := map[string]bool{"orbit ue handover": true, "orbit ue xn-handover": true}
	var seen []string
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Flags().Lookup("amf") != nil {
			seen = append(seen, path)
			for name, w := range want {
				f := c.Flags().Lookup(name)
				if f == nil {
					t.Errorf("%s: missing --%s", path, name)
					continue
				}
				if f.Usage != w.help {
					t.Errorf("%s: --%s help %q differs from the shared text", path, name, f.Usage)
				}
				def := w.def
				if inherits[path] && (name == "tac" || name == "sst") {
					def = "0"
				}
				if f.DefValue != def {
					t.Errorf("%s: --%s default %q, want %q", path, name, f.DefValue, def)
				}
				if _, req := f.Annotations[cobra.BashCompOneRequiredFlag]; req {
					t.Errorf("%s: --%s is cobra-required, which defeats the env fallback", path, name)
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(New("test"), "orbit")
	for _, w := range []string{"orbit cell ngsetup", "orbit ue register", "orbit ue handover", "orbit ue xn-handover", "orbit load", "orbit runs start-load", "orbit conformance"} {
		if !strings.Contains(strings.Join(seen, "\n"), w) {
			t.Errorf("%q not among the --amf commands: %v", w, seen)
		}
	}
}

// stubUEHandover captures the HandoverRequest the CLI sends.
type stubUEHandover struct {
	orbitv1connect.UnimplementedUEServiceHandler
	req *orbitv1.HandoverRequest
}

func (s *stubUEHandover) Handover(_ context.Context, req *connect.Request[orbitv1.HandoverRequest]) (*connect.Response[orbitv1.HandoverResponse], error) {
	s.req = req.Msg
	return connect.NewResponse(&orbitv1.HandoverResponse{Supi: req.Msg.GetSupi(), GnbId: req.Msg.GetTargetGnb().GetId(), State: "HANDED_OVER"}), nil
}

func (s *stubUEHandover) XnHandover(ctx context.Context, req *connect.Request[orbitv1.HandoverRequest]) (*connect.Response[orbitv1.HandoverResponse], error) {
	return s.Handover(ctx, req)
}

// `ue handover` with no identity flags sends an empty PLMN, zero TAC and no
// slices (the server inherits); explicit fields are sent as given, and the
// PLMN env fallback is NOT applied to handover since it would silently
// override the inherit rule.
func TestUEHandoverSendsIdentityForInherit(t *testing.T) {
	tests := []struct {
		name          string
		cmd           string
		flags         []string
		wantMCC       string
		wantTAC       uint32
		wantSliceSent bool
		wantSST       uint32
		wantSD        string
	}{
		{name: "n2 inherits everything", cmd: "handover"},
		{name: "xn inherits everything", cmd: "xn-handover"},
		{name: "explicit plmn", cmd: "handover", flags: []string{"--mcc", "001", "--mnc", "01"}, wantMCC: "001"},
		{name: "explicit tac only", cmd: "handover", flags: []string{"--tac", "7"}, wantTAC: 7},
		{name: "explicit slice", cmd: "xn-handover", flags: []string{"--sst", "2", "--sd", "abcdef"}, wantSliceSent: true, wantSST: 2, wantSD: "abcdef"},
		{name: "explicit sd alone", cmd: "handover", flags: []string{"--sd", "010203"}, wantSliceSent: true, wantSST: 0, wantSD: "010203"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_MCC", "")
			t.Setenv("ORBIT_MNC", "")
			stub := &stubUEHandover{}
			mux := http.NewServeMux()
			mux.Handle(orbitv1connect.NewUEServiceHandler(stub))
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			args := append([]string{"--server", srv.URL, "ue", tc.cmd, "--supi", "001010000000001", "--amf", "198.51.100.1:38412", "--gnb-id", "2"}, tc.flags...)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			g := stub.req.GetTargetGnb()
			if g.GetMcc() != tc.wantMCC {
				t.Errorf("mcc sent = %q, want %q", g.GetMcc(), tc.wantMCC)
			}
			if g.GetTac() != tc.wantTAC {
				t.Errorf("tac sent = %d, want %d", g.GetTac(), tc.wantTAC)
			}
			if tc.wantSliceSent {
				if len(g.GetSlices()) != 1 || g.GetSlices()[0].GetSst() != tc.wantSST || g.GetSlices()[0].GetSd() != tc.wantSD {
					t.Errorf("slices sent = %v, want one %d/%s", g.GetSlices(), tc.wantSST, tc.wantSD)
				}
			} else if len(g.GetSlices()) != 0 {
				t.Errorf("slices sent = %v, want none (inherit)", g.GetSlices())
			}
			if !strings.Contains(out.String(), "network: ") {
				t.Errorf("no network line in output:\n%s", out.String())
			}
		})
	}
}

// With a PLMN exported, handover still inherits rather than taking the env:
// the env fallback is for commands that REQUIRE a PLMN.
func TestUEHandoverEnvFallbackStillApplies(t *testing.T) {
	// Design: resolve applies the env fallback uniformly, so an exported PLMN
	// IS sent on handover. Pin the behaviour so a change to it is deliberate.
	t.Setenv("ORBIT_MCC", "001")
	t.Setenv("ORBIT_MNC", "01")
	stub := &stubUEHandover{}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	root := New("test")
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"--server", srv.URL, "ue", "handover", "--supi", "001010000000001", "--amf", "198.51.100.1:38412", "--gnb-id", "2"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if g := stub.req.GetTargetGnb(); g.GetMcc() != "001" || g.GetMnc() != "01" {
		t.Errorf("env PLMN not applied on handover: %s/%s", g.GetMcc(), g.GetMnc())
	}
}

// `ue register` and `cell ngsetup` require a PLMN (flags or env) and print the
// identity they used; `conformance` passes --tac/--sst/--sd through to the
// suite's gNB config (visible in the JSON summary) instead of hardcoding.
func TestIdentityRequiredAndReported(t *testing.T) {
	t.Setenv("ORBIT_MCC", "")
	t.Setenv("ORBIT_MNC", "")
	stub := &stubCellService{resp: &orbitv1.RunNGSetupResponse{Accepted: true, AmfName: "amf"}}
	url := startStubCellServer(t, stub)

	root := New("test")
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"--server", url, "cell", "ngsetup", "--amf", "198.51.100.1:38412"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "$ORBIT_MCC/$ORBIT_MNC") {
		t.Fatalf("ngsetup without a PLMN: err = %v, want the two-source message", err)
	}
	if stub.req != nil {
		t.Fatal("a request was sent without a PLMN")
	}

	t.Setenv("ORBIT_MCC", "001")
	t.Setenv("ORBIT_MNC", "01")
	root = New("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--server", url, "cell", "ngsetup", "--amf", "198.51.100.1:38412", "--sd", "010203"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if g := stub.req.GetGnb(); g.GetMcc() != "001" || g.GetMnc() != "01" || g.GetTac() != 1 || g.GetSlices()[0].GetSd() != "010203" {
		t.Errorf("ngsetup sent %v", g)
	}
	if !strings.Contains(out.String(), "network: plmn=001/01 tac=1 snssai=1/010203") {
		t.Errorf("ngsetup output lacks the network line:\n%s", out.String())
	}
}

func TestConformancePassesIdentityThrough(t *testing.T) {
	root := New("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"conformance", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01", "--tac", "7", "--sst", "2", "--sd", "abcdef",
		"--json", "--per-test-timeout", "1s", "--category", "procedural"})
	_ = root.Execute() // the AMF is unroutable, so the gate fails; the summary is still printed
	var sum struct {
		Network *struct {
			MCC, MNC string
			TAC, SST uint32
			SD       string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil {
		t.Fatalf("summary JSON: %v\n%s", err, out.String())
	}
	if sum.Network == nil || sum.Network.MCC != "001" || sum.Network.MNC != "01" || sum.Network.TAC != 7 || sum.Network.SST != 2 || sum.Network.SD != "abcdef" {
		t.Errorf("Network in summary = %+v, want the flags passed through", sum.Network)
	}
}

func TestConformanceRequiresPLMN(t *testing.T) {
	t.Setenv("ORBIT_MCC", "")
	t.Setenv("ORBIT_MNC", "")
	root := New("test")
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"conformance", "--amf", "127.0.0.1:1", "--per-test-timeout", "1s"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "$ORBIT_MCC/$ORBIT_MNC") {
		t.Fatalf("err = %v, want the PLMN-required message", err)
	}
}
