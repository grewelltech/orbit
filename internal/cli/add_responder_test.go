package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// stubAddJoin answers CreateJoinToken with a URL on its own TLS server.
type stubAddJoin struct {
	orbitv1connect.UnimplementedJoinServiceHandler
	create *orbitv1.CreateJoinTokenRequest
	base   string
	pin    string
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
for last; do :; done
case $last in
*mktemp*) [ "$mode" = badpath ] && echo "/tmp/x; rm -rf ~" || echo /tmp/orbit-join.ABCDEFGHIJ ;;
"cat > "*) cat >"$dir/stdin.$n" ;;
*--dry-run*) [ "$mode" = dryfail ] && exit 3; [ "$mode" = slow ] && sleep 10; echo "Plan: (remote dry run)" ;;
*--yes*) [ "$mode" = applyfail ] && exit 2; echo "orbit join: applied 9 changes." ;;
"rm -f "*) echo removed >>"$dir/removed" ;;
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
	}{
		{name: "planned, then applied with --yes", args: []string{"--ssh", "ops@app1", "--yes"},
			wantCalls:   []string{"mktemp", "cat > '/tmp/orbit-join.ABCDEFGHIJ'", "--dry-run", "--yes; fi; rc=$?; rm -f '/tmp/orbit-join.ABCDEFGHIJ'", "rm -f '/tmp/orbit-join.ABCDEFGHIJ'"},
			wantRemoved: true, wantOut: "applied 9 changes"},
		{name: "no terminal and no --yes", args: []string{"--ssh", "app1"}, wantErr: "pass --yes"},
		{name: "a host that would be an ssh option", args: []string{"--ssh", "-oProxyCommand=id", "--yes"}, wantErr: "want a host"},
		{name: "mktemp answers something else", mode: "badpath", args: []string{"--ssh", "app1", "--yes"},
			wantErr: "not a /tmp/orbit-join.* path", wantCalls: []string{"mktemp"}},
		{name: "the plan fails", mode: "dryfail", args: []string{"--ssh", "app1", "--yes"}, wantErr: "the plan (--dry-run) failed",
			wantCalls: []string{"mktemp", "cat > ", "--dry-run", "rm -f "}, wantRemoved: true},
		{name: "the apply fails", mode: "applyfail", args: []string{"--ssh", "app1", "--yes"}, wantErr: "the join script failed",
			wantCalls: []string{"mktemp", "cat > ", "--dry-run", "--yes;", "rm -f "}, wantRemoved: true},
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
	if err := s.run(ctx, []byte("#!/bin/sh\n"), true, strings.NewReader(""), "jt-9"); err == nil {
		t.Fatal("a cancelled run succeeded")
	}
	if !l.removed() {
		t.Error("a cancelled run left the remote file")
	}
}
