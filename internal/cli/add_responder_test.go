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
	"path/filepath"
	"strings"
	"sync"
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
}

// stubAddJoin answers CreateJoinToken with a URL on its own TLS server,
// lists that token as pending until a registration would use it (used) or
// it is revoked, and records revokes.
type stubAddJoin struct {
	orbitv1connect.UnimplementedJoinServiceHandler
	create    *orbitv1.CreateJoinTokenRequest
	base      string
	pin       string
	mu        sync.Mutex
	used      bool
	revokeErr error
	revoked   []string
}

func (s *stubAddJoin) ListJoinTokens(context.Context, *connect.Request[orbitv1.ListJoinTokensRequest]) (*connect.Response[orbitv1.ListJoinTokensResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &orbitv1.ListJoinTokensResponse{Tokens: []*orbitv1.JoinToken{{Id: "jt-other"}}}
	if !s.used && len(s.revoked) == 0 {
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
// command. MODE (a file next to it) can make mktemp lie or a step fail.
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
*--yes*) [ "$mode" = applyfail ] && exit 2; echo "orbit join: applied 9 changes." ;;
*"rm -f "*) echo removed >>"$dir/removed" ;;
esac
exit 0
`

func newAddResponderLab(t *testing.T, mode string) *addResponderLab {
	t.Helper()
	j := &stubAddJoin{}
	// The script over TLS, pinned; the API on loopback.
	scripts := http.NewServeMux()
	scripts.HandleFunc("/join/"+addSecret, func(w http.ResponseWriter, _ *http.Request) {
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
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &addResponderLab{join: j, url: srv.URL, log: filepath.Join(dir, "log"), stdins: dir}
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
		{name: "no terminal and no --yes", args: []string{"--ssh", "app1"}, wantErr: "pass --yes"},
		{name: "a host that would be an ssh option", args: []string{"--ssh", "-oProxyCommand=id", "--yes"}, wantErr: "want a host"},
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
	if applied, err := s.run(ctx, []byte("#!/bin/sh\n"), true, strings.NewReader("")); err == nil || applied {
		t.Fatalf("a cancelled run: applied %v, err %v", applied, err)
	}
	if !l.removed() {
		t.Error("a cancelled run left the remote file")
	}
}

// A plan declined here applies nothing: no apply call, the file removed,
// and not applied (so the token is revoked).
func TestAddResponderDeclined(t *testing.T) {
	l := newAddResponderLab(t, "")
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	s := &sshSession{ssh: ssh, host: "app1", out: io.Discard, errOut: &errOut}
	applied, err := s.run(context.Background(), []byte("#!/bin/sh\n"), false, strings.NewReader("n\n"))
	if err != nil || applied {
		t.Fatalf("applied %v, err %v", applied, err)
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
