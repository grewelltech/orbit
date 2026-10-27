package cli

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/server"
	"github.com/bgrewell/orbit/internal/tlspin"
)

const s1Token = "0123456789abcdef0123456789abcdef"

func tokenFileOf(t *testing.T, token string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(token+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// serve's rules, settled before anything binds: loopback needs nothing;
// any other listener needs TLS unless --insecure-listen and a token unless
// --no-auth.
func TestServeSecurityMatrix(t *testing.T) {
	good := tokenFileOf(t, s1Token, 0o600)
	short := tokenFileOf(t, "short", 0o600)
	samey := tokenFileOf(t, strings.Repeat("orbit", 8), 0o600)     // 4 distinct
	varied := tokenFileOf(t, strings.Repeat("abcdefgh", 4), 0o600) // exactly 8
	loose := tokenFileOf(t, s1Token, 0o644)
	state := t.TempDir()
	// A certificate and key in files, for --tls-cert/--tls-key.
	pairDir := filepath.Join(t.TempDir(), "pair")
	if _, err := tlspin.SelfSigned(pairDir, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	cert, key := filepath.Join(pairDir, "cert.pem"), filepath.Join(pairDir, "key.pem")
	type row struct {
		name, listen, tls, cert, key, token, state string
		noAuth, insecure                           bool
		wantErr                                    string // "" = starts
		wantTLS, wantToken                         bool
	}
	rows := []row{
		{name: "loopback v4, plaintext, no token", listen: "127.0.0.1:8412"},
		{name: "loopback v6, plaintext, no token", listen: "[::1]:8412"},
		{name: "localhost", listen: "localhost:8412"},
		{name: "loopback with TLS and a token", listen: "127.0.0.1:8412", tls: "auto", state: state, token: good, wantTLS: true, wantToken: true},
		{name: "all interfaces, plaintext", listen: "0.0.0.0:8412", token: good, wantErr: "is not loopback: serve it with TLS"},
		{name: "empty host, plaintext", listen: ":8412", token: good, wantErr: "is not loopback: serve it with TLS"},
		{name: "::, plaintext", listen: "[::]:8412", token: good, wantErr: "is not loopback: serve it with TLS"},
		{name: "LAN, TLS, no token", listen: "192.0.2.1:8412", tls: "auto", state: state, wantErr: "give a bearer token (--token-file), or pass --no-auth"},
		{name: "LAN, TLS auto, token", listen: "192.0.2.1:8412", tls: "auto", state: state, token: good, wantTLS: true, wantToken: true},
		{name: "LAN, cert files, token", listen: "192.0.2.1:8412", cert: cert, key: key, token: good, wantTLS: true, wantToken: true},
		{name: "LAN, TLS, --no-auth", listen: "192.0.2.1:8412", tls: "auto", state: state, noAuth: true, wantTLS: true},
		{name: "LAN, --insecure-listen, token", listen: "192.0.2.1:8412", insecure: true, token: good, wantToken: true},
		{name: "LAN, --insecure-listen, no token", listen: "192.0.2.1:8412", insecure: true, wantErr: "give a bearer token"},
		{name: "LAN, --insecure-listen --no-auth", listen: "192.0.2.1:8412", insecure: true, noAuth: true},
		{name: "--tls auto and cert files", listen: "127.0.0.1:8412", tls: "auto", state: state, cert: cert, key: key, wantErr: "alternatives"},
		{name: "cert without key", listen: "127.0.0.1:8412", cert: cert, wantErr: "go together"},
		{name: "--tls bogus", listen: "127.0.0.1:8412", tls: "yes", wantErr: "the only value is auto"},
		{name: "--tls auto without a state dir", listen: "127.0.0.1:8412", tls: "auto", wantErr: "keeps its certificate in --state-dir"},
		{name: "a short token", listen: "127.0.0.1:8412", token: short, wantErr: "at least 32 characters"},
		{name: "a token with too little variety", listen: "127.0.0.1:8412", token: samey, wantErr: "too little variety; generate one with `openssl rand -hex 32`"},
		{name: "a token with just enough variety", listen: "127.0.0.1:8412", token: varied, wantToken: true},
		{name: "a token others can read", listen: "127.0.0.1:8412", token: loose, wantErr: "readable by others"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			sec, err := resolveServeSecurity(r.listen, r.tls, r.cert, r.key, r.token, r.state, r.noAuth, r.insecure)
			if r.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), r.wantErr) {
					t.Fatalf("err = %v, want %q", err, r.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (sec.tls != nil) != r.wantTLS || (sec.token != "") != r.wantToken {
				t.Errorf("tls %t token %t", sec.tls != nil, sec.token != "")
			}
		})
	}
	// --tls auto reuses its certificate across restarts.
	a, _ := resolveServeSecurity("127.0.0.1:8412", "auto", "", "", "", state, false, false)
	b, _ := resolveServeSecurity("127.0.0.1:8412", "auto", "", "", "", state, false, false)
	if tlspin.Fingerprint(a.leaf) != tlspin.Fingerprint(b.leaf) {
		t.Error("--tls auto made a new key on restart")
	}
	if st, err := os.Stat(filepath.Join(state, "tls", "key.pem")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v %v", err, st)
	}
}

// tlsServer serves the ORBIT API over TLS with a --tls auto certificate,
// with token (may be ""), and returns its https URL and pin.
func tlsServer(t *testing.T, token string) (string, string) {
	t.Helper()
	p, err := tlspin.SelfSigned(t.TempDir(), []string{"127.0.0.1"})
	pair, leaf := p.Cert, p.Leaf
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", observability.NewRegistry(), server.Options{Token: token})
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, tlspin.Fingerprint(leaf)
}

// clientConfig points the client's servers.yaml at a scratch directory.
func clientConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("ORBIT_TOKEN", "")
	_ = os.Unsetenv("ORBIT_TOKEN")
	return filepath.Join(dir, "orbit", "servers.yaml")
}

func writeServersFile(t *testing.T, path string, entries ...serverEntry) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeServers(path, serversFile{Servers: entries}); err != nil {
		t.Fatal(err)
	}
}

