package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
)

// --timeout and --hold travel as timeout_ms and hold_ms; the defaults keep
// today's behaviour (10 s budget, no hold); a zero or negative timeout is
// refused before any request; `held:` is printed only when a hold was asked.
func TestCellNGSetupTimeoutAndHoldFlags(t *testing.T) {
	tests := []struct {
		name          string
		flags         []string
		heldMs        uint32
		wantTimeoutMs uint32
		wantHoldMs    uint32
		wantHeld      string // "" = the held line must be absent
		wantErr       string
	}{
		{name: "defaults", wantTimeoutMs: 10000, wantHoldMs: 0},
		{name: "timeout and hold", flags: []string{"--timeout", "3s", "--hold", "2s"}, heldMs: 1500, wantTimeoutMs: 3000, wantHoldMs: 2000, wantHeld: "held: 1.5s"},
		{name: "hold alone", flags: []string{"--hold", "250ms"}, heldMs: 250, wantTimeoutMs: 10000, wantHoldMs: 250, wantHeld: "held: 250ms"},
		{name: "zero timeout refused", flags: []string{"--timeout", "0"}, wantErr: "--timeout must be greater than 0"},
		{name: "negative timeout refused", flags: []string{"--timeout", "-1s"}, wantErr: "--timeout must be greater than 0"},
		{name: "negative hold refused", flags: []string{"--hold", "-1s"}, wantErr: "--hold must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubCellService{resp: &orbitv1.RunNGSetupResponse{Accepted: true, AmfName: "amf", LocalAddress: "10.0.0.21:40001", HeldMs: tc.heldMs}}
			url := startStubCellServer(t, stub)
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(append([]string{"--server", url, "cell", "ngsetup", "--amf", "198.51.100.1:38412", "--mcc", "208", "--mnc", "93"}, tc.flags...))
			err := root.Execute()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				if stub.req != nil {
					t.Error("a request was sent despite the rejected flag")
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if stub.req == nil {
				t.Fatal("server saw no request")
			}
			if stub.req.GetTimeoutMs() != tc.wantTimeoutMs || stub.req.GetHoldMs() != tc.wantHoldMs {
				t.Errorf("sent timeout_ms=%d hold_ms=%d, want %d/%d", stub.req.GetTimeoutMs(), stub.req.GetHoldMs(), tc.wantTimeoutMs, tc.wantHoldMs)
			}
			if tc.wantHeld == "" && strings.Contains(out.String(), "held:") {
				t.Errorf("held line printed without a hold:\n%s", out.String())
			}
			if tc.wantHeld != "" && !strings.Contains(out.String(), tc.wantHeld) {
				t.Errorf("output lacks %q:\n%s", tc.wantHeld, out.String())
			}
		})
	}
}

// Every command that opens an N2 association either exposes its dial bound
// as a flag (--timeout, --per-test-timeout) or is one of the commands that
// inherit sctp.DefaultDialTimeout; a new N2-opening command must land in one
// of the two so USAGE's per-command list stays complete.
func TestEveryAMFCommandDocumentsItsDialBound(t *testing.T) {
	inherits := map[string]bool{
		"orbit ue register":     true,
		"orbit ue handover":     true,
		"orbit ue xn-handover":  true,
		"orbit load":            true,
		"orbit runs start-load": true,
	}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Flags().Lookup("amf") != nil {
			hasFlag := c.Flags().Lookup("timeout") != nil || c.Flags().Lookup("per-test-timeout") != nil
			if !hasFlag && !inherits[path] {
				t.Errorf("%s: opens an N2 association but neither exposes a timeout flag nor is listed as inheriting DefaultDialTimeout", path)
			}
			if hasFlag && inherits[path] {
				t.Errorf("%s: listed as inheriting the default but exposes a timeout flag", path)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(New("test"), "orbit")
}
