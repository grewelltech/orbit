package sctp

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	isctp "github.com/ishidawataru/sctp"
)

// The requested SCTP max-init timeout caps ONE INIT attempt's RTO, so it is a
// quarter of the dial bound (three retransmits, initAttempts, then make up
// the rest), rounded up and clamped to the option's 1..65535 ms range. Never
// 0: the kernel reads 0 as its sysctl default.
func TestInitTimeout(t *testing.T) {
	tests := []struct {
		name  string
		bound time.Duration
		want  uint16
	}{
		{name: "default 20 s", bound: DefaultDialTimeout, want: 5000},
		{name: "2 s", bound: 2 * time.Second, want: 500},
		{name: "5 s", bound: 5 * time.Second, want: 1250},
		{name: "2 ms rounds up to 1", bound: 2 * time.Millisecond, want: 1},
		{name: "0.4 ms is still 1", bound: 400 * time.Microsecond, want: 1},
		{name: "spent bound is still 1", bound: -time.Minute, want: 1},
		{name: "zero is still 1", bound: 0, want: 1},
		{name: "300 s clamps to the field max", bound: 300 * time.Second, want: 65535},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := initTimeout(tc.bound); got != tc.want {
				t.Errorf("initTimeout(%v) = %d, want %d", tc.bound, got, tc.want)
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

// Dial returns as soon as its bound passes even while the kernel is still
// connecting: the caller's deadline, a cancel, or, with no deadline at all,
// the timeout Dial sets itself (the bound is always on). It wraps the context
// error so callers can errors.Is it, names the source, the elapsed time and
// the bound, and closes the connection the kernel eventually produces exactly
// once, so a cancelled dial leaks nothing.
func TestDialContextBoundReturnsEarlyAndClosesLate(t *testing.T) {
	tests := []struct {
		name     string
		ctx      func() (context.Context, context.CancelFunc)
		opts     []DialOption
		wantErr  error
		wantText []string
	}{
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, wantErr: context.DeadlineExceeded, wantText: []string{"no INIT-ACK after ", "(bound 50ms; AMF unreachable or black-holed)"}},
		{name: "cancel", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
			return ctx, cancel
		}, wantErr: context.Canceled, wantText: []string{"cancelled after "}},
		{name: "no deadline: Dial's own bound", ctx: func() (context.Context, context.CancelFunc) {
			return context.Background(), func() {}
		}, opts: []DialOption{WithDialTimeout(50 * time.Millisecond)}, wantErr: context.DeadlineExceeded,
			wantText: []string{"no INIT-ACK after ", "(bound 50ms; AMF unreachable or black-holed)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			b := newBlockingDialer()
			start := time.Now()
			_, err := Dial(ctx, "127.0.0.1", "127.0.0.1:38412", append([]DialOption{withRawDialer(b.dial)}, tc.opts...)...)
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("Dial took %s; it must return when the bound passes", elapsed)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			for _, w := range append([]string{`sctp dial "127.0.0.1:38412" from "127.0.0.1:0"`}, tc.wantText...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if tc.wantErr == context.Canceled && strings.Contains(err.Error(), "INIT-ACK") {
				t.Errorf("a cancel must not claim the AMF was unreachable: %q", err)
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

// A kernel timeout on the INIT exchange is reported as "no INIT-ACK after
// <elapsed> (bound <bound>; ...)", naming the bound actually requested; any
// other connect error keeps its existing wrapping. Both wrap the syscall error.
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
			want: []string{`sctp dial "127.0.0.1:38412" from "127.0.0.1:0"`, "no INIT-ACK after ", "(bound 20s; AMF unreachable or black-holed)"}, wantIsTO: true},
		{name: "timed out, option bound", opts: []DialOption{WithDialTimeout(3 * time.Second)}, dialErr: syscall.ETIMEDOUT,
			want: []string{"(bound 3s;"}, wantIsTO: true},
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

// The InitMsg handed to the kernel asks for a quarter of the bound per INIT
// attempt and three retransmits, so it abandons the exchange at about the
// bound: the context deadline when there is one, else the option or the
// default; the option never extends a deadline. Streams are untouched.
func TestDialInitTimeoutPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		timeout  time.Duration // context deadline from now; 0 = none
		opts     []DialOption
		min, max uint16
	}{
		{name: "default 20 s", min: 5000, max: 5000},
		{name: "option 2 s without deadline", opts: []DialOption{WithDialTimeout(2 * time.Second)}, min: 500, max: 500},
		{name: "option 2 ms without deadline", opts: []DialOption{WithDialTimeout(2 * time.Millisecond)}, min: 1, max: 1},
		{name: "deadline 500 ms without option", timeout: 500 * time.Millisecond, min: 100, max: 125},
		{name: "option never extends a deadline", timeout: 500 * time.Millisecond, opts: []DialOption{WithDialTimeout(2 * time.Second)}, min: 100, max: 125},
		{name: "deadline 90 s", timeout: 90 * time.Second, min: 22400, max: 22500},
		{name: "deadline beyond four times the field max clamps", timeout: 300 * time.Second, min: 65535, max: 65535},
		{name: "non-positive option is ignored", opts: []DialOption{WithDialTimeout(0)}, min: 5000, max: 5000},
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
			if got.NumOstreams != DefaultNGAPStreams || got.MaxInstreams != DefaultNGAPStreams {
				t.Errorf("InitMsg = %+v; streams changed", got)
			}
			if got.MaxAttempts != initAttempts {
				t.Errorf("MaxAttempts = %d, want %d", got.MaxAttempts, initAttempts)
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

// openFDs counts this process's open file descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot count file descriptors: %v", err)
	}
	return len(entries)
}

// Against a real black hole (an address that drops SCTP; set
// ORBIT_BLACKHOLE_AMF=ip:port, normally inside a network namespace), a dial
// with a 2 s deadline returns in about 2 s with the context error, and the
// kernel socket the dial left behind is closed within a few seconds of that,
// because the kernel was asked to abandon the exchange at about the bound
// rather than after its default ~2.4 min. Skipped by default so the unit
// suite sends nothing off-host.
func TestDialBlackHoleBounded(t *testing.T) {
	amf := os.Getenv("ORBIT_BLACKHOLE_AMF")
	if amf == "" {
		t.Skip("set ORBIT_BLACKHOLE_AMF=ip:port (an address that drops SCTP) to run")
	}
	baseline := openFDs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, "", amf)
	took := time.Since(start)
	t.Logf("Dial(%s) returned after %s: %v", amf, took.Round(100*time.Millisecond), err)
	if err == nil {
		t.Fatal("the black hole answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, syscall.ETIMEDOUT) {
		t.Errorf("err = %v, want the deadline or a kernel timeout", err)
	}
	if took < 1500*time.Millisecond || took > 4*time.Second {
		t.Errorf("Dial took %s, want about 2 s", took)
	}
	// rto_min (1 s by default) floors each attempt, so with a 2 s bound the
	// kernel needs about 4.5 s in all to give up; allow for that.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if openFDs(t) <= baseline {
			t.Logf("orphaned socket closed %s after Dial returned", time.Since(start.Add(took)).Round(100*time.Millisecond))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("file descriptors did not return to the baseline of %d within 6 s of the dial returning", baseline)
}

// The bound in error text is rounded to 10 ms, so one derived from a
// deadline a moment old reads as requested (#122).
func TestPrintedBound(t *testing.T) {
	tests := []struct {
		bound time.Duration
		want  string
	}{
		{9999 * time.Millisecond, "10s"},
		{9994 * time.Millisecond, "9.99s"},
		{20 * time.Second, "20s"},
		{19*time.Second + 999*time.Millisecond + 400*time.Microsecond, "20s"},
		{50 * time.Millisecond, "50ms"},
		{49996 * time.Microsecond, "50ms"},
		{3 * time.Millisecond, "3ms"},
	}
	for _, tc := range tests {
		if got := printedBound(tc.bound).String(); got != tc.want {
			t.Errorf("printedBound(%s) = %s, want %s", tc.bound, got, tc.want)
		}
	}
}