// ADR-0008 §4 end to end: a pinned TLS server with a token. The pin and
// token from servers.yaml work; a missing token is unauthenticated; a wrong
// pin is refused naming both fingerprints; no pin against a self-signed
// certificate is refused; $ORBIT_TOKEN wins over servers.yaml.
func TestPinnedTLSWithToken(t *testing.T) {
	path := clientConfig(t)
	url, pin := tlsServer(t, s1Token)
	info := func() error {
		_, err := systemClient(&url).GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{}))
		return err
	}
	writeServersFile(t, path, serverEntry{URL: url, Pin: pin, Token: s1Token})
	if err := info(); err != nil {
		t.Fatalf("pinned with token: %v", err)
	}
	writeServersFile(t, path, serverEntry{URL: url, Pin: pin})
	if err := info(); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("no token: %v", err)
	}
	t.Setenv("ORBIT_TOKEN", s1Token)
	if err := info(); err != nil {
		t.Errorf("$ORBIT_TOKEN: %v", err)
	}
	writeServersFile(t, path, serverEntry{URL: url, Pin: pin, Token: strings.Repeat("z", 32)})
	if err := info(); err != nil {
		t.Errorf("$ORBIT_TOKEN over a wrong servers.yaml token: %v", err)
	}
	other := "sha256/" + strings.Repeat("A", 43) + "="
	writeServersFile(t, path, serverEntry{URL: url, Pin: other, Token: s1Token})
	if err := info(); err == nil || !strings.Contains(err.Error(), pin) || !strings.Contains(err.Error(), other) {
		t.Errorf("wrong pin: %v, want both fingerprints", err)
	}
	writeServersFile(t, path)
	if err := info(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("no pin against a self-signed certificate: %v", err)
	}
}

// Condition (A): the CLI refuses, before sending anything, subscriber keys
// or a token over http:// to a host that is not loopback. Loopback and
// https are fine. No override.
func TestClientRefusesPlaintextSecrets(t *testing.T) {
	clientConfig(t)
	loop := httptest.NewServer(server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", observability.NewRegistry(), server.Options{}))
	t.Cleanup(loop.Close)
	creds := &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff", Opc: "000102030405060708090a0b0c0d0e0f"}
	register := func(url string, keys bool) error {
		req := &orbitv1.RegisterRequest{Supi: "001010000000001", AmfAddress: "127.0.0.1:1"}
		if keys {
			req.Credentials = creds
		}
		// The rows that get past the guard dial an unroutable address: a
		// short deadline keeps them quick.
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := ueClient(&url).Register(ctx, connect.NewRequest(req))
		return err
	}
	refused := func(err error) bool { return err != nil && strings.Contains(err.Error(), "refusing to send") }
	for _, tc := range []struct {
		name, url   string
		keys, token bool
		wantRefusal bool
	}{
		{name: "loopback, keys", url: loop.URL, keys: true},
		{name: "loopback, token", url: loop.URL, token: true},
		{name: "loopback, keys and token", url: loop.URL, keys: true, token: true},
		{name: "off-host http, keys", url: "http://192.0.2.1:1", keys: true, wantRefusal: true},
		{name: "off-host http, token", url: "http://192.0.2.1:1", token: true, wantRefusal: true},
		{name: "off-host http, keys and token", url: "http://192.0.2.1:1", keys: true, token: true, wantRefusal: true},
		{name: "off-host http by name, keys", url: "http://ran1.example:8412", keys: true, wantRefusal: true},
		{name: "off-host http, neither", url: "http://192.0.2.1:1"},
		{name: "off-host https, keys and token", url: "https://192.0.2.1:1", keys: true, token: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.token {
				t.Setenv("ORBIT_TOKEN", s1Token)
			} else {
				_ = os.Unsetenv("ORBIT_TOKEN")
			}
			err := register(tc.url, tc.keys)
			if refused(err) != tc.wantRefusal {
				t.Errorf("err = %v, want refusal %t", err, tc.wantRefusal)
			}
			if tc.wantRefusal && strings.Contains(err.Error(), s1Token) {
				t.Error("the refusal shows the token")
			}
		})
	}
}

