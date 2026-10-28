package server

import (
	"context"
	"fmt"
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
	h := newHandler(log, "test", prometheus.NewRegistry(), Options{}, sec)
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

// After authFailLimit wrong tokens within the window a peer is refused,
// the right token included, until the lockout ends; a request with no token
// is never counted.
func TestAuthLockout(t *testing.T) {
	srv, sec := secureServer(t, testToken, true, false)
	now := time.Unix(1000, 0)
	sec.now = func() time.Time { return now }
	bad := orbitv1connect.NewSystemServiceClient(srv.Client(), srv.URL, bearer("Bearer nope"))
	good := orbitv1connect.NewSystemServiceClient(srv.Client(), srv.URL, bearer("Bearer "+testToken))
	none := orbitv1connect.NewSystemServiceClient(srv.Client(), srv.URL)
	call := func(c orbitv1connect.SystemServiceClient) error {
		_, err := c.GetInfo(context.Background(), connect.NewRequest(&orbitv1.GetInfoRequest{}))
		return err
	}
	wantCode := func(step string, err error, code connect.Code, msg string) {
		t.Helper()
		if connect.CodeOf(err) != code || (msg != "" && !strings.Contains(err.Error(), msg)) {
			t.Errorf("%s: err = %v, want %v %q", step, err, code, msg)
		}
	}

	// A dashboard polling before its token is entered: no header, never
	// counted, however often.
	for i := 0; i < 5*authFailLimit; i++ {
		wantCode("no token", call(none), connect.CodeUnauthenticated, msgTokenRequired)
	}
	for i := 0; i < authFailLimit; i++ {
		wantCode("wrong token", call(bad), connect.CodeUnauthenticated, msgTokenInvalid)
	}
	wantCode("wrong token, locked out", call(bad), connect.CodeResourceExhausted, msgLockedOut)
	wantCode("the right token, locked out", call(good), connect.CodeResourceExhausted, msgLockedOut)
	wantCode("no token during the lockout", call(none), connect.CodeUnauthenticated, msgTokenRequired)

	now = now.Add(authLockout + time.Second)
	if err := call(good); err != nil {
		t.Errorf("the right token after the lockout: %v", err)
	}
	wantCode("wrong token after the lockout", call(bad), connect.CodeUnauthenticated, msgTokenInvalid)

	// Wrong tokens spread out beyond the window never lock a peer out.
	for i := 0; i < 2*authFailLimit; i++ {
		now = now.Add(authFailWindow + time.Second)
		_ = call(bad)
	}
	wantCode("slow wrong tokens", call(bad), connect.CodeUnauthenticated, msgTokenInvalid)
}

// A peer is counted by its IPv4 address, or by its IPv6 /64, so rotating
// through a /64 does not sidestep the lockout.
func TestPeerKey(t *testing.T) {
	tests := []struct{ remote, want string }{
		{"192.0.2.7:4000", "192.0.2.7"},
		{"[::ffff:192.0.2.7]:4000", "192.0.2.7"},
		{"[2001:db8:1:2:aaaa::1]:4000", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:bbbb::9]:4001", "2001:db8:1:2::/64"},
		{"[2001:db8:1:3::1]:4000", "2001:db8:1:3::/64"},
		{"[fe80::1%eth0]:4000", "fe80::/64"},
		{"not-an-address", "not-an-address"},
	}
	for _, tc := range tests {
		if got := peerKey(tc.remote); got != tc.want {
			t.Errorf("peerKey(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}

	sec := newSecurity(slog.New(slog.NewTextHandler(io.Discard, nil)), testToken)
	h := sec.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	try := func(remote, auth string) int {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		r.RemoteAddr = remote
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < authFailLimit; i++ {
		try(fmt.Sprintf("[2001:db8:1:2::%x]:4000", i+1), "Bearer wrong")
	}
	if c := try("[2001:db8:1:2::ffff]:4000", "Bearer wrong"); c != http.StatusTooManyRequests {
		t.Errorf("a fresh address in the locked /64 = %d, want 429", c)
	}
	if c := try("[2001:db8:1:3::1]:4000", "Bearer wrong"); c != http.StatusUnauthorized {
		t.Errorf("another /64 = %d, want 401", c)
	}
	if c := try("[2001:db8:1:2::ffff]:4000", "Bearer "+testToken); c != http.StatusTooManyRequests {
		t.Errorf("the right token from the locked /64 = %d, want 429", c)
	}
	if c := try("[2001:db8:1:3::1]:4000", "Bearer "+testToken); c != http.StatusOK {
		t.Errorf("the right token from another /64 = %d, want 200", c)
	}
}

// The failure and lockout tables stay bounded under address rotation.
func TestAuthTablesBounded(t *testing.T) {
	sec := newSecurity(slog.New(slog.NewTextHandler(io.Discard, nil)), testToken)
	now := time.Unix(1000, 0)
	sec.now = func() time.Time { return now }
	for i := 0; i < 3*authMaxPeers; i++ {
		peer := fmt.Sprintf("198.18.%d.%d", i/256, i%256)
		for j := 0; j < authFailLimit; j++ {
			sec.fail(peer, "/x")
		}
		sec.fail(fmt.Sprintf("10.%d.%d.1", i/256, i%256), "/x") // below the limit
	}
	if n, m := len(sec.failures), len(sec.locked); n > authMaxPeers || m > authMaxPeers {
		t.Errorf("tables grew to %d failures and %d lockouts, cap %d", n, m, authMaxPeers)
	}
	// Expired entries go first.
	now = now.Add(authLockout + authFailWindow + time.Second)
	sec.fail("192.0.2.1", "/x")
	sec.mu.Lock()
	sec.sweep(now)
	sec.mu.Unlock()
	if _, ok := sec.failures["192.0.2.1"]; !ok {
		t.Error("the sweep dropped a live entry")
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
				Spec: &orbitv1.StartRunRequest_Load{Load: &orbitv1.LoadRunSpec{Credentials: creds}}})) //nolint:staticcheck // the deprecated arm, kept for API clients
			return err
		}},
		{name: "keys inside a fleet scenario", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{
				Spec: &orbitv1.StartRunRequest_Fleet{Fleet: &orbitv1.FleetRunSpec{ScenarioYaml: "kind: fleet\ncredentials: { ki: 00112233445566778899aabbccddeeff }\n"}}})) //nolint:staticcheck // the deprecated arm, kept for API clients
			return err
		}},
		{name: "a responder token", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewUEServiceClient(h, url).StartApp(ctx, connect.NewRequest(&orbitv1.StartAppRequest{Supi: "001010000000001", Token: "t"}))
			return err
		}},
		{name: "a plan carrying subscribers[].ki", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return startPlan(ctx, h, url, &orbitv1.RunPlan{Target: &orbitv1.ResolvedTarget{Subscribers: []*orbitv1.SubscriberPool{{Name: "main", Ki: creds.Ki}}}})
		}},
		{name: "a plan carrying subscribers[].opc", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return startPlan(ctx, h, url, &orbitv1.RunPlan{Target: &orbitv1.ResolvedTarget{Subscribers: []*orbitv1.SubscriberPool{{Name: "main", Opc: creds.Opc}}}})
		}},
		{name: "a plan with a secret-named app param", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return startPlan(ctx, h, url, planWithParams(map[string]string{"X-Api-Key": "k-123456"}))
		}},
		{name: "a probe carrying keys (a stream)", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return drainProbe(ctx, h, url, &orbitv1.ResolvedTarget{Subscribers: []*orbitv1.SubscriberPool{{Name: "main", Ki: creds.Ki}}})
		}},
		{name: "a probe carrying a responder token (a stream)", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return drainProbe(ctx, h, url, &orbitv1.ResolvedTarget{App: &orbitv1.TargetApp{Responders: []*orbitv1.Responder{{Name: "app1", Token: "t"}}}})
		}},
		{name: "a probe without secrets (a stream)", call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return drainProbe(ctx, h, url, &orbitv1.ResolvedTarget{Name: "lab1"})
		}},
		// PlanRun (U2): every ${VAR} value counts as a secret, whatever its
		// name; a key written out in either document is one too.
		{name: "PlanRun with a var", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).PlanRun(ctx, connect.NewRequest(&orbitv1.ResolveRequest{
				TargetYaml: "kind: target\nname: lab1\n", RunYaml: "kind: run\n", Vars: []*orbitv1.Variable{{Name: "LAB_K", Value: "v"}}}))
			return err
		}},
		{name: "PlanRun with a key in the target", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).PlanRun(ctx, connect.NewRequest(&orbitv1.ResolveRequest{
				TargetYaml: "kind: target\nname: lab1\nsubscribers:\n  - {name: main, ki: " + creds.Ki + "}\n", RunYaml: "kind: run\n"}))
			return err
		}},
		{name: "PlanRun with placeholders only", call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).PlanRun(ctx, connect.NewRequest(&orbitv1.ResolveRequest{
				TargetYaml: "kind: target\nname: lab1\nsubscribers:\n  - {name: main, ki: \"${ORBIT_KI}\"}\n", RunYaml: "kind: run\n"}))
			return ignoreInvalid(err)
		}},
		{name: "RenderRun with a token in behaviors", secrets: true, call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).RenderRun(ctx, connect.NewRequest(&orbitv1.RenderRunRequest{
				TargetName: "lab1", RunYaml: "kind: run\nworkloads:\n  - name: f\n    behaviors: {traffic: {apps: [{token: t-123}]}}\n"}))
			return err
		}},
		{name: "RenderRun without secrets", call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			_, err := orbitv1connect.NewRunServiceClient(h, url).RenderRun(ctx, connect.NewRequest(&orbitv1.RenderRunRequest{TargetName: "lab1", RunYaml: "kind: run\n"}))
			return ignoreInvalid(err)
		}},
		{name: "a plan without secrets", call: func(ctx context.Context, h connect.HTTPClient, url string) error {
			return startPlan(ctx, h, url, planWithParams(map[string]string{"object_size": "256KB"}))
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

// ignoreInvalid is err unless it is InvalidArgument: a call that passed
// the secrets check and was refused for its content.
func ignoreInvalid(err error) error {
	if connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil
	}
	return err
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:5000": true, "127.9.9.9:1": true, "[::1]:5000": true, "[::ffff:127.0.0.1]:1": true,
		"192.0.2.1:5000": false, "[2001:db8::1]:1": false, "garbage": false} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%s) = %t", addr, got)
		}
	}
}

