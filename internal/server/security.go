package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/bgrewell/orbit/internal/secrets"
)

// Auth failure limits: a peer with authFailLimit failed authentications
// within authFailWindow is refused for authLockout (ADR-0008 §4: a token
// is not to be guessed at line rate).
const (
	authFailLimit  = 10
	authFailWindow = 10 * time.Second
	authLockout    = 30 * time.Second
)

// connState is what the transport knew about a request's connection, kept
// in its context for the keys check.
type connState struct {
	tls      bool // the request arrived over TLS
	loopback bool // the peer's address is loopback
}

type connStateKey struct{}

// isLoopbackAddr reports whether a host:port (a request's RemoteAddr) is a
// loopback address.
func isLoopbackAddr(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// security is the API's access control: the bearer token on every Connect
// procedure and /metrics (/healthz and the dashboard's static files stay
// open: they hold no secrets), a limit on repeated failures, and the record
// of each connection's TLS and peer for the keys-over-plaintext check.
type security struct {
	log   *slog.Logger
	token string // "" = no token required
	// loopback reports whether a peer is loopback; tests replace it to make
	// a peer non-loopback.
	loopback func(remote string) bool
	now      func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time // by peer IP, within the window
	locked   map[string]time.Time   // peer IP → refused until
}

func newSecurity(log *slog.Logger, token string) *security {
	return &security{log: log, token: token, loopback: isLoopbackAddr, now: time.Now,
		failures: map[string][]time.Time{}, locked: map[string]time.Time{}}
}

// protected reports whether a request path needs the token: a Connect
// procedure (/orbit.v1.Service/Method) or /metrics.
func protected(path string) bool {
	return strings.HasPrefix(path, "/orbit.v1.") || path == "/metrics"
}

func peerIP(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// middleware wraps the mux: it records the connection's state and, when a
// token is set, refuses a protected request without it.
func (s *security) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs := connState{tls: r.TLS != nil, loopback: s.loopback(r.RemoteAddr)}
		r = r.WithContext(context.WithValue(r.Context(), connStateKey{}, cs))
		if s.token == "" || !protected(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ip := peerIP(r.RemoteAddr)
		if s.lockedOut(ip) {
			s.refuse(w, r, connect.CodeResourceExhausted, http.StatusTooManyRequests,
				"too many failed authentications from this address; try again shortly")
			return
		}
		if !s.authorized(r.Header.Get("Authorization")) {
			s.fail(ip, r.URL.Path)
			s.refuse(w, r, connect.CodeUnauthenticated, http.StatusUnauthorized,
				"a bearer token is required: set $ORBIT_TOKEN, or add it to ~/.config/orbit/servers.yaml (orbit server trust)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized checks an Authorization header against the token in constant
// time.
func (s *security) authorized(header string) bool {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := []byte(strings.TrimSpace(header[len(prefix):]))
	return subtle.ConstantTimeCompare(got, []byte(s.token)) == 1
}

// fail records a failed authentication, logged without what was presented,
// and locks the peer out once it reaches the limit within the window.
func (s *security) fail(ip, path string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := s.failures[ip][:0]
	for _, t := range s.failures[ip] {
		if now.Sub(t) < authFailWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	s.failures[ip] = recent
	s.log.Warn("unauthenticated request", "peer", ip, "path", path, "recent_failures", len(recent))
	if len(recent) >= authFailLimit {
		s.locked[ip] = now.Add(authLockout)
		delete(s.failures, ip)
		s.log.Warn("peer locked out after repeated failed authentications", "peer", ip, "for", authLockout)
	}
}

func (s *security) lockedOut(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.locked[ip]
	if !ok {
		return false
	}
	if s.now().After(until) {
		delete(s.locked, ip)
		return false
	}
	return true
}

// refuse answers a protected request: a Connect error for a procedure, a
// plain status for /metrics.
func (s *security) refuse(w http.ResponseWriter, r *http.Request, code connect.Code, status int, msg string) {
	if strings.HasPrefix(r.URL.Path, "/orbit.v1.") {
		_ = connect.NewErrorWriter().Write(w, r, connect.NewError(code, errors.New(msg)))
		return
	}
	http.Error(w, msg, status)
}

// errKeysOverPlaintext is the refusal for secrets sent in the clear from
// off the host (ADR-0008 §4). The CLI refuses to send them first; this is
// the defence for other clients.
var errKeysOverPlaintext = errors.New("subscriber keys or tokens over plaintext from a non-loopback peer; serve with TLS (--tls auto or --tls-cert) and connect with https://")

// checkSecrets refuses a message carrying secrets (secrets.Find) when the
// request came over plaintext from a non-loopback peer.
func checkSecrets(ctx context.Context, msg any) error {
	cs, ok := ctx.Value(connStateKey{}).(connState)
	if !ok || cs.tls || cs.loopback {
		return nil
	}
	m, ok := msg.(proto.Message)
	if !ok {
		return nil
	}
	if found := secrets.Find(m); len(found) > 0 {
		return connect.NewError(connect.CodeFailedPrecondition, errKeysOverPlaintext)
	}
	return nil
}

// keysInterceptor applies checkSecrets to every request message, unary and
// streamed.
type keysInterceptor struct{}

func (keysInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := checkSecrets(ctx, req.Any()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (keysInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (keysInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(ctx, &checkedConn{StreamingHandlerConn: conn, ctx: ctx})
	}
}

// checkedConn checks each message a stream receives.
type checkedConn struct {
	connect.StreamingHandlerConn
	ctx context.Context
}

func (c *checkedConn) Receive(msg any) error {
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	return checkSecrets(c.ctx, msg)
}
