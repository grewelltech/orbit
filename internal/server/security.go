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

// Auth failure limits: a peer presenting authFailLimit wrong tokens within
// authFailWindow is refused for authLockout, the right token included and
// with no token compared, so the limit caps the guess rate (ADR-0008 §4: a
// token is not to be guessed at line rate). Requests without a token are
// never counted, so a dashboard polling before its token is entered cannot
// lock its operator out. At most
// authMaxPeers peers are tracked, so rotating addresses cannot grow the
// tables without bound.
const (
	authFailLimit  = 10
	authFailWindow = 10 * time.Second
	authLockout    = 30 * time.Second
	authMaxPeers   = 4096
)

// Refusal messages: a request without a token, and one with the wrong one.
const (
	msgTokenRequired = "a bearer token is required: set $ORBIT_TOKEN, or add it to ~/.config/orbit/servers.yaml (orbit server trust)"
	msgTokenInvalid  = "invalid bearer token"
	msgLockedOut     = "too many invalid bearer tokens from this address; try again in 30 s"
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
	failures map[string][]time.Time // by peerKey, within the window
	locked   map[string]time.Time   // peerKey → refused until
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

// peerKey is what the failure limit counts by: the peer's IPv4 address, or
// its IPv6 /64, since one host can rotate through a whole /64.
func peerKey(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, err := a.WithZone("").Prefix(64)
	if err != nil {
		return a.String()
	}
	return p.String()
}

// middleware wraps the mux: it records the connection's state and, when a
// token is set, refuses a protected request without the right one.
func (s *security) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs := connState{tls: r.TLS != nil, loopback: s.loopback(r.RemoteAddr)}
		r = r.WithContext(context.WithValue(r.Context(), connStateKey{}, cs))
		if s.token == "" || !protected(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		header := r.Header.Get("Authorization")
		switch {
		case header == "":
			// No guess was made, so nothing is counted: a dashboard polling
			// before its token is entered is not an attack.
			s.refuse(w, r, connect.CodeUnauthenticated, http.StatusUnauthorized, msgTokenRequired)
		case s.lockedOut(peerKey(r.RemoteAddr)):
			// Before the comparison: a locked-out peer learns nothing about
			// any token until the lockout ends.
			s.refuse(w, r, connect.CodeResourceExhausted, http.StatusTooManyRequests, msgLockedOut)
		case s.authorized(header):
			next.ServeHTTP(w, r)
		default:
			s.fail(peerKey(r.RemoteAddr), r.URL.Path)
			s.refuse(w, r, connect.CodeUnauthenticated, http.StatusUnauthorized, msgTokenInvalid)
		}
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

// fail records a wrong token, logged without what was presented, and locks
// the peer out once it reaches the limit within the window.
func (s *security) fail(peer, path string) {
	s.failWith(peer, "invalid bearer token", "path", path)
}

// failWith is fail with its own log message and attributes: a join token
// miss logs no path, which would hold the token presented.
func (s *security) failWith(peer, msg string, attrs ...any) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(now)
	recent := s.failures[peer][:0]
	for _, t := range s.failures[peer] {
		if now.Sub(t) < authFailWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	s.failures[peer] = recent
	s.log.Warn(msg, append([]any{"peer", peer}, append(attrs, "recent_failures", len(recent))...)...)
	if len(recent) >= authFailLimit {
		s.locked[peer] = now.Add(authLockout)
		delete(s.failures, peer)
		s.log.Warn("peer locked out after repeated invalid tokens", "peer", peer, "for", authLockout)
	}
}

// sweep drops expired entries once a table reaches authMaxPeers, and then,
// if it is still full, arbitrary ones: under an address-rotation flood the
// limit degrades, but memory stays bounded. Called with s.mu held.
func (s *security) sweep(now time.Time) {
	if len(s.failures) >= authMaxPeers {
		for k, ts := range s.failures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= authFailWindow {
				delete(s.failures, k)
			}
		}
		for k := range s.failures {
			if len(s.failures) < authMaxPeers {
				break
			}
			delete(s.failures, k)
		}
	}
	if len(s.locked) >= authMaxPeers {
		for k, until := range s.locked {
			if now.After(until) {
				delete(s.locked, k)
			}
		}
		for k := range s.locked {
			if len(s.locked) < authMaxPeers {
				break
			}
			delete(s.locked, k)
		}
	}
}

func (s *security) lockedOut(peer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.locked[peer]
	if !ok {
		return false
	}
	if s.now().After(until) {
		delete(s.locked, peer)
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
