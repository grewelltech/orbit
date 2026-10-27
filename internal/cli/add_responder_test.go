package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/tlspin"
)

// addResponderLab is a TLS join server that serves a script holding the
// token, and a fake ssh on PATH that logs every call's argv and stdin and
// plays the app host's part.
type addResponderLab struct {
	join   *stubAddJoin
	url    string
	log    string // the fake ssh's call log
	stdins string // where it keeps what each call read
	// onScript, when set, runs as the script is served, before it is.
	onScript func(*http.Request)
}

// stubAddJoin answers CreateJoinToken with a URL on its own TLS server,
// lists that token as pending until a registration uses it (used, or the
// fake ssh's apply writing usedFile) or it is revoked, and records revokes.
type stubAddJoin struct {
	orbitv1connect.UnimplementedJoinServiceHandler
	create    *orbitv1.CreateJoinTokenRequest
	base      string
	pin       string
	usedFile  string
	mu        sync.Mutex
	used      bool
	revokeErr error
	revoked   []string
}

func (s *stubAddJoin) ListJoinTokens(context.Context, *connect.Request[orbitv1.ListJoinTokensRequest]) (*connect.Response[orbitv1.ListJoinTokensResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &orbitv1.ListJoinTokensResponse{Tokens: []*orbitv1.JoinToken{{Id: "jt-other"}}}
	used := s.used
	if s.usedFile != "" {
		_, err := os.Stat(s.usedFile)
		used = used || err == nil
	}
	if !used && len(s.revoked) == 0 {
		out.Tokens = append(out.Tokens, &orbitv1.JoinToken{Id: "jt-9"})
	}
	return connect.NewResponse(out), nil
}

func (s *stubAddJoin) RevokeJoinToken(_ context.Context, req *connect.Request[orbitv1.RevokeJoinTokenRequest]) (*connect.Response[orbitv1.RevokeJoinTokenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revokeErr != nil {
		return nil, s.revokeErr
	}
	s.revoked = append(s.revoked, req.Msg.GetId())
	return connect.NewResponse(&orbitv1.RevokeJoinTokenResponse{}), nil
}

const addSecret = "oj1_SECRETSECRETSECRETSE"

func (s *stubAddJoin) CreateJoinToken(_ context.Context, req *connect.Request[orbitv1.CreateJoinTokenRequest]) (*connect.Response[orbitv1.CreateJoinTokenResponse], error) {
	s.create = req.Msg
	return connect.NewResponse(&orbitv1.CreateJoinTokenResponse{
		Issued: &orbitv1.JoinToken{Id: "jt-9", ExpiresUnixNano: time.Now().Add(10 * time.Minute).UnixNano()},
		Secret: addSecret, Url: s.base + "/join/" + addSecret, Pin: s.pin,
	}), nil
}

// fakeSSH is the fake ssh: it logs its argv (one call per line, args
// separated by " | ") and each call's stdin, and answers by the remote
// command. MODE (a file next to it) can make mktemp lie or a step fail. An
// apply registers (writes "registered", which uses the token) unless MODE
// is noreg: a rerun on a host already joined, nothing to register.
const fakeSSH = `#!/bin/sh
dir=$(dirname "$0")
n=$(($(cat "$dir/n" 2>/dev/null || echo 0) + 1)); echo $n >"$dir/n"
{ printf '%s' "$1"; shift; for a; do printf ' | %s' "$a"; done; echo; } >>"$dir/log"
cat >"$dir/stdin.$n" </dev/null
mode=$(cat "$dir/mode" 2>/dev/null)
[ "$mode" = sshfail ] && { echo "ssh: connect to host app1 port 22: Connection refused" >&2; exit 255; }
for last; do :; done
case $last in
"sh -c "*) ;;
*) echo "not an sh -c command: $last" >&2; exit 255 ;;
esac
case $last in
*mktemp*) [ "$mode" = badpath ] && echo "/tmp/x; rm -rf ~" || echo /tmp/orbit-join.ABCDEFGHIJ ;;
*"cat > "*) cat >"$dir/stdin.$n" ;;
*--dry-run*) [ "$mode" = dryfail ] && exit 3; [ "$mode" = slow ] && sleep 10; echo "Plan: (remote dry run)" ;;
*--yes*) [ "$mode" = applyfail ] && exit 2; [ "$mode" = noreg ] || : >"$dir/registered"; echo "orbit join: applied 9 changes." ;;
*"rm -f "*) echo removed >>"$dir/removed" ;;
esac
exit 0
`

func newAddResponderLab(t *testing.T, mode string) *addResponderLab {
	t.Helper()
	j := &stubAddJoin{}
	l := &addResponderLab{join: j}
	// The script over TLS, pinned; the API on loopback.
	scripts := http.NewServeMux()
	scripts.HandleFunc("/join/"+addSecret, func(w http.ResponseWriter, r *http.Request) {
		if l.onScript != nil {
			l.onScript(r)
		}
		_, _ = io.WriteString(w, "#!/bin/sh\n# join script\nORBIT_TOKEN='"+addSecret+"'\n")
	})
	tlsSrv := httptest.NewTLSServer(scripts)
	t.Cleanup(tlsSrv.Close)
	j.base, j.pin = tlsSrv.URL, tlspin.Fingerprint(tlsSrv.Certificate())
	api := http.NewServeMux()
	api.Handle(orbitv1connect.NewJoinServiceHandler(j))
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	j.usedFile = filepath.Join(dir, "registered")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	l.url, l.log, l.stdins = srv.URL, filepath.Join(dir, "log"), dir
	return l
}

func (l *addResponderLab) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(l.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (l *addResponderLab) removed() bool {
	_, err := os.Stat(filepath.Join(l.stdins, "removed"))
	return err == nil
}

// The ssh exchange: mktemp, the script copied in on stdin, the plan, the
// apply, and the file removed on every path. The token is on no argv; no
// SSH detail reaches the token request or the target.
func TestAddResponderSSH(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	clearOrbitEnv(t)
	doc := strings.Replace(lab1, "  dnn: internet\n",
		"  dnn: internet\n  ue_pools: [10.250.0.0/16]\n  upf_n6: 198.51.100.1\n  n6_subnet: 198.51.100.0/24\n", 1)
	tests := []struct {
		name        string
		mode        string
		args        []string
		wantErr     string
		wantCalls   []string // a substring of each call's remote command, in order
		wantRemoved bool
		wantOut     string
		wantRevoked bool
		wantNoToken bool // refused before a token was created
		// exact is each call's whole argv, as the fake ssh logs it.
		exact []string
	}{
		{name: "planned, then applied with --yes", args: []string{"--ssh", "ops@app1", "--yes"},
			wantCalls:   []string{"mktemp", "cat > ", "--dry-run", "--yes; fi; rc=$?; rm -f ", "rm -f "},
			wantRemoved: true, wantOut: "applied 9 changes",
			exact: []string{
				`ops@app1 | sh -c 'umask 077; mktemp /tmp/orbit-join.XXXXXXXXXX'`,
				`ops@app1 | sh -c 'cat > '\''/tmp/orbit-join.ABCDEFGHIJ'\'''`,
				`ops@app1 | sh -c 'if [ "$(id -u)" = 0 ]; then sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --dry-run; else sudo sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --dry-run; fi'`,
				`ops@app1 | sh -c 'if [ "$(id -u)" = 0 ]; then sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --yes; else sudo sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --yes; fi; rc=$?; rm -f '\''/tmp/orbit-join.ABCDEFGHIJ'\''; exit $rc'`,
				`ops@app1 | sh -c 'rm -f '\''/tmp/orbit-join.ABCDEFGHIJ'\'''`,
			}},
		{name: "applied, nothing registered (a rerun): the token revoked", mode: "noreg", args: []string{"--ssh", "app1", "--yes"},
			wantCalls:   []string{"mktemp", "cat > ", "--dry-run", "--yes; fi; rc=$?; rm -f ", "rm -f "},
			wantRemoved: true, wantOut: "applied 9 changes", wantRevoked: true},
		{name: "the script options passed through, quoted",
			args:      []string{"--ssh", "app1", "--yes", "--n6-ip", "198.51.100.20", "--bind", "[2001:db8::20]:9551", "--no-firewall", "--no-route", "--no-systemd"},
			wantCalls: []string{"mktemp", "cat > ", "--dry-run", "--yes ", "rm -f "}, wantRemoved: true,
			exact: []string{
				`app1 | sh -c 'umask 077; mktemp /tmp/orbit-join.XXXXXXXXXX'`,
				`app1 | sh -c 'cat > '\''/tmp/orbit-join.ABCDEFGHIJ'\'''`,
				`app1 | sh -c 'if [ "$(id -u)" = 0 ]; then sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --dry-run --n6-ip '\''198.51.100.20'\'' --bind '\''[2001:db8::20]:9551'\'' --no-firewall --no-route --no-systemd; else sudo sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --dry-run --n6-ip '\''198.51.100.20'\'' --bind '\''[2001:db8::20]:9551'\'' --no-firewall --no-route --no-systemd; fi'`,
				`app1 | sh -c 'if [ "$(id -u)" = 0 ]; then sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --yes --n6-ip '\''198.51.100.20'\'' --bind '\''[2001:db8::20]:9551'\'' --no-firewall --no-route --no-systemd; else sudo sh '\''/tmp/orbit-join.ABCDEFGHIJ'\'' --yes --n6-ip '\''198.51.100.20'\'' --bind '\''[2001:db8::20]:9551'\'' --no-firewall --no-route --no-systemd; fi; rc=$?; rm -f '\''/tmp/orbit-join.ABCDEFGHIJ'\''; exit $rc'`,
				`app1 | sh -c 'rm -f '\''/tmp/orbit-join.ABCDEFGHIJ'\'''`,
			}},
		{name: "--n6-ip not an address", args: []string{"--ssh", "app1", "--yes", "--n6-ip", "198.51.100.20; reboot"},
			wantErr: "--n6-ip", wantNoToken: true},
		{name: "--bind without a port", args: []string{"--ssh", "app1", "--yes", "--bind", "192.0.2.20"},
			wantErr: "--bind", wantNoToken: true},
		{name: "--bind a name, not an address", args: []string{"--ssh", "app1", "--yes", "--bind", "app1:9551"},
			wantErr: "--bind", wantNoToken: true},
		{name: "--bind port 0", args: []string{"--ssh", "app1", "--yes", "--bind", "192.0.2.20:0"},
			wantErr: "--bind", wantNoToken: true},
		{name: "no terminal and no --yes", args: []string{"--ssh", "app1"}, wantErr: "pass --yes", wantNoToken: true},
		{name: "a host that would be an ssh option", args: []string{"--ssh", "-oProxyCommand=id", "--yes"}, wantErr: "want a host", wantNoToken: true},
		// No '=' in these: only the leading '-' refuses them.
		{name: "a host that is an ssh option, no =", args: []string{"--ssh", "-oProxyCommand", "--yes"}, wantErr: "want a host", wantNoToken: true},
		{name: "a host that is an ssh flag", args: []string{"--ssh", "-x", "--yes"}, wantErr: "want a host", wantNoToken: true},
		{name: "mktemp answers something else", mode: "badpath", args: []string{"--ssh", "app1", "--yes"},
			wantErr: "not a /tmp/orbit-join.* path", wantCalls: []string{"mktemp"}, wantRevoked: true},
		{name: "the plan fails", mode: "dryfail", args: []string{"--ssh", "app1", "--yes"}, wantErr: "the plan (--dry-run) failed",
			wantCalls: []string{"mktemp", "cat > ", "--dry-run", "rm -f "}, wantRemoved: true, wantRevoked: true},
		{name: "the apply fails", mode: "applyfail", args: []string{"--ssh", "app1", "--yes"}, wantErr: "the join script failed",
			wantCalls: []string{"mktemp", "cat > ", "--dry-run", "--yes;", "rm -f "}, wantRemoved: true, wantRevoked: true},
		{name: "ssh fails outright", mode: "sshfail", args: []string{"--ssh", "app1", "--yes"}, wantErr: "mktemp",
			wantCalls: []string{"mktemp"}, wantRevoked: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newAddResponderLab(t, tc.mode)
			dir := targetDir(t, doc)
			before, _ := os.ReadFile(filepath.Join(dir.Dir, "orbit.yaml"))
			r := runOrbit(t, dir, append([]string{"--server", l.url, "target", "add-responder", "-t", "lab1"}, tc.args...)...)
			if tc.wantErr == "" && r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			if tc.wantErr != "" && (r.execErr == nil || !strings.Contains(r.execErr.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", r.execErr, tc.wantErr)
			}
			calls := l.calls(t)
			if len(calls) != len(tc.wantCalls) {
				t.Fatalf("ssh calls:\n%s\nwant %d", strings.Join(calls, "\n"), len(tc.wantCalls))
			}
			for i, w := range tc.wantCalls {
				if !strings.Contains(calls[i], w) {
					t.Errorf("call %d = %q, want %q", i, calls[i], w)
				}
			}
			for i, w := range tc.exact {
				if calls[i] != w {
					t.Errorf("call %d argv:\n got %s\nwant %s", i, calls[i], w)
				}
			}
			if got := len(l.join.revoked) > 0; got != tc.wantRevoked {
				t.Errorf("the token revoked: %v (%v), want %v\n%s", got, l.join.revoked, tc.wantRevoked, r.err)
			}
			if tc.wantRevoked && !strings.Contains(r.err, "revoked the unused join token jt-9") {
				t.Errorf("stderr = %q", r.err)
			}
			if tc.wantNoToken && l.join.create != nil {
				t.Errorf("a token was created before the refusal: %v", l.join.create)
			}
			if strings.Contains(strings.Join(calls, "\n"), addSecret) {
				t.Errorf("the token is on an ssh argv:\n%s", strings.Join(calls, "\n"))
			}
			if l.removed() != tc.wantRemoved {
				t.Errorf("the remote file removed: %v, want %v", l.removed(), tc.wantRemoved)
			}
			if tc.wantOut != "" && !strings.Contains(r.out, tc.wantOut) {
				t.Errorf("out = %q", r.out)
			}
			if len(calls) >= 2 {
				// The script reached the host on stdin, token and all.
				if b, err := os.ReadFile(filepath.Join(l.stdins, "stdin.2")); err != nil || !strings.Contains(string(b), addSecret) {
					t.Errorf("the copy's stdin: %q, %v", b, err)
				}
			}
			if c := l.join.create; c != nil {
				if strings.Contains(c.String(), "app1") && c.GetName() != "app1" || strings.Contains(c.String(), "ops@") {
					t.Errorf("the token request carries SSH detail: %v", c)
				}
			}
			after, _ := os.ReadFile(filepath.Join(dir.Dir, "orbit.yaml"))
			if string(before) != string(after) {
				t.Error("the target file changed")
			}
		})
	}
}

// A Ctrl-C while the script is fetched, after the token exists and before
// any ssh, cancels the command rather than ending the process, and the
// token is revoked. The test holds its own SIGINT channel so that, if the
// command caught nothing then, the process lives and the row fails.
func TestAddResponderInterruptedFetchRevokes(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	clearOrbitEnv(t)
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, os.Interrupt)
	defer signal.Stop(guard)
	l := newAddResponderLab(t, "")
	l.onScript = func(r *http.Request) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}
	doc := strings.Replace(lab1, "  dnn: internet\n",
		"  dnn: internet\n  ue_pools: [10.250.0.0/16]\n  upf_n6: 198.51.100.1\n  n6_subnet: 198.51.100.0/24\n", 1)
	r := runOrbit(t, targetDir(t, doc), "--server", l.url, "target", "add-responder", "-t", "lab1", "--ssh", "app1", "--yes")
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "fetching the join script") {
		t.Fatalf("err = %v, want the fetch cancelled", r.execErr)
	}
	if calls := l.calls(t); len(calls) != 0 {
		t.Errorf("ssh ran after the Ctrl-C:\n%s", strings.Join(calls, "\n"))
	}
	if len(l.join.revoked) != 1 || !strings.Contains(r.err, "revoked the unused join token jt-9") {
		t.Errorf("revoked %v; stderr %q", l.join.revoked, r.err)
	}
}

