package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// --server wins over $ORBIT_SERVER, which wins over the loopback default, and
// whatever is chosen must be an absolute http(s) URL. A rejection names the
// source so the operator knows whether the shell environment was involved.
func TestResolveServerURL(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		env     string
		want    string
		wantSrc serverSource
		wantErr string
	}{
		{name: "neither: loopback default", want: "http://127.0.0.1:8412", wantSrc: serverFromDefault},
		{name: "flag only", flag: "http://10.127.0.21:8412", want: "http://10.127.0.21:8412", wantSrc: serverFromFlag},
		{name: "env only", env: "http://10.120.0.21:8412", want: "http://10.120.0.21:8412", wantSrc: serverFromEnv},
		{name: "flag wins over env", flag: "http://flag:1", env: "http://env:2", want: "http://flag:1", wantSrc: serverFromFlag},
		{name: "https accepted", env: "https://orbit.lab", want: "https://orbit.lab", wantSrc: serverFromEnv},
		{name: "trailing slash kept", env: "http://orbit.lab:8412/", want: "http://orbit.lab:8412/", wantSrc: serverFromEnv},
		{name: "path prefix kept", flag: "http://orbit.lab/api", want: "http://orbit.lab/api", wantSrc: serverFromFlag},
		{name: "bare host:port from env", env: "10.127.0.21:8412",
			wantErr: `invalid server URL "10.127.0.21:8412" (from $ORBIT_SERVER): want http://host:port or https://host:port`},
		{name: "bare hostname:port from env", env: "localhost:8412",
			wantErr: `invalid server URL "localhost:8412" (from $ORBIT_SERVER)`},
		{name: "ftp scheme", env: "ftp://orbit.lab:8412", wantErr: `invalid server URL "ftp://orbit.lab:8412" (from $ORBIT_SERVER)`},
		{name: "empty host", env: "http://", wantErr: `invalid server URL "http://" (from $ORBIT_SERVER)`},
		{name: "port without host", env: "http://:8412", wantErr: `invalid server URL "http://:8412" (from $ORBIT_SERVER)`},
		{name: "port out of range", env: "http://127.0.0.1:99999", wantErr: `invalid server URL "http://127.0.0.1:99999" (from $ORBIT_SERVER)`},
		{name: "port zero", flag: "http://127.0.0.1:0", wantErr: `invalid server URL "http://127.0.0.1:0" (from --server)`},
		{name: "ipv6 literal accepted", env: "http://[::1]:8412", want: "http://[::1]:8412", wantSrc: serverFromEnv},
		{name: "no port accepted", env: "http://orbit.lab", want: "http://orbit.lab", wantSrc: serverFromEnv},
		{name: "bad flag names the flag", flag: "not-a-url", env: "http://env:2",
			wantErr: `invalid server URL "not-a-url" (from --server)`},
		{name: "bad env ignored when flag set", flag: "http://flag:1", env: "garbage", want: "http://flag:1", wantSrc: serverFromFlag},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_SERVER", tc.env)
			got, src, err := resolveServerURL(tc.flag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want || src != tc.wantSrc {
				t.Errorf("got (%q, %s), want (%q, %s)", got, src, tc.want, tc.wantSrc)
			}
		})
	}
}

// hookedGroups are the top-level commands whose every leaf talks to the API
// server; exactly these carry the --server resolution hook.
var hookedGroups = []string{"system", "cell", "ue", "runs"}

// runCommand is the one dual-mode client command: a step scenario is an API
// client and resolves --server in RunE, a `kind: fleet` scenario drives the
// engine in-process and never reads $ORBIT_SERVER. It has no hook.
const runCommand = "run"

// targetCommand is a mixed group: target init (unless --no-probe) and probe
// talk to the server and resolve --server in RunE; list and show only read
// files and never read $ORBIT_SERVER. It has no hook either
// (TestTargetCommandsServerURL).
const targetCommand = "target"

// localCommands are the top-level commands that never talk to the API server
// and so never read $ORBIT_SERVER. With hookedGroups and runCommand this is
// the whole top level: a new command must be classified in one of them, so a
// client group added without withServerURL cannot pass as "local".
var localCommands = []string{"version", "serve", "load", "conformance", "responder"}

// leaves returns every runnable leaf below c with its full path.
func leaves(c *cobra.Command, path string) []string {
	if !c.HasSubCommands() {
		return []string{path}
	}
	var out []string
	for _, sub := range c.Commands() {
		out = append(out, leaves(sub, path+" "+sub.Name())...)
	}
	return out
}