// The keys check covers streamed requests too (checkedConn.Receive). No
// streaming RPC's request carries a marked field today, so a synthetic
// server-stream procedure taking Credentials stands in for a future one.
func TestSecretsOverPlaintextStreaming(t *testing.T) {
	const procedure = "/orbit.v1.TestService/Stream"
	for _, tc := range []struct {
		name         string
		loopback     bool
		creds        *orbitv1.Credentials
		wantRefusal  bool
		wantMessages int
	}{
		{name: "non-loopback plaintext, keys", creds: &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff"}, wantRefusal: true},
		{name: "non-loopback plaintext, no keys", creds: &orbitv1.Credentials{}, wantMessages: 1},
		{name: "loopback plaintext, keys", loopback: true, creds: &orbitv1.Credentials{Ki: "00112233445566778899aabbccddeeff"}, wantMessages: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := newSecurity(slog.New(slog.NewTextHandler(io.Discard, nil)), "")
			sec.loopback = func(string) bool { return tc.loopback }
			mux := http.NewServeMux()
			mux.Handle(procedure, connect.NewServerStreamHandler(procedure,
				func(_ context.Context, _ *connect.Request[orbitv1.Credentials], s *connect.ServerStream[orbitv1.GetInfoResponse]) error {
					return s.Send(&orbitv1.GetInfoResponse{Version: "streamed"})
				}, connect.WithInterceptors(keysInterceptor{})))
			srv := httptest.NewServer(sec.middleware(mux))
			t.Cleanup(srv.Close)

			c := connect.NewClient[orbitv1.Credentials, orbitv1.GetInfoResponse](srv.Client(), srv.URL+procedure)
			stream, err := c.CallServerStream(context.Background(), connect.NewRequest(tc.creds))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for stream.Receive() {
				n++
			}
			err = stream.Err()
			if tc.wantRefusal {
				if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "plaintext") {
					t.Fatalf("err = %v, want failed_precondition naming plaintext", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if n != tc.wantMessages {
				t.Errorf("received %d messages, want %d", n, tc.wantMessages)
			}
		})
	}
}

// startPlan sends a StartRun with plan; until R2 an accepted one answers
// UNIMPLEMENTED, which the caller reads as "not refused".
func startPlan(ctx context.Context, h connect.HTTPClient, url string, p *orbitv1.RunPlan) error {
	_, err := orbitv1connect.NewRunServiceClient(h, url).StartRun(ctx, connect.NewRequest(&orbitv1.StartRunRequest{
		Spec: &orbitv1.StartRunRequest_Plan{Plan: p}}))
	return err
}

// planWithParams is a one-workload plan whose app cohort has params.
func planWithParams(params map[string]string) *orbitv1.RunPlan {
	return &orbitv1.RunPlan{Workloads: []*orbitv1.Workload{{Name: "w", Kind: &orbitv1.Workload_Population{Population: &orbitv1.PopulationWorkload{
		Behaviors: &orbitv1.Behaviors{Traffic: &orbitv1.Traffic{Mix: []*orbitv1.TrafficShare{{Name: "web",
			Kind: &orbitv1.TrafficShare_App{App: &orbitv1.AppCohort{App: "http", Params: params}}}}}},
	}}}}}
}

// drainProbe runs a probe of t and reads it to the end: its error, or the
// stream's.
func drainProbe(ctx context.Context, h connect.HTTPClient, url string, t *orbitv1.ResolvedTarget) error {
	stream, err := orbitv1connect.NewTargetServiceClient(h, url).Probe(ctx, connect.NewRequest(&orbitv1.ProbeRequest{Target: t}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
	}
	return stream.Err()
}