// A run cancelled mid-way (Ctrl-C) still removes the remote file.
func TestAddResponderCancelledRemoves(t *testing.T) {
	l := newAddResponderLab(t, "slow")
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s := &sshSession{ssh: ssh, host: "app1", out: io.Discard, errOut: io.Discard}
	if err := s.run(ctx, []byte("#!/bin/sh\n"), true, strings.NewReader("")); err == nil {
		t.Fatal("a cancelled run returned no error")
	}
	if !l.removed() {
		t.Error("a cancelled run left the remote file")
	}
}

// A plan declined here applies nothing: no apply call, the file removed.
// (The token is then pending, and the command's revokeUnused revokes it.)
func TestAddResponderDeclined(t *testing.T) {
	l := newAddResponderLab(t, "")
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	s := &sshSession{ssh: ssh, host: "app1", out: io.Discard, errOut: &errOut}
	if err := s.run(context.Background(), []byte("#!/bin/sh\n"), false, strings.NewReader("n\n")); err != nil {
		t.Fatal(err)
	}
	calls := l.calls(t)
	if len(calls) != 4 || strings.Contains(strings.Join(calls, "\n"), "--yes") || !l.removed() {
		t.Errorf("calls:\n%s\nremoved %v", strings.Join(calls, "\n"), l.removed())
	}
	if !strings.Contains(errOut.String(), "not applied: nothing on app1 was changed") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// revokeUnused revokes a pending token and only that; a token a
// registration used is left alone; a failure is told with the ID and
// returns nothing to mask the run's own error.
func TestRevokeUnused(t *testing.T) {
	tests := []struct {
		name        string
		used        bool
		revokeErr   error
		wantRevoked bool
		wantOut     string
	}{
		{name: "pending: revoked", wantRevoked: true, wantOut: "orbit: revoked the unused join token jt-9\n"},
		{name: "used by a registration: left alone", used: true},
		{name: "the revoke fails: told, with the ID", revokeErr: connect.NewError(connect.CodeUnavailable, errors.New("down")),
			wantOut: "could not revoke the unused join token jt-9: unavailable: down; revoke it with orbit join revoke jt-9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := &stubAddJoin{used: tc.used, revokeErr: tc.revokeErr}
			var out bytes.Buffer
			revokeUnused(j, "jt-9", &out)
			if got := len(j.revoked) == 1 && j.revoked[0] == "jt-9"; got != tc.wantRevoked {
				t.Errorf("revoked %v, want %v", j.revoked, tc.wantRevoked)
			}
			if !strings.Contains(out.String(), tc.wantOut) || (tc.wantOut == "" && out.Len() != 0) {
				t.Errorf("out = %q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}
