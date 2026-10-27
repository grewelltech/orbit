package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
)

const testToken = "0123456789abcdef0123456789abcdef"

// secureServer is a server with token (may be ""), every peer seen as
// loopback or not, plaintext or TLS.
func secureServer(t *testing.T, token string, loopback, tls bool) (*httptest.Server, *security) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sec := newSecurity(log, token)
	sec.loopback = func(string) bool { return loopback }
	h := newHandler(log, "test", prometheus.NewRegistry(), Options{TLS: tls}, sec)
	srv := httptest.NewUnstartedServer(h)
	if tls {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, sec
}

func bearer(token string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", token)
			}
			return next(ctx, req)
		}
	}))
}

func get(t *testing.T, srv *httptest.Server, path, auth string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// The token guards every procedure and /metrics; /healthz and the
// dashboard's static files stay open. A missing, wrong or malformed token
// is unauthenticated; the right one, any case of "Bearer", passes. No token
// configured: everything is open.
func TestAuth(t *testing.T) {
	tests := []struct {
		name, token, auth string
		want              connect.Code // 0 = ok
	}{
		{name: "no token configured", auth: ""},
		{name: "missing", token: testToken, auth: "", want: connect.CodeUnauthenticated},
		{name: "wrong", token: testToken, auth: "Bearer " + strings.Repeat("x", 32), want: connect.CodeUnauthenticated},
		{name: "a prefix of the token", token: testToken, auth: "Bearer " + testToken[:31], want: connect.CodeUnauthenticated},
		{name: "not Bearer", token: testToken, auth: "Basic " + testToken, want: connect.CodeUnauthenticated},
		{name: "right", token: testToken, auth: "Bearer " + testToken},
		{name: "right, lower-case scheme", token: testToken, auth: "bearer " + testToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := secureServer(t, tc.token, true, false)
			c := orbitv1connect.NewSystemServiceClient(srv.Client(), srv.URL, bearer(tc.auth))
			_, err := c.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{}))
			if got := connect.CodeOf(err); (err == nil) != (tc.want == 0) || (err != nil && got != tc.want) {
				t.Errorf("GetInfo: err = %v, want code %v", err, tc.want)
			}
			metrics := get(t, srv, "/metrics", tc.auth)
			if (metrics == http.StatusOK) != (tc.want == 0) {
				t.Errorf("/metrics = %d", metrics)
			}
			if c := get(t, srv, "/healthz", ""); c != http.StatusOK {
				t.Errorf("/healthz = %d, want open", c)
			}
			if c := get(t, srv, "/", ""); c == http.StatusUnauthorized {
				t.Errorf("the dashboard's static files need a token")
			}
		})
	}
}

// After authFailLimit failures within the window a peer is refused, the
// right token included, until the lockout ends.
func TestAuthLockout(t *testing.T) {
	srv, sec := secureServer(t, testToken, true, false)
	now := time.Unix(1000, 0)
	sec.now = func() time.Time { return now }
	bad := orbitv1connect.NewSystemServiceClient(srv.Client(), srv.URL, bearer("Bearer nope"))
	good := orbitv1connect.NewSystemServiceClient(srv.Client(), srv.URL, bearer("Bearer "+testToken))
	for i := 0; i < authFailLimit; i++ {
		if _, err := bad.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	if _, err := good.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{})); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("locked out: err = %v, want resource_exhausted", err)
	}
	now = now.Add(authLockout + time.Second)
	if _, err := good.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{})); err != nil {
		t.Errorf("after the lockout: %v", err)
	}
	// Failures spread out beyond the window never lock a peer out.
	for i := 0; i < 2*authFailLimit; i++ {
		now = now.Add(authFailWindow / 2)
		_, _ = bad.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{}))
		now = now.Add(authFailWindow)
	}
	if _, err := good.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{})); err != nil {
		t.Errorf("slow failures locked the peer out: %v", err)
	}
}

// A request carrying subscriber keys or a token is refused over plaintext
// from a non-loopback peer, and passes over TLS or from loopback; one
// without secrets passes either way.
func TestSecretsOverPlaintext(t *testing.T) {
	creds := &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff", Opc: "000102030405060708090a0b0c0d0e0f"}
	calls := []struct {
		name    string
		secrets bool
		call    func(ctx context.Context, h connect.HTTPClient, url string) error
	}{
		{name: "register with keys", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewUEServiceClient(h, url).Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{Supi: "001010000000001", Credentials: creds}))
			return err
		}},
		{name: "a load run with keys", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{
				Spec: &orbitv1.StartRunRequest_Load{Load: &orbitv1.LoadRunSpec{Credentials: creds}}}))
			return err
		}},
		{name: "keys inside a fleet scenario", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{
				Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{ScenarioYaml: "kind: fleet\ncredentials: { ki: 00112233445566778899aabbccddeeff }\n"}}}))
			return err
		}},
		{name: "a responder token", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewUEServiceClient(h, url).StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{Supi: "001010000000001", Token: "t"}))
			return err
		}},
		{name: "no secrets", call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewUEServiceClient(h, url).Register(ctx, connect.NewRequest(&orbitv1.RegisterRequest{Supi: "001010000000001"}))
			return err
		}},
	}
	for _, peer := range []struct {
		name          string
		loopback, tls bool
		refuse        bool // a call carrying secrets is refused
	}{
		{name: "loopback, plaintext", loopback: true},
		{name: "non-loopback, plaintext", refuse: true},
		{name: "non-loopback, TLS", tls: true},
	} {
		srv, _ := secureServer(t, "", peer.loopback, peer.tls)
		for _, c := range calls {
			t.Run(peer.name+"/"+c.name, func(t *testing.T) {
				err := c.call(context.Background(), srv.Client(), srv.URL)
				refused := connect.CodeOf(err) == connect.CodeFailedPrecondition && strings.Contains(err.Error(), "over plaintext from a non-loopback peer")
				if refused != (peer.refuse && c.secrets) {
					t.Errorf("err = %v, want refused: %t", err, peer.refuse && c.secrets)
				}
			})
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:5000": true, "127.9.9.9:1": true, "[::1]:5000": true, "[::ffff:127.0.0.1]:1": true,
		"192.0.2.1:5000": false, "[2001:db8::1]:1": false, "garbage": false} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%s) = %t", addr, got)
		}
	}
}
