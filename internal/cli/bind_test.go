package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/load"
)

// Every command that opens an N2 association (it takes --amf) must take
// --bind with the one standard help text, so the N2 source is chosen the same
// way everywhere and the N2/N3 pairing reads the same on every --help.
func TestEveryAMFCommandHasStandardBindFlag(t *testing.T) {
	var seen []string
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Flags().Lookup("amf") != nil {
			seen = append(seen, path)
			bind := c.Flags().Lookup("bind")
			if bind == nil {
				t.Errorf("%s: has --amf but no --bind", path)
			} else if !strings.HasPrefix(bind.Usage, bindFlagHelp) {
				t.Errorf("%s: --bind help %q does not start with the standard text", path, bind.Usage)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(New("test"), "orbit")

	// The set this invariant covers; a new N2-opening command lands here.
	want := []string{
		"orbit cell ngsetup",
		"orbit ue register",
		"orbit ue handover",
		"orbit ue xn-handover",
		"orbit runs start-load",
		"orbit load",
		"orbit conformance",
	}
	got := strings.Join(seen, "\n")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("expected %q among the commands taking --amf; got:\n%s", w, got)
		}
	}
}

// stubCellService captures the RunNGSetup request the CLI sends.
type stubCellService struct {
	orbitv1connect.UnimplementedCellServiceHandler
	req  *orbitv1.RunNGSetupRequest
	resp *orbitv1.RunNGSetupResponse
}

func (s *stubCellService) RunNGSetup(_ context.Context, req *connect.Request[orbitv1.RunNGSetupRequest]) (*connect.Response[orbitv1.RunNGSetupResponse], error) {
	s.req = req.Msg
	return connect.NewResponse(s.resp), nil
}

func startStubCellServer(t *testing.T, s *stubCellService) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewCellServiceHandler(s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// `cell ngsetup` takes --bind, keeps the deprecated --local working, and lets
// --bind win when both are given.
func TestCellNGSetupBindPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  string
	}{
		{name: "neither: route-selected default", want: ""},
		{name: "bind alone", flags: []string{"--bind", "10.0.0.21"}, want: "10.0.0.21"},
		{name: "deprecated local alone still works", flags: []string{"--local", "10.0.0.22:0"}, want: "10.0.0.22:0"},
		{name: "both: bind wins", flags: []string{"--local", "10.0.0.22:0", "--bind", "10.0.0.21"}, want: "10.0.0.21"},
		{name: "explicit empty bind beats local", flags: []string{"--local", "10.0.0.22:0", "--bind", ""}, want: ""},
		{name: "explicit empty bind via equals beats local", flags: []string{"--local", "10.0.0.22:0", "--bind="}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubCellService{resp: &orbitv1.RunNGSetupResponse{Accepted: true, AmfName: "amf", LocalAddress: "10.0.0.21:40001"}}
			url := startStubCellServer(t, stub)
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			args := append([]string{"--server", url, "cell", "ngsetup", "--amf", "198.51.100.1:38412", "--mcc", "208", "--mnc", "93"}, tc.flags...)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if stub.req == nil {
				t.Fatal("server saw no request")
			}
			if got := stub.req.GetLocalAddress(); got != tc.want {
				t.Errorf("local_address sent = %q, want %q", got, tc.want)
			}
			if !strings.Contains(out.String(), "n2-source=10.0.0.21:40001") {
				t.Errorf("output does not report the N2 source the server used:\n%s", out.String())
			}
		})
	}
}

func TestNGSetupLocalAddress(t *testing.T) {
	tests := []struct {
		bindSet     bool
		bind, local string
		want        string
	}{
		{false, "", "", ""},
		{true, "a", "", "a"},
		{false, "", "b", "b"},
		{true, "a", "b", "a"},
		{true, "", "b", ""}, // an explicit --bind "" means the route-selected source, over --local
	}
	for _, tc := range tests {
		if got := ngsetupLocalAddress(tc.bindSet, tc.bind, tc.local); got != tc.want {
			t.Errorf("ngsetupLocalAddress(%v, %q, %q) = %q, want %q", tc.bindSet, tc.bind, tc.local, got, tc.want)
		}
	}
}