// Every hooked group has the hook, nothing below a group overrides it (cobra
// runs only the nearest PersistentPreRunE), `run` resolves in RunE instead,
// and no local command has a hook. Then, behaviourally: every hooked leaf
// fails on a malformed $ORBIT_SERVER before doing anything else; `run` does
// so only for a step scenario and ignores the variable for a fleet; the
// commands that never talk to the server are untouched by it.
func TestServerURLHookCoversClientCommands(t *testing.T) {
	root := New("test")
	isClient := map[string]bool{}
	for _, g := range hookedGroups {
		isClient[g] = true
	}
	known := map[string]bool{runCommand: true, targetCommand: true}
	for _, name := range append(append([]string{}, hookedGroups...), localCommands...) {
		known[name] = true
	}
	for _, top := range root.Commands() {
		if !known[top.Name()] {
			t.Errorf("orbit %s: not classified; add it to hookedGroups (wrap it with withServerURL) or localCommands in serverurl_test.go", top.Name())
		}
	}
	var clientLeaves []string
	for _, top := range root.Commands() {
		if top.Name() == runCommand || top.Name() == targetCommand {
			if top.PersistentPreRunE != nil || top.PersistentPreRun != nil {
				t.Errorf("orbit %s: has a hook; it must resolve --server in RunE, only where it talks to the server", top.Name())
			}
			continue
		}
		if isClient[top.Name()] {
			if top.PersistentPreRunE == nil {
				t.Errorf("orbit %s: client group has no PersistentPreRunE", top.Name())
			}
			for _, sub := range top.Commands() {
				var walk func(c *cobra.Command, path string)
				walk = func(c *cobra.Command, path string) {
					if c.PersistentPreRunE != nil || c.PersistentPreRun != nil {
						t.Errorf("%s: overrides the group's --server hook", path)
					}
					for _, s := range c.Commands() {
						walk(s, path+" "+s.Name())
					}
				}
				walk(sub, "orbit "+top.Name()+" "+sub.Name())
			}
			clientLeaves = append(clientLeaves, leaves(top, "orbit "+top.Name())...)
			continue
		}
		if top.PersistentPreRunE != nil || top.PersistentPreRun != nil {
			t.Errorf("orbit %s: non-client command has a persistent pre-run hook", top.Name())
		}
	}
	if len(clientLeaves) < 20 {
		t.Fatalf("found only %d client leaves; the walk is broken", len(clientLeaves))
	}

	t.Setenv("ORBIT_SERVER", "not-a-url")
	const wantURLErr = `invalid server URL "not-a-url" (from $ORBIT_SERVER)`
	for _, leaf := range clientLeaves {
		t.Run(leaf, func(t *testing.T) {
			// One positional argument satisfies every leaf's Args rule; the hook
			// runs after args validation and before required-flag checks and
			// RunE, so nothing here needs a server.
			args := append(strings.Fields(leaf)[1:], "x")
			err := execute(t, args...)
			if err == nil || !strings.Contains(err.Error(), wantURLErr) {
				t.Errorf("err = %v, want containing %q", err, wantURLErr)
			}
		})
	}

	// `run`: a step scenario is a client and fails on the URL before any
	// request; a fleet scenario gets past URL handling (it fails later on its
	// own terms, here validation) and prints no server line.
	dir := t.TempDir()
	stepFile := filepath.Join(dir, "step.yaml")
	fleetFile := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(stepFile, []byte("steps: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleetFile, []byte("kind: fleet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("orbit run step", func(t *testing.T) {
		_, stderr, err := executeCapture(t, "run", stepFile)
		if err == nil || !strings.Contains(err.Error(), wantURLErr) {
			t.Errorf("err = %v, want containing %q", err, wantURLErr)
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})
	t.Run("orbit run fleet", func(t *testing.T) {
		_, stderr, err := executeCapture(t, "run", fleetFile)
		if err == nil {
			t.Fatal("a bare fleet file ran; expected it to fail validation")
		}
		if strings.Contains(err.Error(), "invalid server URL") {
			t.Errorf("fleet mode read $ORBIT_SERVER: %v", err)
		}
		if strings.Contains(stderr, "server:") {
			t.Errorf("fleet mode printed the server line: %q", stderr)
		}
	})
	t.Run("orbit run fleet with valid env prints nothing", func(t *testing.T) {
		t.Setenv("ORBIT_SERVER", "http://192.0.2.1:8412")
		_, stderr, err := executeCapture(t, "run", fleetFile)
		if err == nil {
			t.Fatal("a bare fleet file ran; expected it to fail validation")
		}
		if strings.Contains(stderr, "server:") {
			t.Errorf("fleet mode printed the server line: %q", stderr)
		}
	})

	// `version` runs to completion under a malformed $ORBIT_SERVER; the
	// other non-client commands fail on an unknown flag, never on the URL.
	if err := execute(t, "version"); err != nil {
		t.Errorf("version: %v", err)
	}
	for _, name := range localCommands {
		if name == "version" {
			continue
		}
		err := execute(t, name, "--no-such-flag")
		if err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s: err = %v, want unknown flag", name, err)
		}
	}
}

// execute runs the orbit command tree with args and discards its output.
func execute(t *testing.T, args ...string) error {
	t.Helper()
	_, _, err := executeCapture(t, args...)
	return err
}

// executeCapture runs the orbit command tree with args and returns what it
// wrote to stdout and stderr separately.
func executeCapture(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := New("test")
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// Owner condition: the server's listen default does not move. `serve` has no
// --server hook, --listen still defaults to loopback, and a malformed
// $ORBIT_SERVER has no effect on its flag parsing.
func TestServeIgnoresServerEnv(t *testing.T) {
	var serve *cobra.Command
	for _, c := range New("test").Commands() {
		if c.Name() == "serve" {
			serve = c
		}
	}
	if serve == nil {
		t.Fatal("no serve command")
	}
	if serve.PersistentPreRunE != nil || serve.PersistentPreRun != nil {
		t.Error("serve has a persistent pre-run hook")
	}
	if DefaultListen != "127.0.0.1:8412" {
		t.Errorf("DefaultListen = %q, want loopback", DefaultListen)
	}
	listen := serve.Flags().Lookup("listen")
	if listen == nil || listen.DefValue != "127.0.0.1:8412" {
		t.Errorf("serve --listen default = %v, want 127.0.0.1:8412", listen)
	}
	t.Setenv("ORBIT_SERVER", "not-a-url")
	if err := serve.ParseFlags([]string{"--listen", "0.0.0.0:1"}); err != nil {
		t.Errorf("serve flag parsing failed under ORBIT_SERVER=not-a-url: %v", err)
	}
}

// stubUEList answers List with a fixed SUPI and counts the calls it saw.
type stubUEList struct {
	orbitv1connect.UnimplementedUEServiceHandler
	supi  string
	calls int
}

func (s *stubUEList) List(context.Context, *connect.Request[orbitv1.ListRequest]) (*connect.Response[orbitv1.ListResponse], error) {
	s.calls++
	return connect.NewResponse(&orbitv1.ListResponse{
		Ues: []*orbitv1.UEStatus{{Supi: s.supi, State: "REGISTERED"}},
	}), nil
}

func newUEListServer(t *testing.T, supi string) (*stubUEList, *httptest.Server) {
	t.Helper()
	stub := &stubUEList{supi: supi}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewUEServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stub, srv
}

// `ue list` reaches the server named by $ORBIT_SERVER when --server is
// absent, and --server wins over it when both are given.
func TestUEListServerFromEnv(t *testing.T) {
	envStub, envSrv := newUEListServer(t, "001010000000001")
	flagStub, flagSrv := newUEListServer(t, "001010000000002")
	t.Setenv("ORBIT_SERVER", envSrv.URL)

	tests := []struct {
		name       string
		flags      []string
		hit        *stubUEList
		idle       *stubUEList
		wantSupi   string
		wantStderr string
	}{
		{name: "env only", hit: envStub, idle: flagStub, wantSupi: "001010000000001",
			wantStderr: "server: " + envSrv.URL + " (from $ORBIT_SERVER)\n"},
		{name: "flag wins", flags: []string{"--server", flagSrv.URL}, hit: flagStub, idle: envStub, wantSupi: "001010000000002"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envStub.calls, flagStub.calls = 0, 0
			stdout, stderr, err := executeCapture(t, append(tc.flags, "ue", "list")...)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if tc.hit.calls != 1 || tc.idle.calls != 0 {
				t.Errorf("calls: chosen server %d, other %d; want 1 and 0", tc.hit.calls, tc.idle.calls)
			}
			if !strings.Contains(stdout, tc.wantSupi) {
				t.Errorf("stdout %q does not list %s", stdout, tc.wantSupi)
			}
			if strings.Contains(stdout, "server:") {
				t.Errorf("stdout carries the server line: %q", stdout)
			}
			if stderr != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr, tc.wantStderr)
			}
		})
	}
}

