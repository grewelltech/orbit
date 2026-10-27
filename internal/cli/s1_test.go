package cli

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
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
	loose := tokenFileOf(t, s1Token, 0o644)
	state := t.TempDir()
	// A certificate and key in files, for --tls-cert/--tls-key.
	pairDir := filepath.Join(t.TempDir(), "pair")
	if _, _, err := tlspin.SelfSigned(pairDir, []string{"localhost"}); err != nil {
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
	pair, leaf, err := tlspin.SelfSigned(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", observability.NewRegistry(), server.Options{Token: token, TLS: true})
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
