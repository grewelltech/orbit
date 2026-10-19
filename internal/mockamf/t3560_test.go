package mockamf_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

const t3560 = 50 * time.Millisecond

// recorder collects a probe's downlink events.
type recorder struct {
	mu  sync.Mutex
	evs []engine.DownlinkEvent
}

func (r *recorder) add(ev engine.DownlinkEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

// count returns how many events carried NAS type mt, and whether a UE
// Context Release was seen.
func (r *recorder) count(mt uint8) (n int, released bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.evs {
		if ev.NASType == mt {
			n++
		}
		if ev.Procedure == "UEContextRelease" {
			released = true
		}
	}
	return n, released
}

// attachWithProbe sets up NG on a fresh pipe and runs an attach that
// withholds the Authentication Response (if withhold), for at most d. after,
// if set, runs on the association before it closes.
func attachWithProbe(t *testing.T, amf *mockamf.AMF, d time.Duration, withhold bool, after func(gnb.Transport)) (*recorder, error) {
	t.Helper()
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	conn := amf.Connect(ctx)
	defer func() { _ = conn.Close() }()
	if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v (accepted=%v)", err, ng)
	}
	supi := "208930100007500"
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	probe := &engine.AttachProbe{OnDownlink: rec.add}
	if withhold {
		probe.Withhold = map[uint8]bool{f5nas.MsgTypeAuthenticationResponse: true}
	}
	_, err = engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}, Probe: probe,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if after != nil {
		after(conn)
	}
	return rec, err
}

// WithT3560 retransmits the Authentication Request on each expiry, retries
// times, then releases the UE; without it the request is never repeated.
// WithT3560NoRelease keeps the retransmissions and drops the release,
// WithNoAuthenticationRequest never challenges, and WithAuthenticationReject
// rejects instead.
func TestT3560Retransmission(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		wantRequests int
		wantReleased bool
		wantErr      func(error) bool
	}{
		{name: "four retransmissions then release", opts: []mockamf.Option{mockamf.WithT3560(t3560, 4)},
			wantRequests: 5, wantReleased: true, wantErr: func(err error) bool { return errors.Is(err, engine.ErrReleasedByCore) }},
		{name: "two retransmissions", opts: []mockamf.Option{mockamf.WithT3560(t3560, 2)},
			wantRequests: 3, wantReleased: true, wantErr: func(err error) bool { return errors.Is(err, engine.ErrReleasedByCore) }},
		{name: "five retransmissions", opts: []mockamf.Option{mockamf.WithT3560(t3560, 5)},
			wantRequests: 6, wantReleased: true, wantErr: func(err error) bool { return errors.Is(err, engine.ErrReleasedByCore) }},
		{name: "no release", opts: []mockamf.Option{mockamf.WithT3560(t3560, 4), mockamf.WithT3560NoRelease()},
			wantRequests: 5, wantErr: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }},
		{name: "no timer", wantRequests: 1, wantErr: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }},
		{name: "no authentication request", opts: []mockamf.Option{mockamf.WithNoAuthenticationRequest()},
			wantErr: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }},
		{name: "authentication reject", opts: []mockamf.Option{mockamf.WithAuthenticationReject()},
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "rejected authentication") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf := testAMFWith(t, tc.opts...)
			rec, err := attachWithProbe(t, amf, 12*t3560, true, nil)
			if !tc.wantErr(err) {
				t.Errorf("Attach err = %v", err)
			}
			n, released := rec.count(f5nas.MsgTypeAuthenticationRequest)
			if n != tc.wantRequests || released != tc.wantReleased {
				t.Errorf("saw %d Authentication Requests, released %v; want %d, %v", n, released, tc.wantRequests, tc.wantReleased)
			}
		})
	}
}

// An Authentication Response stops T3560: the attach completes and nothing
// more arrives over several T3560 periods.
func TestT3560StopsOnAuthenticationResponse(t *testing.T) {
	amf := testAMFWith(t, mockamf.WithT3560(t3560, 4))
	var late error
	rec, err := attachWithProbe(t, amf, 12*t3560, false, func(conn gnb.Transport) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*t3560)
		defer cancel()
		if _, err := gnb.ReadPDU(ctx, conn); err == nil {
			late = errors.New("a downlink PDU arrived after the attach")
		}
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if late != nil {
		t.Error(late)
	}
	if n, released := rec.count(f5nas.MsgTypeAuthenticationRequest); n != 1 || released {
		t.Errorf("saw %d Authentication Requests, released %v; want 1 and no release", n, released)
	}
}

// WithCrashAfterRelease: once the release has gone out, the mock closes
// every association, so a fresh NG Setup fails.
func TestCrashAfterRelease(t *testing.T) {
	amf := testAMFWith(t, mockamf.WithT3560(t3560, 4), mockamf.WithCrashAfterRelease())
	if _, err := attachWithProbe(t, amf, 12*t3560, true, nil); !errors.Is(err, engine.ErrReleasedByCore) {
		t.Fatalf("Attach err = %v, want the release", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	conn := amf.Connect(ctx)
	defer func() { _ = conn.Close() }()
	gnbCfg := gnb.Config{ID: 2, Name: "probe", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err == nil {
		t.Errorf("NG Setup answered after the release: %+v", ng)
	}
}

func testAMFWith(t *testing.T, opts ...mockamf.Option) *mockamf.AMF {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	a, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc,
		SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if n := a.EncodeErrors(); n != 0 {
			t.Errorf("mock AMF dropped %d PDUs that failed to encode", n)
		}
	})
	return a
}

// A second Initial UE Message for the same RAN UE NGAP ID on one association
// stops the first T3560 rather than orphaning it: the second attach, which
// answers, completes with nothing stale arriving after it.
func TestT3560RestartedByNewInitialUE(t *testing.T) {
	amf := testAMFWith(t, mockamf.WithT3560(t3560, 4))
	gnbCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	ctx, cancel := context.WithTimeout(context.Background(), 20*t3560)
	defer cancel()
	conn := amf.Connect(ctx)
	defer func() { _ = conn.Close() }()
	if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v", err)
	}
	supi := "208930100007500"
	id, _ := ue.ParseIdentity(supi, "208", "93", "0")
	cfg := engine.UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, stop := context.WithTimeout(ctx, t3560/2)
	cfg.Probe = &engine.AttachProbe{Withhold: map[uint8]bool{f5nas.MsgTypeAuthenticationResponse: true}}
	if _, err := engine.Attach(first, conn, gnbCfg, cfg, log, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first attach err = %v, want the deadline", err)
	}
	stop()
	cfg.Probe = nil
	if _, err := engine.Attach(ctx, conn, gnbCfg, cfg, log, nil); err != nil {
		t.Fatalf("second attach: %v", err)
	}
	late, lcancel := context.WithTimeout(ctx, 6*t3560)
	defer lcancel()
	if pdu, err := gnb.ReadPDU(late, conn); err == nil {
		t.Errorf("a stale downlink PDU arrived after the second attach: %+v", pdu.Present)
	}
}