// server trust pins after the fingerprint is confirmed: --fingerprint must
// match, and with no terminal and no --fingerprint nothing is trusted.
// servers.yaml is 0600 and never written through a link; list masks the
// token; forget removes it.
func TestServerTrust(t *testing.T) {
	path := clientConfig(t)
	url, pin := tlsServer(t, s1Token)
	loc := targetDir(t, lab1)
	r := runOrbit(t, loc, "server", "trust", url)
	if r.execErr == nil || !strings.Contains(r.execErr.Error(), "no terminal to confirm on") || !strings.Contains(r.execErr.Error(), pin) {
		t.Errorf("no terminal, no --fingerprint: %v", r.execErr)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an unconfirmed trust wrote servers.yaml")
	}
	if r := runOrbit(t, loc, "server", "trust", url, "--fingerprint", "sha256/"+strings.Repeat("A", 43)+"="); r.execErr == nil || !strings.Contains(r.execErr.Error(), "not trusted") {
		t.Errorf("a wrong --fingerprint: %v", r.execErr)
	}
	tok := tokenFileOf(t, s1Token, 0o600)
	r = runOrbit(t, loc, "server", "trust", url, "--fingerprint", pin, "--token-file", tok)
	if r.execErr != nil || !strings.Contains(r.out, "fingerprint: "+pin) {
		t.Fatalf("trust: %v\n%s", r.execErr, r.out)
	}
	if !strings.Contains(r.out, "token: from --token-file "+tok) {
		t.Errorf("trust does not say where the token came from:\n%s", r.out)
	}
	// Re-pinning without a token keeps the old one, and says so; with
	// $ORBIT_TOKEN set, that is the token kept, and it says so.
	_ = os.Unsetenv("ORBIT_TOKEN")
	if r := runOrbit(t, loc, "server", "trust", url, "--fingerprint", pin); !strings.Contains(r.out, "token: kept from the previous pin") {
		t.Errorf("re-pin: %v\n%s", r.execErr, r.out)
	}
	t.Setenv("ORBIT_TOKEN", s1Token)
	if r := runOrbit(t, loc, "server", "trust", url, "--fingerprint", pin); !strings.Contains(r.out, "token: from $ORBIT_TOKEN") {
		t.Errorf("trust with $ORBIT_TOKEN: %v\n%s", r.execErr, r.out)
	}
	_ = os.Unsetenv("ORBIT_TOKEN")
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("servers.yaml: %v %v", err, st)
	}
	// The pinned server now answers through the CLI.
	if r := runOrbit(t, loc, "--server", url, "system", "info"); r.execErr != nil {
		t.Errorf("system info through the pin: %v\n%s", r.execErr, r.out)
	}
	r = runOrbit(t, loc, "server", "list")
	if !strings.Contains(r.out, pin) || !strings.Contains(r.out, "<redacted>") || strings.Contains(r.out, s1Token) {
		t.Errorf("list:\n%s", r.out)
	}
	if r := runOrbit(t, loc, "server", "forget", url); r.execErr != nil {
		t.Fatal(r.execErr)
	}
	if f, _ := loadServers(path); len(f.Servers) != 0 {
		t.Errorf("forget left %+v", f.Servers)
	}
	// A symlinked servers.yaml is refused.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if r := runOrbit(t, loc, "server", "trust", url, "--fingerprint", pin); r.execErr == nil || !strings.Contains(r.execErr.Error(), "is a symlink") {
		t.Errorf("trust through a symlink: %v", r.execErr)
	}
	// A servers.yaml holding a token that others can read is refused.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("servers:\n  - { url: "+url+", token: "+s1Token+" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runOrbit(t, loc, "--server", url, "system", "info"); r.execErr == nil || !strings.Contains(r.execErr.Error(), "readable by others") {
		t.Errorf("a loose servers.yaml: %v", r.execErr)
	}
}