// The deprecated --local flag is marked so cobra warns on use and hides it
// from --help.
func TestCellNGSetupLocalIsDeprecated(t *testing.T) {
	cmd := findCommand(t, New("test"), "cell", "ngsetup")
	f := cmd.Flags().Lookup("local")
	if f == nil {
		t.Fatal("--local removed; it must stay as a deprecated alias")
	}
	if f.Deprecated == "" {
		t.Error("--local is not marked deprecated")
	}
}

// `orbit load --bind` is validated against --gnb-count before anything is
// dialled: a count that is neither 0, 1 nor the gNB count is refused with
// both numbers in the message.
func TestLoadBindCountValidation(t *testing.T) {
	tests := []struct {
		name     string
		gnbCount string
		binds    []string
		wantErr  string
	}{
		{name: "two binds for three gNBs", gnbCount: "3", binds: []string{"--bind", "10.0.0.1,10.0.0.2"}, wantErr: "2 bind addresses for 3 gNBs"},
		{name: "four binds for three gNBs", gnbCount: "3", binds: []string{"--bind", "10.0.0.1", "--bind", "10.0.0.2", "--bind", "10.0.0.3", "--bind", "10.0.0.4"}, wantErr: "4 bind addresses for 3 gNBs"},
		{name: "two binds for one gNB", gnbCount: "1", binds: []string{"--bind", "10.0.0.1,10.0.0.2"}, wantErr: "2 bind addresses for 1 gNBs"},
		{name: "shared fixed port", gnbCount: "2", binds: []string{"--bind", "10.0.0.1:38412"}, wantErr: `shared bind address "10.0.0.1:38412" has a fixed port`},
		{name: "duplicate fixed port", gnbCount: "2", binds: []string{"--bind", "10.0.0.1:38412,10.0.0.1:38412"}, wantErr: `both "10.0.0.1:38412"`},
		{name: "malformed", gnbCount: "1", binds: []string{"--bind", "10.0.0.1:0:0"}, wantErr: `invalid local address "10.0.0.1:0:0"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			args := append([]string{"load", "--local", "--amf", "127.0.0.1:1", "--base-imsi", "208930100000000",
				"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f",
				"--mcc", "208", "--mnc", "93", "--gnb-count", tc.gnbCount, "--progress-every", "0"}, tc.binds...)
			root.SetArgs(args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "--bind:") {
				t.Errorf("error %q does not name the flag", err)
			}
		})
	}
}

func TestSharedBindNote(t *testing.T) {
	tests := []struct {
		name     string
		binds    []string
		gnbCount int
		want     string
	}{
		{name: "no binds one gNB says nothing", binds: nil, gnbCount: 1},
		{name: "no binds many gNBs share the route-selected source", binds: nil, gnbCount: 4, want: "4 gNBs share one N2 source (the route-selected address)"},
		{name: "one bind one gNB says nothing", binds: []string{"10.0.0.1"}, gnbCount: 1},
		{name: "one bind many gNBs warns", binds: []string{"10.0.0.1"}, gnbCount: 4, want: "4 gNBs share one N2 source (10.0.0.1)"},
		{name: "one per gNB says nothing", binds: []string{"a", "b"}, gnbCount: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sharedBindNote(tc.binds, tc.gnbCount)
			if tc.want == "" && got != "" {
				t.Fatalf("got note %q, want none", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("note %q does not contain %q", got, tc.want)
			}
		})
	}
}

// `runs start-load --bind` travels to the server as the plan's N2 source
// pool, in order; the server applies the count rule.
func TestRunsStartLoadSendsBindAddresses(t *testing.T) {
	stub := &stubRunService{startRunID: "run-1"}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewRunServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	root := New("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--server", srv.URL, "runs", "start-load", "--amf", "198.51.100.1:38412",
		"--base-imsi", "208930100000000", "--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f",
		"--mcc", "208", "--mnc", "93", "--gnb-count", "2", "--bind", "10.0.0.21,10.0.0.22:0"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := stub.startReq.GetPlan().GetTarget().GetRan().GetN2SourceIps()
	if strings.Join(got, ",") != "10.0.0.21,10.0.0.22:0" {
		t.Errorf("bind_addresses = %q", got)
	}
}

// An n2-source line is the single ip:port the server reported, bound or not:
// unbound dials bind the route-selected address, so there is no several-
// address case left to annotate. The literals below are the old note's words.
func TestN2SourceLinePrintedAsIs(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
	}{
		{name: "unbound"},
		{name: "bound", flags: []string{"--bind", "10.0.0.21"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubCellService{resp: &orbitv1.RunNGSetupResponse{Accepted: true, AmfName: "amf", LocalAddress: "10.0.0.21:40001"}}
			url := startStubCellServer(t, stub)
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(append([]string{"--server", url, "cell", "ngsetup", "--amf", "198.51.100.1:38412", "--mcc", "208", "--mnc", "93"}, tc.flags...))
			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !strings.Contains(out.String(), "  n2-source=10.0.0.21:40001\n") {
				t.Errorf("output lacks the plain n2-source line:\n%s", out.String())
			}
			if strings.Contains(out.String(), "unbound") || strings.Contains(out.String(), "multi-homed") {
				t.Errorf("output annotates the source:\n%s", out.String())
			}
		})
	}
}

// The load report prints its N2 sources exactly as reported, one ip:port per
// gNB, with no annotation.
func TestPrintLoadReportSourcesAsIs(t *testing.T) {
	rep := load.Report{N2Sources: []string{"10.0.0.21:40001", "10.0.0.21:40002"}}
	var out bytes.Buffer
	printLoadReport(&out, rep)
	if !strings.Contains(out.String(), "  n2-source: 10.0.0.21:40001 10.0.0.21:40002\n") {
		t.Errorf("load header does not list the sources as reported:\n%s", out.String())
	}
	if strings.Contains(out.String(), "unbound") || strings.Contains(out.String(), "multi-homed") {
		t.Errorf("load header annotates the sources:\n%s", out.String())
	}
}

// `load` with no --bind and several gNBs says on stderr that they share one
// N2 source: every unbound gNB dials from the same route-selected address.
// The run itself then fails on the refused loopback AMF, which is fine; the
// note comes first.
func TestLoadUnboundSharedSourceNote(t *testing.T) {
	tests := []struct {
		name     string
		gnbCount string
		wantNote bool
	}{
		{name: "two gNBs", gnbCount: "2", wantNote: true},
		{name: "one gNB", gnbCount: "1", wantNote: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := New("test")
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetArgs([]string{"load", "--local", "--amf", "127.0.0.1:1", "--base-imsi", "208930100000000",
				"--ki", "00112233445566778899aabbccddeeff", "--opc", "000102030405060708090a0b0c0d0e0f",
				"--mcc", "208", "--mnc", "93", "--gnb-count", tc.gnbCount, "--count", "1", "--progress-every", "0"})
			if err := root.Execute(); err == nil {
				t.Fatal("load against a refused loopback AMF succeeded")
			}
			want := "note: " + tc.gnbCount + " gNBs share one N2 source (the route-selected address)"
			if got := strings.Contains(errOut.String(), want); got != tc.wantNote {
				t.Errorf("note present = %v, want %v; stderr:\n%s", got, tc.wantNote, errOut.String())
			}
		})
	}
}

// `orbit conformance --bind` refuses a fixed port before dialling anything:
// each check opens a fresh association and the liveness probe a second one,
// so only an ephemeral port works.
func TestConformanceBindRejectsFixedPort(t *testing.T) {
	tests := []struct {
		name, bind, wantErr string
	}{
		{name: "fixed port", bind: "10.0.0.1:38412", wantErr: "fixes a port"},
		{name: "malformed", bind: "10.0.0.1:0:0", wantErr: `invalid local address "10.0.0.1:0:0"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"conformance", "--local", "--amf", "127.0.0.1:1", "--bind", tc.bind, "--per-test-timeout", "1s"})
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.HasPrefix(err.Error(), "--bind:") {
				t.Fatalf("err = %v, want --bind: ... %q", err, tc.wantErr)
			}
		})
	}
}
