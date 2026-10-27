package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		{name: "optional ignores the env and inherits", envMCC: "208", envMNC: "93", required: false, wantMCC: "", wantMNC: ""},
		{name: "optional with explicit flags keeps them", args: []string{"--mcc", "001", "--mnc", "01"}, envMCC: "208", envMNC: "93", required: false, wantMCC: "001", wantMNC: "01"},
		{name: "only mcc, optional", args: []string{"--mcc", "001"}, required: false, wantErr: "neither to inherit"},
		{name: "only mnc from env, required", envMNC: "01", required: true, wantErr: "$ORBIT_MCC/$ORBIT_MNC"},
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
				help := w.help
				if inherits[path] {
					// Handover never reads the env, so its PLMN help must not
					// promise it; it says to omit the flag to inherit instead.
					switch name {
					case "mcc":
						help = mccInheritHelp
					case "mnc":
						help = mncInheritHelp
					}
					if (name == "mcc" || name == "mnc") && strings.Contains(f.Usage, "ORBIT_M") {
						t.Errorf("%s: --%s help %q mentions the env, which handover ignores", path, name, f.Usage)
					}
				}
				if f.Usage != help {
					t.Errorf("%s: --%s help %q differs from the expected text %q", path, name, f.Usage, help)
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

// Handover answers as the server does: the target it reports is the
// request's, with unset fields filled from a serving gNB of 208/93, TAC 1
// and slices 1/010203 and 2.
func (s *stubUEHandover) Handover(_ context.Context, req *connect.Request[orbitv1.HandoverRequest]) (*connect.Response[orbitv1.HandoverResponse], error) {
	s.req = req.Msg
	t := &orbitv1.GnbConfig{Id: req.Msg.GetTargetGnb().GetId(), Mcc: req.Msg.GetTargetGnb().GetMcc(), Mnc: req.Msg.GetTargetGnb().GetMnc(),
		Tac: req.Msg.GetTargetGnb().GetTac(), Slices: req.Msg.GetTargetGnb().GetSlices()}
	if t.Mcc == "" && t.Mnc == "" {
		t.Mcc, t.Mnc = "208", "93"
	}
	if t.Tac == 0 {
		t.Tac = 1
	}
	if len(t.Slices) == 0 {
		t.Slices = []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}, {Sst: 2}}
	}
	return connect.NewResponse(&orbitv1.HandoverResponse{Supi: req.Msg.GetSupi(), GnbId: t.Id, State: "HANDED_OVER", Target: t}), nil
}

func (s *stubUEHandover) XnHandover(ctx context.Context, req *connect.Request[orbitv1.HandoverRequest]) (*connect.Response[orbitv1.HandoverResponse], error) {
	return s.Handover(ctx, req)
}

// `ue handover` with no identity flags sends an empty PLMN, zero TAC and no
// slices (the server inherits); explicit fields are sent as given.
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
		wantNetwork   string // the printed line, from the server's reported target
	}{
		{name: "n2 inherits everything", cmd: "handover", wantNetwork: "network: plmn=208/93 tac=1 snssai=1/010203,2"},
		{name: "xn inherits everything", cmd: "xn-handover", wantNetwork: "network: plmn=208/93 tac=1 snssai=1/010203,2"},
		{name: "explicit plmn", cmd: "handover", flags: []string{"--mcc", "001", "--mnc", "01"}, wantMCC: "001",
			wantNetwork: "network: plmn=001/01 tac=1 snssai=1/010203,2"},
		{name: "explicit tac only", cmd: "handover", flags: []string{"--tac", "7"}, wantTAC: 7, wantNetwork: "network: plmn=208/93 tac=7 snssai=1/010203,2"},
		{name: "explicit slice", cmd: "xn-handover", flags: []string{"--sst", "2", "--sd", "abcdef"}, wantSliceSent: true, wantSST: 2, wantSD: "abcdef",
			wantNetwork: "network: plmn=208/93 tac=1 snssai=2/abcdef"},
		{name: "explicit sst alone", cmd: "handover", flags: []string{"--sst", "2"}, wantSliceSent: true, wantSST: 2, wantSD: "",
			wantNetwork: "network: plmn=208/93 tac=1 snssai=2"},
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
			if !strings.Contains(out.String(), tc.wantNetwork+"\n") {
				t.Errorf("output lacks %q:\n%s", tc.wantNetwork, out.String())
			}
		})
	}
}

// On handover the slice is given as a pair or not at all: --sd without --sst
// would send SST 0, the not-given sentinel, as a value.
func TestUEHandoverSDNeedsSST(t *testing.T) {
	stub := &stubUEHandover{}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	root := New("test")
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--server", srv.URL, "ue", "handover", "--supi", "001010000000001", "--amf", "198.51.100.1:38412", "--gnb-id", "2", "--sd", "010203"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--sd needs --sst") {
		t.Fatalf("err = %v, want the sd-needs-sst error", err)
	}
	if stub.req != nil {
		t.Error("a request was sent with an SST-less slice")
	}
}

// With a PLMN exported, handover still inherits rather than taking the env:
// a handover target is in the serving PLMN by definition, so the UE's
// session is the source of truth and $ORBIT_MCC/$ORBIT_MNC are a convenience
// for new attaches only.
func TestUEHandoverIgnoresEnvPLMN(t *testing.T) {
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
	if g := stub.req.GetTargetGnb(); g.GetMcc() != "" || g.GetMnc() != "" {
		t.Errorf("handover sent the env PLMN %s/%s; want empty so the server inherits", g.GetMcc(), g.GetMnc())
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
	root.SetArgs([]string{"conformance", "--local", "--amf", "127.0.0.1:1", "--mcc", "001", "--mnc", "01", "--tac", "7", "--sst", "2", "--sd", "abcdef",
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
	root.SetArgs([]string{"conformance", "--local", "--amf", "127.0.0.1:1", "--per-test-timeout", "1s"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "$ORBIT_MCC/$ORBIT_MNC") {
		t.Fatalf("err = %v, want the PLMN-required message", err)
	}
}

// presentedNetwork prints the reported target with every slice, and a
// placeholder when the server reports none.
func TestPresentedNetwork(t *testing.T) {
	tests := []struct {
		name string
		in   *orbitv1.GnbConfig
		want string
	}{
		{name: "not reported", want: "(not reported by the server)"},
		{name: "one slice", in: &orbitv1.GnbConfig{Mcc: "001", Mnc: "01", Tac: 1, Slices: []*orbitv1.Snssai{{Sst: 1, Sd: "010203"}}},
			want: "plmn=001/01 tac=1 snssai=1/010203"},
		{name: "two slices", in: &orbitv1.GnbConfig{Mcc: "208", Mnc: "093", Tac: 7, Slices: []*orbitv1.Snssai{{Sst: 1}, {Sst: 2, Sd: "abcdef"}}},
			want: "plmn=208/093 tac=7 snssai=1,2/abcdef"},
	}
	for _, tc := range tests {
		if got := presentedNetwork(tc.in); got != tc.want {
			t.Errorf("%s: presentedNetwork = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// No "(inherited)" placeholder is left in the code: a handover prints what
// the server reports it presented (#108).
func TestNoInheritedPlaceholder(t *testing.T) {
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || strings.HasPrefix(d.Name(), ".wt-")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "(inherited)") {
			t.Errorf("%s still prints the (inherited) placeholder", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
