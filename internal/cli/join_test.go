package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// stubJoin is a canned JoinService that keeps what it was sent.
type stubJoin struct {
	orbitv1connect.UnimplementedJoinServiceHandler
	create  *orbitv1.CreateJoinTokenRequest
	revoked string
}

const stubSecret = "oj1_AAAAAAAAAAAAAAAAAAAAAA"

func (s *stubJoin) CreateJoinToken(_ context.Context, req *connect.Request[orbitv1.CreateJoinTokenRequest]) (*connect.Response[orbitv1.CreateJoinTokenResponse], error) {
	s.create = req.Msg
	return connect.NewResponse(&orbitv1.CreateJoinTokenResponse{
		Issued:   &orbitv1.JoinToken{Id: "jt-1", Name: req.Msg.GetName(), ExpiresUnixNano: time.Now().Add(10 * time.Minute).UnixNano(), Spec: req.Msg.GetSpec()},
		Secret:   stubSecret,
		OneLiner: "curl … 'https://ran1:8412/join/" + stubSecret + "' | sudo sh",
	}), nil
}

func (s *stubJoin) ListJoinTokens(context.Context, *connect.Request[orbitv1.ListJoinTokensRequest]) (*connect.Response[orbitv1.ListJoinTokensResponse], error) {
	return connect.NewResponse(&orbitv1.ListJoinTokensResponse{Tokens: []*orbitv1.JoinToken{{Id: "jt-1", Name: "app1",
		ExpiresUnixNano: time.Now().UnixNano(), Spec: &orbitv1.JoinSpec{UePools: []string{"10.250.0.0/16"}, ManagementAddress: "ran1:8412"}}}}), nil
}

func (s *stubJoin) RevokeJoinToken(_ context.Context, req *connect.Request[orbitv1.RevokeJoinTokenRequest]) (*connect.Response[orbitv1.RevokeJoinTokenResponse], error) {
	s.revoked = req.Msg.GetId()
	return connect.NewResponse(&orbitv1.RevokeJoinTokenResponse{}), nil
}

func stubJoinServer(t *testing.T) (*stubJoin, string) {
	t.Helper()
	s := &stubJoin{}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewJoinServiceHandler(s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// `orbit join create`: the target gives the spec, a flag overrides one
// field, and stdout is the one-liner alone (scripts capture it).
func TestJoinCreate(t *testing.T) {
	clearOrbitEnv(t)
	// lab1 with the N6 side's view of the core.
	doc := strings.Replace(lab1, "  dnn: internet\n",
		"  dnn: internet\n  ue_pools: [10.250.0.0/16, 10.251.0.0/16]\n  upf_n6: 198.51.100.1\n  n6_subnet: 198.51.100.0/24\n", 1)
	tests := []struct {
		name      string
		args      []string
		wantPools string
		wantUPF   string
		wantMgmt  string
	}{
		{name: "from the target", args: []string{"-t", "lab1", "--name", "app1"}, wantPools: "10.250.0.0/16,10.251.0.0/16", wantUPF: "198.51.100.1"},
		{name: "a flag overrides the target", args: []string{"-t", "lab1", "--upf-n6", "198.51.100.9", "--mgmt", "ran1"},
			wantPools: "10.250.0.0/16,10.251.0.0/16", wantUPF: "198.51.100.9", wantMgmt: "ran1"},
		{name: "flags alone", args: []string{"--ue-pool", "10.9.0.0/16", "--upf-n6", "198.51.100.2", "--n6-subnet", "198.51.100.0/24"},
			wantPools: "10.9.0.0/16", wantUPF: "198.51.100.2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, url := stubJoinServer(t)
			r := runOrbit(t, targetDir(t, doc), append([]string{"--server", url, "join", "create"}, tc.args...)...)
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			sp := s.create.GetSpec()
			if strings.Join(sp.GetUePools(), ",") != tc.wantPools || sp.GetUpfN6() != tc.wantUPF || sp.GetManagementAddress() != tc.wantMgmt {
				t.Errorf("spec = %v", sp)
			}
			if lines := strings.Split(strings.TrimSpace(r.out), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "curl ") {
				t.Errorf("stdout = %q, want the one-liner alone", r.out)
			}
			if !strings.Contains(r.err, "jt-1") || !strings.Contains(r.err, "orbit join revoke jt-1") {
				t.Errorf("stderr = %q", r.err)
			}
		})
	}
	t.Run("a ttl that is not whole seconds", func(t *testing.T) {
		_, url := stubJoinServer(t)
		r := runOrbit(t, targetDir(t, doc), "--server", url, "join", "create", "-t", "lab1", "--ttl", "1500ms")
		if r.execErr == nil || !strings.Contains(r.execErr.Error(), "whole number of seconds") {
			t.Errorf("err = %v", r.execErr)
		}
	})
}

// list shows pending tokens, never a secret; revoke names the ID.
func TestJoinListRevoke(t *testing.T) {
	clearOrbitEnv(t)
	s, url := stubJoinServer(t)
	r := runOrbit(t, targetDir(t, lab1), "--server", url, "join", "list")
	if r.execErr != nil || !strings.Contains(r.out, "jt-1") || !strings.Contains(r.out, "app1") || strings.Contains(r.out, stubSecret) {
		t.Errorf("list: %v\n%s", r.execErr, r.out)
	}
	r = runOrbit(t, targetDir(t, lab1), "--server", url, "join", "revoke", "jt-1")
	if r.execErr != nil || s.revoked != "jt-1" {
		t.Errorf("revoke: %v, revoked %q", r.execErr, s.revoked)
	}
}

// The binaries a join serves: --join-binaries' orbit-linux-<arch> files,
// and this binary for its own arch when the directory has none for it.
func TestJoinBinaries(t *testing.T) {
	self := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	got, err := joinBinaries("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[runtime.GOARCH]; ok != self {
		t.Errorf("no dir: %v", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orbit-linux-arm64"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = joinBinaries(dir)
	if err != nil || got["arm64"] != filepath.Join(dir, "orbit-linux-arm64") {
		t.Errorf("dir: %v, %v", got, err)
	}
	if _, err := joinBinaries(t.TempDir()); err == nil || !strings.Contains(err.Error(), "holds neither") {
		t.Errorf("an empty dir: %v", err)
	}
}
