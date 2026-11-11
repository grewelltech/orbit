package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// ReregisterAfterRelease deregisters the UE, answers the release and
// registers the UE again on the same association under the next RAN UE
// NGAP ID. A core that answers leaves the UE registered with the Manager,
// to be deregistered as usual; one that drops the request, or sends no
// release, leaves it deregistered and the association closed.
func TestReregisterAfterRelease(t *testing.T) {
	const supi = "208930100000001"
	tests := []struct {
		name    string
		amfOpts []mockamf.Option
		gap     time.Duration
		// stopAfter closes the new registration's probe Stop after this
		// long; zero leaves the registration to run.
		stopAfter     time.Duration
		opts          []DeregisterOption
		wantErr       string // ReregisterAfterRelease's error, "" for none
		wantAttachErr error
		wantRegs      int   // Registration Requests the mock answered
		wantDropped   int64 // and dropped
		wantDials     int
	}{
		{name: "answered at once", wantRegs: 2, wantDials: 1},
		{name: "answered after a gap", gap: 30 * time.Millisecond, wantRegs: 2, wantDials: 1},
		{name: "dropped within the core's window", amfOpts: []mockamf.Option{mockamf.WithDropRegistrationAfterRelease(time.Hour)},
			stopAfter: 150 * time.Millisecond, wantAttachErr: ErrProbeStopped, wantRegs: 1, wantDropped: 1, wantDials: 1},
		{name: "sent after the core's window", amfOpts: []mockamf.Option{mockamf.WithDropRegistrationAfterRelease(40 * time.Millisecond)},
			gap: 250 * time.Millisecond, wantRegs: 2, wantDials: 1},
		{name: "no release", amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			wantErr: "the core sent no UE Context Release within 200ms", wantRegs: 1, wantDials: 1},
		{name: "no release within the call's own wait", amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()},
			opts: []DeregisterOption{WithReleaseWait(60 * time.Millisecond)}, wantErr: "the core sent no UE Context Release within 60ms", wantRegs: 1, wantDials: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, append([]mockamf.Option{mockamf.WithGUTI()}, tc.amfOpts...)...)
			if err != nil {
				t.Fatal(err)
			}
			_, cfg, _, _ := newBindTestAMF(t)
			d := &fakeN2Dialer{amf: amf}
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.dial = d.dial
			m.releaseLinger = 200 * time.Millisecond

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ueCfg := bindTestUE(t, supi, ki, opc)
			reg, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, ueCfg)
			if err != nil || !reg.Registered {
				t.Fatalf("Register: %v", err)
			}
			if tc.stopAfter > 0 {
				stop := make(chan struct{})
				timer := time.AfterFunc(tc.stopAfter, func() { close(stop) })
				defer timer.Stop()
				ueCfg.Probe = &AttachProbe{Stop: stop}
			}
			res, err := m.ReregisterAfterRelease(ctx, supi, ueCfg, tc.gap, tc.opts...)
			if tc.wantErr != "" {
				if !errors.Is(err, ErrNoReregistration) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want ErrNoReregistration with %q", err, tc.wantErr)
				}
				if res == nil || res.Deregistration.Release != nil || !res.RequestedAt.IsZero() || res.Attach != nil {
					t.Errorf("result %+v, want a deregistration without a release and no request", res)
				}
			} else {
				if err != nil {
					t.Fatalf("ReregisterAfterRelease: %v", err)
				}
				rel := res.Deregistration.Release
				if rel == nil || rel.RANUENGAPID != reg.RANUENGAPID || res.RANUENGAPID != reg.RANUENGAPID+1 {
					t.Fatalf("released %+v, re-registered as RAN UE NGAP ID %d; want a release of %d and %d", rel, res.RANUENGAPID, reg.RANUENGAPID, reg.RANUENGAPID+1)
				}
				if got := res.RequestedAt.Sub(res.CompletedAt); res.RequestedAt.IsZero() || got < tc.gap {
					t.Errorf("request %s after the complete, want at least the gap of %s", got, tc.gap)
				}
				if !errors.Is(res.AttachErr, tc.wantAttachErr) || (res.Attach != nil) != (tc.wantAttachErr == nil) {
					t.Errorf("attach %+v, err %v; want err %v", res.Attach, res.AttachErr, tc.wantAttachErr)
				}
			}
			registered := res != nil && res.Attach != nil
			if _, err := m.Status(supi); (err == nil) != registered {
				t.Errorf("Status err = %v with the UE registered again = %v", err, registered)
			}
			if registered {
				if res.Attach.RANUENGAPID != res.RANUENGAPID || !res.Attach.Registered || res.Attach.N2Source != reg.N2Source {
					t.Errorf("attach %+v, want registered as %d over %s", res.Attach, res.RANUENGAPID, reg.N2Source)
				}
				// The UE the Manager now holds deregisters as any other,
				// and that closes the association.
				dres, err := m.DeregisterWith(ctx, supi)
				if err != nil || dres.Release == nil || dres.RANUENGAPID != res.RANUENGAPID {
					t.Errorf("deregistering the re-registered UE: %+v, %v", dres, err)
				}
			}
			if got := len(amf.RegistrationSUPIs()); got != tc.wantRegs {
				t.Errorf("the mock answered %d Registration Requests, want %d", got, tc.wantRegs)
			}
			if got := amf.DroppedRegistrations(); got != tc.wantDropped {
				t.Errorf("the mock dropped %d Registration Requests, want %d", got, tc.wantDropped)
			}
			if laddrs, _ := d.dialed(); len(laddrs) != tc.wantDials {
				t.Errorf("%d associations dialled, want %d: the UE registers again over the one it had", len(laddrs), tc.wantDials)
			}
			if n := amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
		})
	}
}

