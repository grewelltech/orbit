package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/join"
	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/tlspin"
)

var joinSpec = &orbitv1.JoinSpec{UePools: []string{"10.250.0.0/16"}, UpfN6: "198.51.100.1", N6Subnet: "198.51.100.0/24"}

// joinLab is a join service behind a TLS (or plain) test server, with its
// log captured.
type joinLab struct {
	srv  *httptest.Server
	j    *joinService
	log  *bytes.Buffer
	join orbitv1connect.JoinServiceClient
	bin  []byte
}

func newJoinLab(t *testing.T, tls bool, listen string) *joinLab {
	t.Helper()
	bin := []byte("#!/bin/sh\necho orbit\n")
	binPath := filepath.Join(t.TempDir(), "orbit-linux-amd64")
	if err := os.WriteFile(binPath, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reg, err := join.OpenRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	j := newJoinService(log, newSecurity(log, ""), Options{TLS: tls, Listen: listen, JoinBinaries: map[string]string{"amd64": binPath}}, reg)
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewJoinServiceHandler(j))
	mux.Handle("/join/", j)
	srv := httptest.NewUnstartedServer(j.sec.middleware(mux))
	if tls {
		srv.StartTLS()
		j.pin = tlspin.Fingerprint(srv.Certificate())
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return &joinLab{srv: srv, j: j, log: &logs, bin: bin, join: orbitv1connect.NewJoinServiceClient(srv.Client(), srv.URL)}
}

func (l *joinLab) create(t *testing.T, name string, mgmt string) *orbitv1.CreateJoinTokenResponse {
	t.Helper()
	spec := &orbitv1.JoinSpec{UePools: joinSpec.UePools, UpfN6: joinSpec.UpfN6, N6Subnet: joinSpec.N6Subnet, ManagementAddress: mgmt}
	res, err := l.join.CreateJoinToken(context.Background(), connect.NewRequest(&orbitv1.CreateJoinTokenRequest{Name: name, Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func (l *joinLab) get(t *testing.T, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, l.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := l.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// CreateJoinToken: TLS required; the secret once, with the URL and the
// pinned one-liner; the management address given, else the server's
// listen address unless that is a wildcard or loopback; refusals for a bad
// spec, a taken name and a TTL past the hour.
func TestCreateJoinToken(t *testing.T) {
	t.Run("refused without TLS", func(t *testing.T) {
		l := newJoinLab(t, false, "192.0.2.10:8412")
		_, err := l.join.CreateJoinToken(context.Background(), connect.NewRequest(&orbitv1.CreateJoinTokenRequest{Spec: joinSpec}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), msgJoinNeedsTLS) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("refused over a plaintext connection to a TLS server", func(t *testing.T) {
		// TLS configured, but this request arrives in the clear (a second,
		// plaintext listener, or a TLS-terminating proxy): its response
		// would carry the token.
		l := newJoinLab(t, false, "192.0.2.10:8412")
		l.j.tls = true
		_, err := l.join.CreateJoinToken(context.Background(), connect.NewRequest(&orbitv1.CreateJoinTokenRequest{Spec: joinSpec}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), msgJoinNeedsTLS) {
			t.Fatalf("err = %v", err)
		}
		if len(l.j.store.List()) != 0 {
			t.Error("a token was issued")
		}
	})
	l := newJoinLab(t, true, "192.0.2.10:8412")
	res := l.create(t, "app1", "")
	curlPin := "sha256//" + strings.TrimPrefix(l.j.pin, "sha256/")
	if !strings.HasPrefix(res.GetSecret(), join.Prefix) || res.GetUrl() != "https://192.0.2.10:8412/join/"+res.GetSecret() ||
		res.GetPin() != l.j.pin || res.GetIssued().GetSpec().GetManagementAddress() != "192.0.2.10:8412" {
		t.Errorf("response = %v", res)
	}
	want := "curl -fsS --proto '=https' -k --pinnedpubkey '" + curlPin + "' '" + res.GetUrl() + "' | sudo sh"
	if res.GetOneLiner() != want {
		t.Errorf("one-liner = %q, want %q", res.GetOneLiner(), want)
	}
	// The response's secrets are marked, so a log or archive masks them.
	masked := proto.Clone(res).(*orbitv1.CreateJoinTokenResponse)
	secrets.Mask(masked)
	if strings.Contains(masked.String(), res.GetSecret()) {
		t.Errorf("Mask left the secret in %v", masked)
	}

	tests := []struct {
		name     string
		listen   string
		req      *orbitv1.CreateJoinTokenRequest
		wantCode connect.Code
		wantErr  string
		wantMgmt string
	}{
		{name: "management with no port takes the server's", listen: "0.0.0.0:8412",
			req: &orbitv1.CreateJoinTokenRequest{Spec: withMgmt("ran1.lab")}, wantMgmt: "ran1.lab:8412"},
		{name: "a wildcard listen needs --mgmt", listen: "0.0.0.0:8412",
			req: &orbitv1.CreateJoinTokenRequest{Spec: joinSpec}, wantCode: connect.CodeInvalidArgument, wantErr: "give --mgmt"},
		{name: "a loopback listen needs --mgmt", listen: "127.0.0.1:8412",
			req: &orbitv1.CreateJoinTokenRequest{Spec: joinSpec}, wantCode: connect.CodeInvalidArgument, wantErr: "give --mgmt"},
		{name: "a bad pool", listen: "192.0.2.10:8412",
			req:      &orbitv1.CreateJoinTokenRequest{Spec: &orbitv1.JoinSpec{UePools: []string{"10.0.0.1"}, UpfN6: "198.51.100.1", N6Subnet: "198.51.100.0/24"}},
			wantCode: connect.CodeInvalidArgument, wantErr: "not a CIDR"},
		{name: "past an hour", listen: "192.0.2.10:8412",
			req: &orbitv1.CreateJoinTokenRequest{Spec: joinSpec, TtlSeconds: 3601}, wantCode: connect.CodeInvalidArgument, wantErr: "at most 1h"},
		{name: "a bad name", listen: "192.0.2.10:8412",
			req: &orbitv1.CreateJoinTokenRequest{Spec: joinSpec, Name: "a b"}, wantCode: connect.CodeInvalidArgument, wantErr: "name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newJoinLab(t, true, tc.listen)
			res, err := l.join.CreateJoinToken(context.Background(), connect.NewRequest(tc.req))
			if tc.wantCode != 0 {
				if connect.CodeOf(err) != tc.wantCode || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %s with %q", err, tc.wantCode, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Msg.GetIssued().GetSpec().GetManagementAddress(); got != tc.wantMgmt {
				t.Errorf("management = %q, want %q", got, tc.wantMgmt)
			}
		})
	}
	t.Run("a taken name", func(t *testing.T) {
		_, err := l.join.CreateJoinToken(context.Background(), connect.NewRequest(&orbitv1.CreateJoinTokenRequest{Name: "app1", Spec: joinSpec}))
		if connect.CodeOf(err) != connect.CodeAlreadyExists {
			t.Fatalf("err = %v", err)
		}
	})
}

func withMgmt(m string) *orbitv1.JoinSpec {
	return &orbitv1.JoinSpec{UePools: joinSpec.UePools, UpfN6: joinSpec.UpfN6, N6Subnet: joinSpec.N6Subnet, ManagementAddress: m}
}

// List shows pending tokens and never a secret; revoke cancels by ID.
func TestListRevokeJoinTokens(t *testing.T) {
	l := newJoinLab(t, true, "192.0.2.10:8412")
	res := l.create(t, "app1", "")
	list, err := l.join.ListJoinTokens(context.Background(), connect.NewRequest(&orbitv1.ListJoinTokensRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Msg.GetTokens()) != 1 || list.Msg.GetTokens()[0].GetId() != res.GetIssued().GetId() ||
		strings.Contains(list.Msg.String(), res.GetSecret()) {
		t.Fatalf("list = %v", list.Msg)
	}
	if _, err := l.join.RevokeJoinToken(context.Background(), connect.NewRequest(&orbitv1.RevokeJoinTokenRequest{Id: res.GetIssued().GetId()})); err != nil {
		t.Fatal(err)
	}
	if _, err := l.join.RevokeJoinToken(context.Background(), connect.NewRequest(&orbitv1.RevokeJoinTokenRequest{Id: res.GetIssued().GetId()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a second revoke: %v", err)
	}
	if r, _ := l.get(t, http.MethodGet, "/join/"+res.GetSecret()); r.StatusCode != http.StatusNotFound {
		t.Errorf("a revoked token's script: %d", r.StatusCode)
	}
}

// The endpoints: over TLS only; the token only as the exact path segment;
// a uniform 404 for an unknown, expired or revoked token and for any other
// path; GET or HEAD; the script and the binary with its SHA-256; fetching
// does not use a token up; the token never in the log.
func TestJoinEndpoints(t *testing.T) {
	t.Run("plaintext", func(t *testing.T) {
		l := newJoinLab(t, false, "192.0.2.10:8412")
		if r, body := l.get(t, http.MethodGet, "/join/"+join.Prefix+"AAAAAAAAAAAAAAAAAAAAAA"); r.StatusCode != http.StatusNotFound || !strings.Contains(body, msgJoinNeedsTLS) {
			t.Errorf("plaintext: %d %q", r.StatusCode, body)
		}
	})
	l := newJoinLab(t, true, "192.0.2.10:8412")
	res := l.create(t, "app1", "")
	secret := res.GetSecret()
	sum := sha256.Sum256(l.bin)

	// The script, twice: fetching does not use the token up.
	for i := 0; i < 2; i++ {
		r, body := l.get(t, http.MethodGet, "/join/"+secret)
		if r.StatusCode != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/x-shellscript") ||
			r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("script %d: %d %v", i, r.StatusCode, r.Header)
		}
		for _, w := range []string{"set -eu", hex.EncodeToString(sum[:]), "sha256//" + strings.TrimPrefix(l.j.pin, "sha256/"), secret} {
			if !strings.Contains(body, w) {
				t.Errorf("script lacks %q", w)
			}
		}
	}
	if r, body := l.get(t, http.MethodHead, "/join/"+secret); r.StatusCode != http.StatusOK || body != "" || r.ContentLength <= 0 {
		t.Errorf("HEAD: %d, %d bytes of body, length %d", r.StatusCode, len(body), r.ContentLength)
	}
	r, body := l.get(t, http.MethodGet, "/join/"+secret+"/bin/amd64")
	got := sha256.Sum256([]byte(body))
	if r.StatusCode != http.StatusOK || body != string(l.bin) || r.Header.Get("X-Orbit-SHA256") != hex.EncodeToString(got[:]) {
		t.Errorf("binary: %d, sha header %s", r.StatusCode, r.Header.Get("X-Orbit-SHA256"))
	}
	if r, body := l.get(t, http.MethodHead, "/join/"+secret+"/bin/amd64"); r.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD binary: %d, %d bytes", r.StatusCode, len(body))
	}

	if r, _ := l.get(t, http.MethodPost, "/join/"+secret); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", r.StatusCode)
	}

	// Uniform 404s, each counted toward the peer's lockout (a fresh lab,
	// so the earlier requests do not count).
	l2 := newJoinLab(t, true, "192.0.2.10:8412")
	sec2 := l2.create(t, "", "").GetSecret()
	_, wantBody := l2.get(t, http.MethodGet, "/join/"+join.Prefix+"AAAAAAAAAAAAAAAAAAAAAA")
	for _, path := range []string{
		"/join/" + join.Prefix + "BBBBBBBBBBBBBBBBBBBBBB",
		"/join/?token=" + sec2,
		"/join/" + sec2 + "?token=" + sec2,
		"/join/" + sec2 + "/",
		"/join/" + sec2 + "/extra",
		"/join/" + sec2 + "/bin/amd64/x",
		"/join/" + sec2 + "/bin",
		"/join/",
	} {
		if r, body := l2.get(t, http.MethodGet, path); r.StatusCode != http.StatusNotFound || body != wantBody {
			t.Errorf("%s: %d %q, want the uniform 404 %q", strings.ReplaceAll(path, sec2, "<token>"), r.StatusCode, body, wantBody)
		}
	}
	// Nine misses so far: one more locks the peer out, the right token
	// included.
	l2.get(t, http.MethodGet, "/join/nope")
	if r, _ := l2.get(t, http.MethodGet, "/join/"+sec2); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after ten misses: %d, want 429", r.StatusCode)
	}

	// A valid token with an arch the server has no binary for: the body
	// of an unknown token's 404, byte for byte, and a miss toward the
	// same lockout, after the same number of tries (a fresh lab).
	l5 := newJoinLab(t, true, "192.0.2.10:8412")
	sec5 := l5.create(t, "", "").GetSecret()
	for i := 0; i < authFailLimit; i++ {
		arch := []string{"mips", "arm64", "riscv64", ""}[i%4]
		if r, body := l5.get(t, http.MethodGet, "/join/"+sec5+"/bin/"+arch); r.StatusCode != http.StatusNotFound || body != wantBody {
			t.Fatalf("bad arch %q (try %d): %d %q, want the uniform 404 %q", arch, i+1, r.StatusCode, body, wantBody)
		}
	}
	if r, _ := l5.get(t, http.MethodGet, "/join/"+sec5); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after %d bad-arch fetches: %d, want the lockout's 429, as after %d unknown tokens", authFailLimit, r.StatusCode, authFailLimit)
	}
	if !strings.Contains(l5.log.String(), "arch=mips") {
		t.Errorf("the log does not say which arch:\n%s", l5.log.String())
	}

	// Percent-encoding anywhere under /join/: the path as sent must be the
	// path as decoded, so an encoded '/', '.' or token character is the
	// same 404, the token's own path included (a fresh lab: own misses).
	l4 := newJoinLab(t, true, "192.0.2.10:8412")
	sec4 := l4.create(t, "", "").GetSecret()
	first := sec4[len(join.Prefix) : len(join.Prefix)+1]
	for _, path := range []string{
		"/join/" + sec4 + "%2Fbin%2Famd64",
		"/join/" + sec4 + "%2E",
		"/join/" + join.Prefix + "%" + strings.ToUpper(hex.EncodeToString([]byte(first))) + sec4[len(join.Prefix)+1:],
	} {
		if r, body := l4.get(t, http.MethodGet, path); r.StatusCode != http.StatusNotFound || body != wantBody {
			t.Errorf("%s: %d %q, want the uniform 404", strings.ReplaceAll(path, sec4, "<token>"), r.StatusCode, body)
		}
	}
	if r, _ := l4.get(t, http.MethodGet, "/join/"+sec4); r.StatusCode != http.StatusOK {
		t.Errorf("the token's own path after the encoded ones: %d", r.StatusCode)
	}

	// Expired: the same 404.
	l3 := newJoinLab(t, true, "192.0.2.10:8412")
	now := time.Now()
	l3.j.store = join.NewStore(join.WithClock(func() time.Time { return now }))
	sec3 := l3.create(t, "", "").GetSecret()
	now = now.Add(join.DefaultTTL)
	if r, body := l3.get(t, http.MethodGet, "/join/"+sec3); r.StatusCode != http.StatusNotFound || body != wantBody {
		t.Errorf("expired: %d %q", r.StatusCode, body)
	}

	// The token never reaches the log: only its ID.
	for _, lab := range []*joinLab{l, l2, l3, l4, l5} {
		for _, s := range []string{secret, sec2, sec3, sec4, sec5} {
			if strings.Contains(lab.log.String(), strings.TrimPrefix(s, join.Prefix)) {
				t.Errorf("the log holds a token:\n%s", lab.log.String())
			}
		}
	}
	if !strings.Contains(l.log.String(), res.GetIssued().GetId()) {
		t.Errorf("the log does not name the token's ID:\n%s", l.log.String())
	}
}

// The per-peer cap: 30 requests a minute, script fetches included.
func TestJoinPeerCap(t *testing.T) {
	l := newJoinLab(t, true, "192.0.2.10:8412")
	secret := l.create(t, "", "").GetSecret()
	for i := 0; i < joinPeerLimit; i++ {
		if r, _ := l.get(t, http.MethodHead, "/join/"+secret); r.StatusCode != http.StatusOK {
			t.Fatalf("request %d: %d", i, r.StatusCode)
		}
	}
	if r, _ := l.get(t, http.MethodGet, "/join/"+secret); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("request %d: %d, want 429", joinPeerLimit+1, r.StatusCode)
	}
}

// The server-wide caps on renders and binary downloads refuse at once,
// without waiting, when full.
func TestJoinConcurrencyCaps(t *testing.T) {
	l := newJoinLab(t, true, "192.0.2.10:8412")
	secret := l.create(t, "", "").GetSecret()
	for i := 0; i < joinMaxRenders; i++ {
		l.j.renders <- struct{}{}
	}
	if r, _ := l.get(t, http.MethodGet, "/join/"+secret); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("renders full: %d", r.StatusCode)
	}
	for i := 0; i < joinMaxBinaries; i++ {
		l.j.binaries <- struct{}{}
	}
	if r, _ := l.get(t, http.MethodGet, "/join/"+secret+"/bin/amd64"); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("binaries full: %d", r.StatusCode)
	}
}

// The one-liner's curl, pinned to the server's key, fetches the script and
// the binary; a different pin is refused before anything is read. Runs
// only where curl is installed.
func TestJoinCurlPinned(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	l := newJoinLab(t, true, "192.0.2.10:8412")
	secret := l.create(t, "", "").GetSecret()
	pin := "sha256//" + strings.TrimPrefix(l.j.pin, "sha256/")
	fetch := func(pin, path string) (string, error) {
		out, err := exec.Command(curl, "-fsS", "--proto", "=https", "-k", "--pinnedpubkey", pin, l.srv.URL+path).Output()
		return string(out), err
	}
	script, err := fetch(pin, "/join/"+secret)
	if err != nil || !strings.HasPrefix(script, "#!/bin/sh") {
		t.Fatalf("script: %v\n%s", err, script)
	}
	if bin, err := fetch(pin, "/join/"+secret+"/bin/amd64"); err != nil || bin != string(l.bin) {
		t.Errorf("binary: %v", err)
	}
	wrong := "sha256//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="
	if out, err := fetch(wrong, "/join/"+secret); err == nil || out != "" {
		t.Errorf("a wrong pin fetched %q", out)
	}
}
