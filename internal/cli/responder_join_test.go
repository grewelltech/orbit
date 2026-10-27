package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

// stubResponders is a canned ResponderService that keeps what it removed.
type stubResponders struct {
	orbitv1connect.UnimplementedResponderServiceHandler
	removed string
}

func (s *stubResponders) ListResponders(context.Context, *connect.Request[orbitv1.ListRespondersRequest]) (*connect.Response[orbitv1.ListRespondersResponse], error) {
	return connect.NewResponse(&orbitv1.ListRespondersResponse{Responders: []*orbitv1.RegisteredResponder{{
		Id: "rs-1", Name: "app1", State: "online", LastSeenUnixNano: time.Now().Add(-3 * time.Second).UnixNano(),
		Source: "203.0.113.7:40000", ControlAddress: "198.51.100.20:9551", DataIp: "198.51.100.20", Version: "v1",
		RegisteredUnixNano: time.Now().UnixNano(), RegisteredFrom: "203.0.113.7:39999", JoinTokenId: "jt-1"}}}), nil
}

func (s *stubResponders) RemoveResponder(_ context.Context, req *connect.Request[orbitv1.RemoveResponderRequest]) (*connect.Response[orbitv1.RemoveResponderResponse], error) {
	s.removed = req.Msg.GetName()
	return connect.NewResponse(&orbitv1.RemoveResponderResponse{Id: "rs-1"}), nil
}

// list shows each responder with its state and token ID; its source
// addresses only with -o wide. remove names the responder.
func TestResponderListRemove(t *testing.T) {
	clearOrbitEnv(t)
	s := &stubResponders{}
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewResponderServiceHandler(s))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantNot []string
	}{
		{name: "default", args: []string{"responder", "list"},
			want: []string{"NAME", "app1", "rs-1", "online", "ago", "198.51.100.20:9551", "jt-1"}, wantNot: []string{"203.0.113.7", "SOURCE"}},
		{name: "wide", args: []string{"responder", "list", "-o", "wide"}, want: []string{"SOURCE", "203.0.113.7:40000", "203.0.113.7:39999"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runOrbit(t, targetDir(t, lab1), append([]string{"--server", srv.URL}, tc.args...)...)
			if r.execErr != nil {
				t.Fatalf("%v\n%s", r.execErr, r.err)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.out, w) {
					t.Errorf("output lacks %q:\n%s", w, r.out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(r.out, w) {
					t.Errorf("output has %q:\n%s", w, r.out)
				}
			}
		})
	}
	r := runOrbit(t, targetDir(t, lab1), "--server", srv.URL, "responder", "remove", "app1")
	if r.execErr != nil || s.removed != "app1" || !strings.Contains(r.err, "removed responder app1 (rs-1)") {
		t.Errorf("remove: %v, %q\n%s", r.execErr, s.removed, r.err)
	}
}

// The secrets a responder command reads come from files only other users
// cannot read; the token never needs to be on the command line.
func TestResponderSecretFiles(t *testing.T) {
	clearOrbitEnv(t)
	dir := t.TempDir()
	open := filepath.Join(dir, "token")
	if err := os.WriteFile(open, []byte("oj1_AAAAAAAAAAAAAAAAAAAAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "register: a token file others can read", args: []string{"responder", "register", "--server", "https://192.0.2.10:8412",
			"--pin", "sha256/47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=", "--token-file", open, "--name", "app1",
			"--control", "198.51.100.20:9551", "--data-ip", "198.51.100.20", "--data-ports", "32768-60999", "--config", filepath.Join(dir, "r.yaml")},
			want: "chmod 600"},
		{name: "responder: a token file others can read", args: []string{"responder", "--bind", "127.0.0.1:0", "--token-file", open}, want: "chmod 600"},
		{name: "responder: --token and --token-file", args: []string{"responder", "--bind", "127.0.0.1:0", "--token", "x", "--token-file", open}, want: "token"},
		{name: "responder: a config others can read", args: []string{"responder", "--config", open}, want: "chmod 600"},
		{name: "register: a bad port range", args: []string{"responder", "register", "--server", "https://192.0.2.10:8412",
			"--pin", "sha256/47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=", "--token-file", open, "--name", "app1",
			"--control", "198.51.100.20:9551", "--data-ip", "198.51.100.20", "--data-ports", "60999-32768"}, want: "chmod 600"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := runOrbit(t, targetDir(t, lab1), tc.args...)
			if r.execErr == nil || !strings.Contains(r.execErr.Error(), tc.want) {
				t.Errorf("err = %v, want %q", r.execErr, tc.want)
			}
		})
	}
}