// A UE the Manager does not hold cannot be registered again.
func TestReregisterAfterReleaseUnknownUE(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.ReregisterAfterRelease(context.Background(), "208930100000009", UEConfig{}, 0); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Errorf("err = %v, want not registered", err)
	}
}

// A normal deregistration is refused, with the UE left registered.
func TestReregisterAfterReleaseSwitchOffOnly(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = (&fakeN2Dialer{amf: amf}).dial
	const supi = "208930100000001"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ueCfg := bindTestUE(t, supi, ki, opc)
	if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, ueCfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.ReregisterAfterRelease(ctx, supi, ueCfg, 0, WithNormalDeregistration(time.Second)); err == nil || !strings.Contains(err.Error(), "switch-off deregistration only") {
		t.Errorf("err = %v, want a refusal", err)
	}
	if _, err := m.Status(supi); err != nil {
		t.Errorf("the refused call dropped the UE: %v", err)
	}
}

// noDelayConn is an association that records SetNoDelay.
type noDelayConn struct {
	n2Conn
	set atomic.Int32 // 1 after SetNoDelay(true), 2 after SetNoDelay(false)
}

func (c *noDelayConn) SetNoDelay(on bool) error {
	if on {
		c.set.Store(1)
	} else {
		c.set.Store(2)
	}
	return nil
}

// The association is set to send each PDU as it is written before the
// deregistration goes out, so the release's answer and the new request are
// not held apart by the transport.
func TestReregisterAfterReleaseSetsNoDelay(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203",
	}, mockamf.WithGUTI())
	if err != nil {
		t.Fatal(err)
	}
	_, cfg, _, _ := newBindTestAMF(t)
	d := &fakeN2Dialer{amf: amf}
	var conn *noDelayConn
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = func(ctx context.Context, laddr, raddr string) (n2Conn, error) {
		c, err := d.dial(ctx, laddr, raddr)
		conn = &noDelayConn{n2Conn: c}
		return conn, err
	}
	const supi = "208930100000001"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ueCfg := bindTestUE(t, supi, ki, opc)
	if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, ueCfg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := conn.set.Load(); got != 0 {
		t.Fatalf("a registration set SCTP_NODELAY (%d)", got)
	}
	res, err := m.ReregisterAfterRelease(ctx, supi, ueCfg, 0)
	if err != nil || res.Attach == nil {
		t.Fatalf("ReregisterAfterRelease: %+v, %v", res, err)
	}
	if got := conn.set.Load(); got != 1 {
		t.Errorf("SetNoDelay state %d, want it set on and left on", got)
	}
	if err := m.Deregister(ctx, supi); err != nil {
		t.Error(err)
	}
}
