package sctp

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"
)

// The requested SCTP max-init timeout is the time remaining to the deadline,
// rounded up and clamped to the option's 1..65535 ms range; with no deadline
// it is the fallback. Never 0: the kernel reads 0 as its sysctl default.
func TestInitTimeout(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		deadline time.Time
		fallback time.Duration
		want     uint16
	}{
		{name: "no deadline: fallback", fallback: DefaultDialTimeout, want: 20000},
		{name: "no deadline: option", fallback: 2 * time.Second, want: 2000},
		{name: "5 s left", deadline: now.Add(5 * time.Second), fallback: DefaultDialTimeout, want: 5000},
		{name: "100 s left clamps to the field max", deadline: now.Add(100 * time.Second), fallback: DefaultDialTimeout, want: 65535},
		{name: "0.4 ms left rounds up to 1", deadline: now.Add(400 * time.Microsecond), fallback: DefaultDialTimeout, want: 1},
		{name: "deadline in the past is still 1", deadline: now.Add(-time.Minute), fallback: DefaultDialTimeout, want: 1},
		{name: "zero fallback and no deadline is still 1", fallback: 0, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := initTimeout(now, tc.deadline, tc.fallback); got != tc.want {
				t.Errorf("initTimeout(%v, %v) = %d, want %d", tc.deadline, tc.fallback, got, tc.want)
			}
		})
	}
}

// fakeRawConn stands in for a kernel association and counts its closes.
type fakeRawConn struct {
	closes atomic.Int32
}

func (f *fakeRawConn) SCTPWrite([]byte, *isctp.SndRcvInfo) (int, error) { return 0, nil }
func (f *fakeRawConn) SCTPRead([]byte) (int, *isctp.SndRcvInfo, error)  { return 0, nil, nil }
func (f *fakeRawConn) SubscribeEvents(int) error                        { return nil }
func (f *fakeRawConn) GetStatus() (*isctp.Status, error)                { return nil, errors.New("fake") }
func (f *fakeRawConn) LocalAddr() net.Addr                              { return nil }
func (f *fakeRawConn) Close() error                                     { f.closes.Add(1); return nil }

// blockingDialer is a rawDialer that blocks until released, then hands back
// the given connection, standing in for a kernel still retransmitting INIT.
type blockingDialer struct {
	release chan struct{}
	conn    *fakeRawConn
	once    sync.Once
	init    isctp.InitMsg
}

func newBlockingDialer() *blockingDialer {
	return &blockingDialer{release: make(chan struct{}), conn: &fakeRawConn{}}
}

func (b *blockingDialer) dial(_, _ *isctp.SCTPAddr, init isctp.InitMsg) (rawConn, error) {
	b.init = init
	<-b.release
	return b.conn, nil
}

func (b *blockingDialer) unblock() { b.once.Do(func() { close(b.release) }) }

// waitFor polls cond for up to 2 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Dial returns as soon as its context is done even while the kernel is still
// connecting, wraps the context error so callers can errors.Is it, names the
// source, and closes the connection the kernel eventually produces exactly
// once, so a cancelled dial leaks nothing.
func TestDialContextBoundReturnsEarlyAndClosesLate(t *testing.T) {
	tests := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, wantErr: context.DeadlineExceeded},
		{name: "cancel", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
			return ctx, cancel
		}, wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			b := newBlockingDialer()
			start := time.Now()
			_, err := Dial(ctx, "127.0.0.1", "127.0.0.1:38412", withRawDialer(b.dial))
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("Dial took %s; it must return when the context is done", elapsed)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), `sctp dial "127.0.0.1:38412" from "127.0.0.1:0"`) {
				t.Errorf("error %q does not name the AMF and source", err)
			}
			if got := b.conn.closes.Load(); got != 0 {
				t.Fatalf("connection closed %d times before the kernel produced it", got)
			}
			b.unblock()
			waitFor(t, "late close", func() bool { return b.conn.closes.Load() == 1 })
			time.Sleep(20 * time.Millisecond)
			if got := b.conn.closes.Load(); got != 1 {
				t.Errorf("late connection closed %d times, want exactly 1", got)
			}
		})
	}
}

