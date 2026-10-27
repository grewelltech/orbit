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
)

const (
	goodKi  = "00112233445566778899aabbccddeeff"
	goodOPc = "000102030405060708090a0b0c0d0e0f"
	envKi   = "ffeeddccbbaa99887766554433221100"
	envOPc  = "0f0e0d0c0b0a09080706050403020100"
)

func TestResolveCreds(t *testing.T) {
	tests := []struct {
		name            string
		flagKi, flagOPc string
		envKi, envOPc   string
		wantKi, wantOPc string
		wantErr         string
	}{
		{name: "flags only", flagKi: goodKi, flagOPc: goodOPc, wantKi: goodKi, wantOPc: goodOPc},
		{name: "env only", envKi: envKi, envOPc: envOPc, wantKi: envKi, wantOPc: envOPc},
		{name: "both: flags win", flagKi: goodKi, flagOPc: goodOPc, envKi: envKi, envOPc: envOPc, wantKi: goodKi, wantOPc: goodOPc},
		{name: "mixed: flag ki, env opc", flagKi: goodKi, envOPc: envOPc, wantKi: goodKi, wantOPc: envOPc},
		{name: "neither", wantErr: "pass --ki/--opc or set $ORBIT_KI/$ORBIT_OPC"},
		{name: "ki only, no opc anywhere", flagKi: goodKi, wantErr: "pass --ki/--opc or set $ORBIT_KI/$ORBIT_OPC"},
		{name: "env opc only, no ki anywhere", envOPc: envOPc, wantErr: "pass --ki/--opc or set $ORBIT_KI/$ORBIT_OPC"},
		{name: "malformed flag ki", flagKi: "nothex", flagOPc: goodOPc, wantErr: "Ki"},
		{name: "malformed flag opc", flagKi: goodKi, flagOPc: "0011", wantErr: "OPc"},
		{name: "malformed env ki", envKi: "zz", envOPc: envOPc, wantErr: "Ki"},
		{name: "malformed env opc", envKi: envKi, envOPc: "zz", wantErr: "OPc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_KI", tc.envKi)
			t.Setenv("ORBIT_OPC", tc.envOPc)
			ki, opc, err := resolveCreds(tc.flagKi, tc.flagOPc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				// Never echo a key value: a malformed key is named, not quoted.
				for _, secret := range []string{tc.flagKi, tc.flagOPc, tc.envKi, tc.envOPc} {
					if len(secret) >= 32 && strings.Contains(err.Error(), secret) {
						t.Errorf("error %q echoes a key value", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCreds: %v", err)
			}
			if ki != tc.wantKi || opc != tc.wantOPc {
				t.Errorf("resolveCreds = (%s, %s), want (%s, %s)", ki, opc, tc.wantKi, tc.wantOPc)
			}
		})
	}
}

// Every command that takes --ki also takes --opc, neither is required (the
// environment may supply them), and both help strings name their env var, so
// credentials are supplied the same way on every command.
func TestEveryKiCommandUsesEnvFallback(t *testing.T) {
	var seen []string
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if ki := c.Flags().Lookup("ki"); ki != nil {
			seen = append(seen, path)
			opc := c.Flags().Lookup("opc")
			if opc == nil {
				t.Errorf("%s: has --ki but no --opc", path)
				return
			}
			for _, f := range []*struct {
				name, env string
				help      string
				ann       map[string][]string
			}{
				{"--ki", "$ORBIT_KI", ki.Usage, ki.Annotations},
				{"--opc", "$ORBIT_OPC", opc.Usage, opc.Annotations},
			} {
				if !strings.Contains(f.help, f.env) {
					t.Errorf("%s: %s help %q does not mention %s", path, f.name, f.help, f.env)
				}
				if _, required := f.ann[cobra.BashCompOneRequiredFlag]; required {
					t.Errorf("%s: %s is marked required, so the env fallback can never apply", path, f.name)
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(New("test"), "orbit")

	want := []string{"orbit ue register", "orbit load", "orbit runs start-load", "orbit runs start-fleet"}
	got := strings.Join(seen, "\n")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("expected %q among the commands taking --ki; got:\n%s", w, got)
		}
	}
}

// stubUERegister captures the RegisterRequest the CLI sends.
type stubUERegister struct {
	orbitv1connect.UnimplementedUEServiceHandler
	req *orbitv1.RegisterRequest
}

func (s *stubUERegister) Register(_ context.Context, req *connect.Request[orbitv1.RegisterRequest]) (*connect.Response[orbitv1.RegisterResponse], error) {
	s.req = req.Msg
	return connect.NewResponse(&orbitv1.RegisterResponse{Supi: req.Msg.GetSupi(), Registered: true}), nil
}

// `ue register` with no key flags takes Ki/OPc from the environment and sends
// them; explicit flags win; with neither it fails before any request is made.
func TestUERegisterCredsFromEnv(t *testing.T) {
	tests := []struct {
		name            string
		flags           []string
		envKi, envOPc   string
		wantKi, wantOPc string
		wantErr         string
	}{
		{name: "env only", envKi: envKi, envOPc: envOPc, wantKi: envKi, wantOPc: envOPc},
		{name: "flags win", flags: []string{"--ki", goodKi, "--opc", goodOPc}, envKi: envKi, envOPc: envOPc, wantKi: goodKi, wantOPc: goodOPc},
		{name: "neither", wantErr: "pass --ki/--opc or set $ORBIT_KI/$ORBIT_OPC"},
		{name: "malformed env", envKi: "bad", envOPc: envOPc, wantErr: "Ki"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_KI", tc.envKi)
			t.Setenv("ORBIT_OPC", tc.envOPc)
			stub := &stubUERegister{}
			mux := http.NewServeMux()
			mux.Handle(orbitv1connect.NewUEServiceHandler(stub))
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			args := append([]string{"--server", srv.URL, "ue", "register", "--amf", "198.51.100.1:38412",
				"--supi", "208930100007500", "--mcc", "208", "--mnc", "93"}, tc.flags...)
			root.SetArgs(args)
			err := root.Execute()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				if stub.req != nil {
					t.Error("a request was sent despite unresolved credentials")
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if stub.req == nil {
				t.Fatal("server saw no request")
			}
			if got := stub.req.GetCredentials(); got.GetKi() != tc.wantKi || got.GetOpc() != tc.wantOPc {
				t.Errorf("credentials sent = (%s, %s), want (%s, %s)", got.GetKi(), got.GetOpc(), tc.wantKi, tc.wantOPc)
			}
		})
	}
}

// `load` resolves credentials first: with none it fails naming both sources;
// with the environment set it gets past credentials to the next check, which
// here is a deliberately bad --bind count (so no load is actually run).
func TestLoadCredsFromEnv(t *testing.T) {
	tests := []struct {
		name          string
		flags         []string
		envKi, envOPc string
		wantErr       string
	}{
		{name: "neither fails on credentials", wantErr: "pass --ki/--opc or set $ORBIT_KI/$ORBIT_OPC"},
		{name: "malformed env fails on credentials", envKi: "bad", envOPc: envOPc, wantErr: "Ki"},
		{name: "env resolves and the run proceeds to the bind check", envKi: envKi, envOPc: envOPc, wantErr: "--bind: 2 bind addresses for 3 gNBs"},
		{name: "flags resolve and the run proceeds to the bind check", flags: []string{"--ki", goodKi, "--opc", goodOPc}, wantErr: "--bind: 2 bind addresses for 3 gNBs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_KI", tc.envKi)
			t.Setenv("ORBIT_OPC", tc.envOPc)
			root := New("test")
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			args := append([]string{"load", "--local", "--amf", "127.0.0.1:1", "--base-imsi", "208930100000000",
				"--mcc", "208", "--mnc", "93", "--gnb-count", "3", "--bind", "10.0.0.1,10.0.0.2", "--progress-every", "0"}, tc.flags...)
			root.SetArgs(args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
