package sctp

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"
)

// Every Dial failure after the local address is accepted is a *DialError
// carrying the remote and the source known at that point, with the text it
// had before the type existed and the causes still visible to errors.Is. A
// malformed local address stays a plain error.
func TestDialErrorTyped(t *testing.T) {
	fail := func(err error) DialOption {
		return withRawDialer(func(_, _ *isctp.SCTPAddr, _ isctp.InitMsg) (rawConn, error) { return nil, err })
	}
	// block holds the connect until the test ends, so the context decides.
	block := func(t *testing.T) DialOption {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		return withRawDialer(func(_, _ *isctp.SCTPAddr, _ isctp.InitMsg) (rawConn, error) {
			<-release
			return nil, syscall.ECONNREFUSED
		})
	}
	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	tests := []struct {
		name       string
		ctx        func(t *testing.T) context.Context
		laddr      string
		raddr      string
		opts       func(t *testing.T) []DialOption
		wantText   string // exact, when set
		wantSource string
		wantIs     error
	}{
		{name: "remote does not parse", raddr: "127.0.0.1",
			wantText: `resolve remote "127.0.0.1": address 127.0.0.1: missing port in address`},
		{name: "remote does not parse, bound", laddr: "127.0.0.1", raddr: "127.0.0.1",
			wantSource: "127.0.0.1:0"},
		{name: "probe fails", raddr: "192.0.2.1:38412",
			opts: func(*testing.T) []DialOption {
				return []DialOption{WithSourceResolver(func(*net.UDPAddr) (net.IP, error) { return nil, syscall.ENETUNREACH })}
			},
			wantText: `cannot determine N2 source for "192.0.2.1:38412": network is unreachable; pass --bind`, wantIs: syscall.ENETUNREACH},
		{name: "probe returns unspecified", raddr: "192.0.2.1:38412",
			opts: func(*testing.T) []DialOption {
				return []DialOption{WithSourceResolver(func(*net.UDPAddr) (net.IP, error) { return net.IPv4zero, nil })}
			},
			wantText: `cannot determine N2 source for "192.0.2.1:38412": resolver returned 0.0.0.0; pass --bind`},
		{name: "family mismatch", laddr: "127.0.0.1", raddr: "[::1]:38412",
			wantText: "address family mismatch: local 127.0.0.1:0 is IPv4 but AMF [::1]:38412 is IPv6", wantSource: "127.0.0.1:0"},
		{name: "context already cancelled", ctx: func(*testing.T) context.Context { return cancelled() }, laddr: "127.0.0.1", raddr: "127.0.0.1:38412",
			opts:     func(*testing.T) []DialOption { return []DialOption{fail(syscall.ECONNREFUSED)} },
			wantText: `sctp dial "127.0.0.1:38412" from "127.0.0.1:0": context canceled`, wantSource: "127.0.0.1:0", wantIs: context.Canceled},
		{name: "refused", laddr: "127.0.0.1", raddr: "127.0.0.1:38412",
			opts:       func(*testing.T) []DialOption { return []DialOption{fail(syscall.ECONNREFUSED)} },
			wantText:   `sctp dial "127.0.0.1:38412" from "127.0.0.1:0": connection refused (3 attempts: ECONNREFUSED, ECONNREFUSED, ECONNREFUSED)`,
			wantSource: "127.0.0.1:0", wantIs: syscall.ECONNREFUSED},
		{name: "refused, without retry", laddr: "127.0.0.1", raddr: "127.0.0.1:38412",
			opts:     func(*testing.T) []DialOption { return []DialOption{fail(syscall.ECONNREFUSED), WithoutRetry()} },
			wantText: `sctp dial "127.0.0.1:38412" from "127.0.0.1:0": connection refused`, wantSource: "127.0.0.1:0", wantIs: syscall.ECONNREFUSED},
		{name: "refused, route-selected", raddr: "127.0.0.1:38412",
			opts: func(*testing.T) []DialOption {
				return []DialOption{fail(syscall.ECONNREFUSED), WithSourceResolver(func(*net.UDPAddr) (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil })}
			},
			wantSource: "127.0.0.1:0 (route-selected)", wantIs: syscall.ECONNREFUSED},
		{name: "kernel gave up", laddr: "127.0.0.1", raddr: "127.0.0.1:38412",
			opts:       func(*testing.T) []DialOption { return []DialOption{fail(syscall.ETIMEDOUT)} },
			wantSource: "127.0.0.1:0", wantIs: syscall.ETIMEDOUT},
		{name: "bound fired", laddr: "127.0.0.1", raddr: "127.0.0.1:38412",
			opts:       func(t *testing.T) []DialOption { return []DialOption{block(t), WithDialTimeout(50 * time.Millisecond)} },
			wantSource: "127.0.0.1:0", wantIs: context.DeadlineExceeded},
		{name: "cancelled mid-dial", laddr: "127.0.0.1", raddr: "127.0.0.1:38412",
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(50*time.Millisecond, cancel)
				t.Cleanup(cancel)
				return ctx
			},
			opts:       func(t *testing.T) []DialOption { return []DialOption{block(t)} },
			wantSource: "127.0.0.1:0", wantIs: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}
			var opts []DialOption
			if tc.opts != nil {
				opts = tc.opts(t)
			}
			_, err := Dial(ctx, tc.laddr, tc.raddr, opts...)
			var de *DialError
			if !errors.As(err, &de) {
				t.Fatalf("Dial error %v (%T) is not a *DialError", err, err)
			}
			if de.Remote != tc.raddr || de.Source != tc.wantSource {
				t.Errorf("Remote %q Source %q, want %q %q", de.Remote, de.Source, tc.raddr, tc.wantSource)
			}
			if err.Error() != de.Err.Error() || !errors.Is(err, de.Err) {
				t.Errorf("Error() %q is not the cause's %q, or Unwrap does not reach it", err, de.Err)
			}
			if tc.wantText != "" && err.Error() != tc.wantText {
				t.Errorf("text %q, want %q", err, tc.wantText)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("errors.Is(%q, %v) = false", err, tc.wantIs)
			}
		})
	}
}

// A malformed local address is the caller's input, not reachability: it
// stays a plain error.
func TestDialBadLocalIsNotDialError(t *testing.T) {
	_, err := Dial(context.Background(), "not an address:x:y", "127.0.0.1:38412")
	var de *DialError
	if err == nil || errors.As(err, &de) {
		t.Errorf("err = %v (%T), want a plain error", err, err)
	}
}
