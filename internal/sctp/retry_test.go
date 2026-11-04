package sctp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"
)

// scriptedDialer is a rawDialer that fails with each errno in turn, then
// connects; it counts the connects it was asked for.
type scriptedDialer struct {
	errs  []error
	calls atomic.Int32
}

func (s *scriptedDialer) dial(_, _ *isctp.SCTPAddr, _ isctp.InitMsg) (rawConn, error) {
	n := int(s.calls.Add(1)) - 1
	if n < len(s.errs) {
		return nil, s.errs[n]
	}
	return &fakeRawConn{}, nil
}

// routeSourceLoopback is a source resolver that always picks 127.0.0.1, so
// an unbound dial needs no route.
func routeSourceLoopback(*net.UDPAddr) (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil }

// An ephemeral-port dial whose connect collides with a closing association
// (EISCONN, EALREADY, or the peer's ECONNREFUSED) is retried on a fresh
// socket, at most twice, inside the bound. A failure after retries names
// every attempt; a success after one leaves exactly one debug line. A pinned
// port, WithoutRetry, and an error that is not a collision are tried once.
func TestDialRetry(t *testing.T) {
	tests := []struct {
		name      string
		laddr     string
		errs      []error
		opts      []DialOption
		wantCalls int32
		wantErr   []string // substrings; none means the dial succeeds
		wantNot   string
		wantIs    error
		wantLog   string // the debug line's errors attribute; "" means no line
	}{
		{name: "first connect succeeds", laddr: "127.0.0.1", wantCalls: 1},
		{name: "EISCONN, then a fresh socket connects", laddr: "127.0.0.1",
			errs: []error{syscall.EISCONN}, wantCalls: 2, wantLog: "EISCONN"},
		{name: "route-selected source is ephemeral too", laddr: "",
			errs: []error{syscall.EALREADY, syscall.ECONNREFUSED}, wantCalls: 3, wantLog: "EALREADY,ECONNREFUSED"},
		{name: "ip:0 is ephemeral", laddr: "127.0.0.1:0",
			errs: []error{syscall.ECONNREFUSED}, wantCalls: 2, wantLog: "ECONNREFUSED"},
		{name: "every attempt collides", laddr: "127.0.0.1",
			errs:      []error{syscall.EISCONN, syscall.EISCONN, syscall.EISCONN, syscall.EISCONN},
			wantCalls: 3, wantErr: []string{"transport endpoint is already connected (3 attempts: EISCONN, EISCONN, EISCONN)"}, wantIs: syscall.EISCONN},
		{name: "a collision, then the kernel gives up", laddr: "127.0.0.1",
			errs:      []error{syscall.EISCONN, syscall.ETIMEDOUT},
			wantCalls: 2, wantErr: []string{"no INIT-ACK after ", "(2 attempts: EISCONN, ETIMEDOUT)"}, wantIs: syscall.ETIMEDOUT},
		{name: "a collision, then an error that is not one", laddr: "127.0.0.1",
			errs:      []error{syscall.EISCONN, syscall.EHOSTUNREACH},
			wantCalls: 2, wantErr: []string{"no route to host (2 attempts: EISCONN, EHOSTUNREACH)"}, wantIs: syscall.EHOSTUNREACH},
		{name: "not a collision: tried once", laddr: "127.0.0.1",
			errs: []error{syscall.EHOSTUNREACH}, wantCalls: 1, wantErr: []string{"no route to host"}, wantNot: "attempts", wantIs: syscall.EHOSTUNREACH},
		{name: "pinned port: tried once, with the reason", laddr: "127.0.0.1:5000",
			errs: []error{syscall.EISCONN}, wantCalls: 1,
			wantErr: []string{"source port 5000 may still hold an association to this AMF from an earlier dial: let it close, or bind port 0"},
			wantNot: "attempts", wantIs: syscall.EISCONN},
		{name: "pinned port refused: the AMF's answer, no hint", laddr: "127.0.0.1:5000",
			errs: []error{syscall.ECONNREFUSED}, wantCalls: 1, wantErr: []string{"connection refused"}, wantNot: "source port", wantIs: syscall.ECONNREFUSED},
		{name: "WithoutRetry: one dial is one connect", laddr: "127.0.0.1", opts: []DialOption{WithoutRetry()},
			errs: []error{syscall.EISCONN}, wantCalls: 1, wantErr: []string{"transport endpoint is already connected"}, wantNot: "attempts", wantIs: syscall.EISCONN},
		// The bound leaves less than the gap: no retry starts, and the first
		// connect's error stands with the reason.
		{name: "bound expires between attempts", laddr: "127.0.0.1",
			opts: []DialOption{WithDialTimeout(time.Second), withRetryGap(time.Hour)},
			errs: []error{syscall.EISCONN}, wantCalls: 1,
			wantErr: []string{"transport endpoint is already connected; no time left in the bound (1s) to retry"}, wantIs: syscall.EISCONN},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			d := &scriptedDialer{errs: tc.errs}
			opts := append([]DialOption{withRawDialer(d.dial), WithLogger(log),
				WithSourceResolver(routeSourceLoopback)}, tc.opts...)
			conn, err := Dial(context.Background(), tc.laddr, "127.0.0.1:38412", opts...)
			if got := d.calls.Load(); got != tc.wantCalls {
				t.Errorf("connects = %d, want %d", got, tc.wantCalls)
			}
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				_ = conn.Close()
			} else {
				if err == nil {
					t.Fatal("Dial succeeded")
				}
				for _, w := range tc.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("error %q lacks %q", err, w)
					}
				}
				if tc.wantNot != "" && strings.Contains(err.Error(), tc.wantNot) {
					t.Errorf("error %q wrongly carries %q", err, tc.wantNot)
				}
				if !errors.Is(err, tc.wantIs) {
					t.Errorf("error %q does not wrap %v", err, tc.wantIs)
				}
				var de *DialError
				if !errors.As(err, &de) {
					t.Errorf("error %T is not a *DialError", err)
				}
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if tc.wantLog == "" {
				if logs.Len() != 0 {
					t.Errorf("logged %q, want nothing", logs.String())
				}
				return
			}
			if len(lines) != 1 || !strings.Contains(lines[0], "level=DEBUG") ||
				!strings.Contains(lines[0], "errors="+tc.wantLog) || !strings.Contains(lines[0], `remote=127.0.0.1:38412`) {
				t.Errorf("log = %q, want one DEBUG line with errors=%s", logs.String(), tc.wantLog)
			}
		})
	}
}