// A fallback that is used is never silent: applyServerURL prints the server
// line exactly once, on stderr only, when $ORBIT_SERVER chose the URL, and
// not at all for an explicit flag or the default; in every case the resolved
// value is written back. Called directly so no connection is ever opened.
func TestServerURLHookPrintsEnvSourceOnce(t *testing.T) {
	const envURL = "http://192.0.2.1:8412"
	const flagURL = "http://192.0.2.2:8412"
	tests := []struct {
		name       string
		flag       string
		env        string
		want       string
		wantStderr string
	}{
		{name: "env prints once", env: envURL, want: envURL, wantStderr: "server: " + envURL + " (from $ORBIT_SERVER)\n"},
		{name: "flag over env prints nothing", flag: flagURL, env: envURL, want: flagURL},
		{name: "neither prints nothing", want: defaultServerURL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORBIT_SERVER", tc.env)
			c := &cobra.Command{Use: "probe"}
			var out, errOut bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&errOut)
			serverURL := tc.flag
			if err := applyServerURL(c, &serverURL); err != nil {
				t.Fatalf("applyServerURL: %v", err)
			}
			if serverURL != tc.want {
				t.Errorf("resolved %q, want %q", serverURL, tc.want)
			}
			if errOut.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", errOut.String(), tc.wantStderr)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", out.String())
			}
		})
	}
}