// The API client follows no redirect, same-origin or not, so neither the
// request (keys included) nor the token goes anywhere but the configured
// server; and the transport sends the token only to that server's origin.
func TestClientFollowsNoRedirect(t *testing.T) {
	clientConfig(t)
	t.Setenv("ORBIT_TOKEN", s1Token)
	type hit struct{ path, auth string }
	record := func(hits *[]hit, mu *sync.Mutex, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*hits = append(*hits, hit{r.URL.Path, r.Header.Get("Authorization")})
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
	var mu sync.Mutex
	var otherHits, selfHits []hit
	api := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", observability.NewRegistry(), server.Options{Token: s1Token})
	other := httptest.NewServer(record(&otherHits, &mu, api))
	t.Cleanup(other.Close)
	var self *httptest.Server
	self = httptest.NewServer(record(&selfHits, &mu, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("to") {
		case "other":
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		case "self":
			http.Redirect(w, r, self.URL+"/moved"+r.URL.Path, http.StatusTemporaryRedirect)
		default:
			api.ServeHTTP(w, r)
		}
	})))
	t.Cleanup(self.Close)

	for _, tc := range []struct {
		name, base  string
		wantErr     string
		wantSelf    int // requests the configured server sees
		wantOther   int
		wantSelfTok bool
	}{
		{name: "cross-origin 307", base: self.URL + "?to=other", wantErr: "server redirected to " + other.URL + "; refusing", wantSelf: 1, wantSelfTok: true},
		{name: "same-origin 307", base: self.URL + "?to=self", wantErr: "server redirected to " + self.URL + "; refusing", wantSelf: 1, wantSelfTok: true},
		{name: "loopback to loopback, no redirect", base: self.URL, wantSelf: 1, wantSelfTok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			otherHits, selfHits = nil, nil
			mu.Unlock()
			// The query rides on every procedure URL Connect builds from base.
			h, guard := apiClient(strings.SplitN(tc.base, "?", 2)[0])
			cl := orbitv1connect.NewSystemServiceClient(&http.Client{Transport: queryTransport{h, tc.base}, CheckRedirect: h.CheckRedirect}, strings.SplitN(tc.base, "?", 2)[0], guard)
			_, err := cl.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{}))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(selfHits) != tc.wantSelf || len(otherHits) != tc.wantOther {
				t.Fatalf("requests: configured server %v, other %v; want %d and %d", selfHits, otherHits, tc.wantSelf, tc.wantOther)
			}
			if got := selfHits[0].auth == "Bearer "+s1Token; got != tc.wantSelfTok {
				t.Errorf("configured server got Authorization %q", selfHits[0].auth)
			}
		})
	}

	// The transport itself: a request to any origin but the configured one
	// carries no token.
	h, _ := apiClient(self.URL)
	for _, u := range []string{other.URL + "/healthz", strings.Replace(self.URL, "127.0.0.1", "localhost", 1) + "/healthz"} {
		mu.Lock()
		otherHits, selfHits = nil, nil
		mu.Unlock()
		resp, err := h.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		mu.Lock()
		for _, x := range append(otherHits, selfHits...) {
			if x.auth != "" {
				t.Errorf("GET %s carried Authorization %q", u, x.auth)
			}
		}
		mu.Unlock()
	}
}

// queryTransport adds the base URL's query to each request, so one test
// server can be told what to answer.
type queryTransport struct {
	c    *http.Client
	base string
}

func (q queryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if i := strings.Index(q.base, "?"); i >= 0 && r.URL.RawQuery == "" {
		r = r.Clone(r.Context())
		r.URL.RawQuery = q.base[i+1:]
	}
	return q.c.Transport.RoundTrip(r)
}

// A plaintext serve speaks HTTP/1.1 and unencrypted HTTP/2 (prior
// knowledge, as gRPC clients use); a TLS one leaves HTTP/2 to ALPN.
func TestNewHTTPServerProtocols(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if srv := newHTTPServer("127.0.0.1:0", ok, &tls.Config{MinVersion: tls.VersionTLS12}); srv.Protocols != nil {
		t.Errorf("a TLS server sets Protocols %v", srv.Protocols)
	}
	srv := newHTTPServer("127.0.0.1:0", ok, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	for _, tc := range []struct {
		name  string
		set   func(*http.Protocols)
		major int
	}{
		{name: "HTTP/1.1", set: func(p *http.Protocols) { p.SetHTTP1(true) }, major: 1},
		{name: "unencrypted HTTP/2", set: func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) }, major: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p http.Protocols
			tc.set(&p)
			c := &http.Client{Transport: &http.Transport{Protocols: &p}}
			resp, err := c.Get("http://" + ln.Addr().String() + "/")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != tc.major {
				t.Errorf("got %d over %s, want 200 over HTTP/%d", resp.StatusCode, resp.Proto, tc.major)
			}
		})
	}
}