// A context cancelled during the pause before a retry ends the dial there:
// no further connect, and the error says why.
func TestDialRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &scriptedDialer{errs: []error{syscall.EISCONN, syscall.EISCONN}}
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := Dial(ctx, "127.0.0.1", "127.0.0.1:38412", withRawDialer(d.dial), withRetryGap(5*time.Second))
	if got := d.calls.Load(); got != 1 {
		t.Errorf("connects = %d, want 1", got)
	}
	if err == nil || !strings.Contains(err.Error(), "already connected; cancelled before a retry") || !errors.Is(err, syscall.EISCONN) {
		t.Errorf("err = %v", err)
	}
}

// A bound that fires while a retry is connecting ends the dial with the
// usual "no INIT-ACK" error, and the attempt list still names the collision
// that led to the retry.
func TestDialRetryCutByBound(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	raw := func(_, _ *isctp.SCTPAddr, _ isctp.InitMsg) (rawConn, error) {
		if calls.Add(1) == 1 {
			return nil, syscall.EISCONN
		}
		<-release
		return nil, syscall.ECONNREFUSED
	}
	_, err := Dial(context.Background(), "127.0.0.1", "127.0.0.1:38412", withRawDialer(raw), WithDialTimeout(100*time.Millisecond))
	if got := calls.Load(); got != 2 {
		t.Errorf("connects = %d, want 2", got)
	}
	if err == nil || !strings.Contains(err.Error(), "no INIT-ACK after ") ||
		!strings.Contains(err.Error(), "(2 attempts: EISCONN, no answer in the bound)") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}

// errnoName gives the symbol for an errno, wrapped or not, and the text of
// anything else.
func TestErrnoName(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{syscall.EISCONN, "EISCONN"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "ECONNREFUSED"},
		{errors.New("odd"), "odd"},
	}
	for _, tc := range tests {
		if got := errnoName(tc.err); got != tc.want {
			t.Errorf("errnoName(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
