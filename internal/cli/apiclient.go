package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/bgrewell/orbit/internal/secrets"
	"github.com/bgrewell/orbit/internal/tlspin"
)

// serverEntry is one server in ~/.config/orbit/servers.yaml: its URL, the
// SPKI pin `orbit server trust` recorded, and the bearer token to send.
type serverEntry struct {
	URL   string `yaml:"url"`
	Pin   string `yaml:"spki_sha256,omitempty"`
	Token string `yaml:"token,omitempty"`
}

type serversFile struct {
	Servers []serverEntry `yaml:"servers"`
}

// serversPath is ~/.config/orbit/servers.yaml.
func serversPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "orbit", "servers.yaml"), nil
}

// loadServers reads servers.yaml; a missing file is no servers. A file
// holding a token that others can read is refused, as serve refuses such a
// --token-file.
func loadServers(path string) (serversFile, error) {
	var f serversFile
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range f.Servers {
		if s.Token != "" {
			if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
				return f, fmt.Errorf("%s holds a token and is readable by others (mode %o); chmod 600 it", path, st.Mode().Perm())
			}
			break
		}
	}
	return f, nil
}

// serverKey is how servers.yaml entries match a URL: scheme, host and
// port, with the scheme's default port filled in.
func serverKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (f serversFile) lookup(raw string) (serverEntry, bool) {
	k := serverKey(raw)
	for _, s := range f.Servers {
		if serverKey(s.URL) == k {
			return s, true
		}
	}
	return serverEntry{}, false
}

// hostLoopback reports whether a URL's host is loopback: a loopback IP or
// localhost. Any other name counts as off the host, without resolving it.
func hostLoopback(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

// apiTransport is the CLI's HTTP transport to the API server: it pins a
// trusted server's key, sends the bearer token, and refuses to send the
// token over plaintext off the host (ADR-0008 §4).
type apiTransport struct {
	base    string // the server URL
	origin  string // serverKey(base): the only origin the token goes to
	once    sync.Once
	rt      http.RoundTripper
	token   string
	loadErr error
}

func (t *apiTransport) init() {
	path, err := serversPath()
	if err != nil {
		t.loadErr = err
		return
	}
	f, err := loadServers(path)
	if err != nil {
		t.loadErr = err
		return
	}
	entry, _ := f.lookup(t.base)
	t.token = entry.Token
	if v, ok := os.LookupEnv("ORBIT_TOKEN"); ok && strings.TrimSpace(v) != "" {
		t.token = strings.TrimSpace(v)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	if entry.Pin != "" {
		// A pinned server is verified by its key alone: --tls auto has no CA.
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: tlspin.VerifyPin(entry.Pin), MinVersion: tls.VersionTLS12}
	}
	t.rt = tr
}

// errPlaintext is the refusal to send a secret over http:// off the host.
func errPlaintext(u *url.URL, what string) error {
	return fmt.Errorf("refusing to send %s in plaintext to %s; use https:// (orbit serve --tls auto, then orbit server trust)", what, u.Host)
}

func (t *apiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.once.Do(t.init)
	if t.loadErr != nil {
		return nil, t.loadErr
	}
	if t.token != "" && serverKey(req.URL.String()) == t.origin {
		if req.URL.Scheme == "http" && !hostLoopback(req.URL) {
			return nil, errPlaintext(req.URL, "the bearer token")
		}
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.rt.RoundTrip(req)
}

// apiClient is the HTTP client and Connect options for the API server at
// base: the transport above, sending the token only to base's origin and
// following no redirect, and an interceptor that refuses, before
// anything is sent, a request carrying subscriber keys or tokens
// (secrets.Find) over http:// to a host that is not loopback. There is no
// override; the server refuses the same as a second line of defence.
func apiClient(base string) (*http.Client, connect.ClientOption) {
	u, err := url.Parse(base)
	if err != nil {
		u = &url.URL{Scheme: "http", Host: base}
	}
	return &http.Client{Transport: &apiTransport{base: base, origin: serverKey(base)}, CheckRedirect: refuseRedirect},
		connect.WithInterceptors(secretsGuard{base: u})
}

// refuseRedirect is the API client's redirect policy: none. A Connect call
// never legitimately redirects, and following one would carry the request
// (keys included) and the token's origin check somewhere else.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("server redirected to %s://%s; refusing", req.URL.Scheme, req.URL.Host)
}

// secretsGuard is the client-side plaintext refusal for request messages,
// for the server at base.
type secretsGuard struct{ base *url.URL }

func (g secretsGuard) check(msg any) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil
	}
	return checkOutgoing(g.base, m)
}

// checkOutgoing refuses a message carrying secrets bound for plaintext off
// the host.
func checkOutgoing(u *url.URL, m proto.Message) error {
	if u.Scheme == "https" || hostLoopback(u) {
		return nil
	}
	if found := secrets.Find(m); len(found) > 0 {
		return errPlaintext(u, "subscriber keys/tokens ("+strings.Join(found, ", ")+")")
	}
	return nil
}

func (g secretsGuard) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := g.check(req.Any()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (g secretsGuard) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		return &guardedConn{StreamingClientConn: next(ctx, spec), g: g}
	}
}

func (secretsGuard) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

type guardedConn struct {
	connect.StreamingClientConn
	g secretsGuard
}

func (c *guardedConn) Send(msg any) error {
	if err := c.g.check(msg); err != nil {
		return err
	}
	return c.StreamingClientConn.Send(msg)
}
