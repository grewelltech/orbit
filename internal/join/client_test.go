package join

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	orbitv1 "github.com/bgrewell/orbit/gen/orbit/v1"
	"github.com/bgrewell/orbit/gen/orbit/v1/orbitv1connect"
	"github.com/bgrewell/orbit/internal/tlspin"
)

// fakeJoin is a JoinService that records what it is sent and answers from
// a script of errors (nil = success).
type fakeJoin struct {
	orbitv1connect.UnimplementedJoinServiceHandler
	mu         sync.Mutex
	registers  []*orbitv1.RegisterResponderRequest
	regErrs    []error
	beats      []string // the Authorization header of each heartbeat
	beatErrs   []error
	beatPrevNs int64
}

func (f *fakeJoin) RegisterResponder(_ context.Context, req *connect.Request[orbitv1.RegisterResponderRequest]) (*connect.Response[orbitv1.RegisterResponderResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers = append(f.registers, req.Msg)
	if n := len(f.registers); n <= len(f.regErrs) && f.regErrs[n-1] != nil {
		return nil, f.regErrs[n-1]
	}
	return connect.NewResponse(&orbitv1.RegisterResponderResponse{ResponderId: "rs-1", Name: req.Msg.GetName(),
		LoomToken: LoomTokenPrefix + "LOOMLOOMLOOMLOOMLOOMLO", HeartbeatSeconds: 10}), nil
}

func (f *fakeJoin) Heartbeat(_ context.Context, req *connect.Request[orbitv1.HeartbeatRequest]) (*connect.Response[orbitv1.HeartbeatResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats = append(f.beats, req.Header().Get("Authorization"))
	if n := len(f.beats); n <= len(f.beatErrs) && f.beatErrs[n-1] != nil {
		return nil, f.beatErrs[n-1]
	}
	return connect.NewResponse(&orbitv1.HeartbeatResponse{ResponderId: "rs-1", HeartbeatSeconds: 10, LastSeenUnixNano: f.beatPrevNs}), nil
}

// fakeServer serves f over TLS and returns a pinned client for it.
func fakeServer(t *testing.T, f *fakeJoin) (orbitv1connect.JoinServiceClient, string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(orbitv1connect.NewJoinServiceHandler(f))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	pin := tlspin.Fingerprint(srv.Certificate())
	c, err := Client(srv.URL, pin)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv.URL, pin
}

func removedErr() error {
	e := connect.NewError(connect.CodeUnauthenticated, errors.New("responder removed"))
	e.Meta().Set("Orbit-Responder-State", "removed")
	return e
}

// Register writes the credential to .pending before it asks, so a request
// whose answer is lost is retried with the same credential; on success the
// config replaces the pending file. An existing config makes a
// re-registration that presents its credential; --fresh does not.
func TestClientRegister(t *testing.T) {
	reg := Registration{Name: "app1", Control: "198.51.100.20:9551", DataIP: "198.51.100.20", PortLow: 32768, PortHigh: 60999, UID: -1, GID: -1}
	t.Run("a lost answer is retried with the same credential", func(t *testing.T) {
		f := &fakeJoin{regErrs: []error{connect.NewError(connect.CodeUnavailable, errors.New("gone"))}}
		c, url, pin := fakeServer(t, f)
		conf := filepath.Join(t.TempDir(), "responder.yaml")
		r := reg
		r.Server, r.Pin = url, pin
		cfg, err := Register(context.Background(), c, Prefix+"AAAAAAAAAAAAAAAAAAAAAA", conf, r)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.registers) != 2 || f.registers[0].GetCredential() != f.registers[1].GetCredential() || f.registers[0].GetCredential() != cfg.Credential {
			t.Errorf("registers: %v", f.registers)
		}
		if _, err := os.Stat(conf + ".pending"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the pending file is still there: %v", err)
		}
		got, err := LoadConfig(conf)
		if err != nil || got != cfg {
			t.Errorf("config: %+v, %v", got, err)
		}
		if fi, _ := os.Stat(conf); fi.Mode().Perm() != 0o600 {
			t.Errorf("config mode %04o", fi.Mode().Perm())
		}
	})
	t.Run("a refusal leaves the pending file for the next run", func(t *testing.T) {
		f := &fakeJoin{regErrs: []error{connect.NewError(connect.CodeUnauthenticated, errors.New("unknown or expired join token"))}}
		c, url, pin := fakeServer(t, f)
		conf := filepath.Join(t.TempDir(), "responder.yaml")
		r := reg
		r.Server, r.Pin = url, pin
		if _, err := Register(context.Background(), c, Prefix+"AAAAAAAAAAAAAAAAAAAAAA", conf, r); err == nil {
			t.Fatal("registered")
		}
		if len(f.registers) != 1 {
			t.Errorf("a refusal was retried %d times", len(f.registers))
		}
		if _, err := os.Stat(conf); !errors.Is(err, os.ErrNotExist) {
			t.Error("a config was written")
		}
		first := f.registers[0].GetCredential()
		f.regErrs = nil
		if _, err := Register(context.Background(), c, Prefix+"BBBBBBBBBBBBBBBBBBBBBB", conf, r); err != nil {
			t.Fatal(err)
		}
		if f.registers[1].GetCredential() != first {
			t.Error("the next run did not resume the pending credential")
		}
	})
	t.Run("re-registration presents the existing credential; --fresh does not", func(t *testing.T) {
		f := &fakeJoin{}
		c, url, pin := fakeServer(t, f)
		conf := filepath.Join(t.TempDir(), "responder.yaml")
		r := reg
		r.Server, r.Pin = url, pin
		first, err := Register(context.Background(), c, Prefix+"AAAAAAAAAAAAAAAAAAAAAA", conf, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Register(context.Background(), c, Prefix+"BBBBBBBBBBBBBBBBBBBBBB", conf, r); err != nil {
			t.Fatal(err)
		}
		if f.registers[1].GetExistingCredential() != first.Credential || f.registers[1].GetCredential() == first.Credential {
			t.Errorf("re-registration: %v", f.registers[1])
		}
		r.Fresh = true
		if _, err := Register(context.Background(), c, Prefix+"CCCCCCCCCCCCCCCCCCCCCC", conf, r); err != nil {
			t.Fatal(err)
		}
		if f.registers[2].GetExistingCredential() != "" {
			t.Errorf("--fresh presented %q", f.registers[2].GetExistingCredential())
		}
	})
	t.Run("a config others can read is refused", func(t *testing.T) {
		conf := filepath.Join(t.TempDir(), "responder.yaml")
		if err := os.WriteFile(conf, []byte("credential: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(conf); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("err = %v", err)
		}
	})
}

// The heartbeat: the credential in the Authorization header; a failure
// backs off from 1 s, doubling to 30 s, and a success returns to the
// interval; an unknown credential keeps trying (logged); only "removed"
// stops it.
func TestHeartbeaterRun(t *testing.T) {
	unknown := connect.NewError(connect.CodeUnauthenticated, errors.New("the server does not know this responder"))
	f := &fakeJoin{beatErrs: []error{nil, unknown, unknown, unknown, unknown, unknown, unknown, unknown, nil, removedErr()}}
	c, _, _ := fakeServer(t, f)
	var waits []time.Duration
	var logs strings.Builder
	h := &Heartbeater{Client: c, Credential: testCred, ID: "rs-1", Log: slog.New(slog.NewTextHandler(&logs, nil)),
		jitter: func() float64 { return 0.5 }, // no jitter: 0.8 + 0.4*0.5 = 1
		after: func(d time.Duration) <-chan time.Time {
			waits = append(waits, d)
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		}}
	if err := h.Run(context.Background()); !errors.Is(err, ErrResponderRemoved) {
		t.Fatalf("run: %v", err)
	}
	want := []time.Duration{10 * time.Second, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		30 * time.Second, 30 * time.Second, 10 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("wait %d = %s, want %s", i, waits[i], want[i])
		}
	}
	for _, a := range f.beats {
		if a != "Responder "+testCred {
			t.Errorf("authorization %q", a)
		}
	}
	if strings.Count(logs.String(), "does not know this responder") != 1 || !strings.Contains(logs.String(), "heartbeat restored") {
		t.Errorf("logs:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), testCred) {
		t.Error("the log holds the credential")
	}
	// Jitter stays within ±20%.
	for _, j := range []float64{0, 1} {
		h.jitter = func() float64 { return j }
		if d := h.jittered(10 * time.Second); d < 8*time.Second || d > 12*time.Second {
			t.Errorf("jitter %v: %s", j, d)
		}
	}
}

// Check passes once the server has seen the service's own heartbeat, waits
// while it has not, and says why on removal or when the server does not
// know the credential.
func TestCheck(t *testing.T) {
	f := &fakeJoin{beatPrevNs: time.Now().UnixNano()}
	c, _, _ := fakeServer(t, f)
	if err := Check(context.Background(), c, testCred, time.Second); err != nil {
		t.Errorf("seen: %v", err)
	}
	f.beatPrevNs = 0
	if err := Check(context.Background(), c, testCred, 1500*time.Millisecond); err == nil || !strings.Contains(err.Error(), "has not sent a heartbeat") {
		t.Errorf("not seen: %v", err)
	}
	f.beatErrs = make([]error, 100)
	for i := range f.beatErrs {
		f.beatErrs[i] = removedErr()
	}
	f.beats = nil
	if err := Check(context.Background(), c, testCred, time.Second); !errors.Is(err, ErrResponderRemoved) {
		t.Errorf("removed: %v", err)
	}
}

const testCred = "orc1_ABCDEFGHIJKLMNOPQRSTUV"