// A kernel timeout on the INIT exchange is reported as "no INIT-ACK within
// <bound>", naming the bound actually requested; any other connect error
// keeps its existing wrapping. Both still wrap the syscall error.
func TestDialKernelErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		opts     []DialOption
		dialErr  syscall.Errno
		want     []string
		wantNot  string
		wantIsTO bool
	}{
		{name: "timed out, default bound", dialErr: syscall.ETIMEDOUT,
			want: []string{`sctp dial "127.0.0.1:38412" from "127.0.0.1:0"`, "no INIT-ACK within 20s", "AMF unreachable or black-holed"}, wantIsTO: true},
		{name: "timed out, option bound", opts: []DialOption{WithDialTimeout(3 * time.Second)}, dialErr: syscall.ETIMEDOUT,
			want: []string{"no INIT-ACK within 3s"}, wantIsTO: true},
		{name: "refused is unchanged", dialErr: syscall.ECONNREFUSED,
			want: []string{`sctp dial "127.0.0.1:38412" from "127.0.0.1:0": connection refused`}, wantNot: "INIT-ACK"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]DialOption{withRawDialer(func(_, _ *isctp.SCTPAddr, _ isctp.InitMsg) (rawConn, error) {
				return nil, tc.dialErr
			})}, tc.opts...)
			_, err := Dial(context.Background(), "127.0.0.1", "127.0.0.1:38412", opts...)
			if err == nil {
				t.Fatal("Dial succeeded on a failing connect")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if tc.wantNot != "" && strings.Contains(err.Error(), tc.wantNot) {
				t.Errorf("error %q wrongly carries %q", err, tc.wantNot)
			}
			if !errors.Is(err, tc.dialErr) {
				t.Errorf("error %q does not wrap %v", err, tc.dialErr)
			}
			if tc.wantIsTO != errors.Is(err, syscall.ETIMEDOUT) {
				t.Errorf("errors.Is(err, ETIMEDOUT) = %v, want %v", !tc.wantIsTO, tc.wantIsTO)
			}
		})
	}
}

// The max-init timeout handed to the kernel follows the context deadline
// when there is one and the option (or the default) when there is not; the
// option never extends a deadline. Streams are untouched.
func TestDialInitTimeoutPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		timeout  time.Duration // context deadline from now; 0 = none
		opts     []DialOption
		min, max uint16
	}{
		{name: "default", min: 20000, max: 20000},
		{name: "option without deadline", opts: []DialOption{WithDialTimeout(2 * time.Second)}, min: 2000, max: 2000},
		{name: "deadline without option", timeout: 500 * time.Millisecond, min: 400, max: 500},
		{name: "option never extends a deadline", timeout: 500 * time.Millisecond, opts: []DialOption{WithDialTimeout(2 * time.Second)}, min: 400, max: 500},
		{name: "deadline beyond the field max clamps", timeout: 90 * time.Second, min: 65535, max: 65535},
		{name: "non-positive option is ignored", opts: []DialOption{WithDialTimeout(0)}, min: 20000, max: 20000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.timeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
			}
			defer cancel()
			var got isctp.InitMsg
			opts := append([]DialOption{withRawDialer(func(_, _ *isctp.SCTPAddr, init isctp.InitMsg) (rawConn, error) {
				got = init
				return nil, syscall.ECONNREFUSED
			})}, tc.opts...)
			_, _ = Dial(ctx, "127.0.0.1", "127.0.0.1:38412", opts...)
			if got.MaxInitTimeout < tc.min || got.MaxInitTimeout > tc.max {
				t.Errorf("MaxInitTimeout = %d, want in [%d, %d]", got.MaxInitTimeout, tc.min, tc.max)
			}
			if got.NumOstreams != DefaultNGAPStreams || got.MaxInstreams != DefaultNGAPStreams || got.MaxAttempts != 0 {
				t.Errorf("InitMsg = %+v; streams or attempts changed", got)
			}
		})
	}
}

// A context already done before Dial is called still runs the local checks
// first (a bad bind is reported as such, before the context), and otherwise
// returns the context error without starting a connect.
func TestDialDoneContextSkipsConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var called atomic.Bool
	raw := withRawDialer(func(_, _ *isctp.SCTPAddr, _ isctp.InitMsg) (rawConn, error) {
		called.Store(true)
		return nil, syscall.ECONNREFUSED
	})
	_, err := Dial(ctx, "127.0.0.1", "127.0.0.1:38412", raw)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if called.Load() {
		t.Error("a connect was started for a context that was already done")
	}
	_, err = Dial(ctx, "10.0.0.1:0:0", "127.0.0.1:38412", raw)
	if err == nil || !strings.Contains(err.Error(), `invalid local address "10.0.0.1:0:0"`) {
		t.Errorf("err = %v, want the bind error ahead of the context error", err)
	}
}
