package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// RegisterAfterOtherRelease deregisters one UE and, at its release,
// registers another on a second association set up beforehand. A core
// that answers leaves the other UE registered with the Manager; one that
// drops its request, or sends no release, leaves neither registered and
// both associations closed; a second gNB the core does not set up leaves
// the first UE registered and untouched.
func TestRegisterAfterOtherRelease(t *testing.T) {
	const supiA, supiB = "208930100000001", "208930100000002"
	tests := []struct {
		name    string
		amfOpts []mockamf.Option
		gap     time.Duration
		// otherMCC overrides the second gNB's MCC, "" keeps it.
		otherMCC string
		// stopAfter closes the other registration's probe Stop after this
		// long; zero leaves the registration to run.
		stopAfter     time.Duration
		opts          []DeregisterOption
		wantErr       error  // the call's error, nil for none
		wantErrText   string // and a fragment of its text
		wantAttachErr error
		wantRegs      []string // the SUPIs whose Registration Requests the mock answered
		wantDropped   int64
		wantDeregs    int64
		wantARegd     bool // the first UE is still registered after the call
	}{
		{name: "answered at once", wantRegs: []string{supiA, supiB}, wantDeregs: 1},
		{name: "answered after a gap", gap: 30 * time.Millisecond, wantRegs: []string{supiA, supiB}, wantDeregs: 1},
		{name: "dropped within the core's window", amfOpts: []mockamf.Option{mockamf.WithDropRegistrationAfterAnyRelease(time.Hour)},
			stopAfter: 150 * time.Millisecond, wantAttachErr: ErrProbeStopped, wantRegs: []string{supiA}, wantDropped: 1, wantDeregs: 1},
		{name: "sent after the core's window", amfOpts: []mockamf.Option{mockamf.WithDropRegistrationAfterAnyRelease(40 * time.Millisecond)},
			gap: 250 * time.Millisecond, wantRegs: []string{supiA, supiB}, wantDeregs: 1},
		{name: "a same-SUPI window spares another SUPI", amfOpts: []mockamf.Option{mockamf.WithDropRegistrationAfterRelease(time.Hour)},
			wantRegs: []string{supiA, supiB}, wantDeregs: 1},
		{name: "no release", amfOpts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()}, opts: []DeregisterOption{WithReleaseWait(60 * time.Millisecond)},
			wantErr: ErrNoReregistration, wantErrText: "the core sent no UE Context Release within 60ms", wantRegs: []string{supiA}, wantDeregs: 1},
		{name: "second gNB not set up", otherMCC: "001", wantErr: ErrNGSetup, wantErrText: "NG Setup rejected",
			wantRegs: []string{supiA}, wantARegd: true},
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
			if reg, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supiA, ki, opc)); err != nil || !reg.Registered {
				t.Fatalf("Register: %v", err)
			}
			other := cfg
			other.ID, other.Name = 2, "orbit-gnb-2"
			if tc.otherMCC != "" {
				other.MCC = tc.otherMCC
			}
			ueB := bindTestUE(t, supiB, ki, opc)
			if tc.stopAfter > 0 {
				stop := make(chan struct{})
				timer := time.AfterFunc(tc.stopAfter, func() { close(stop) })
				defer timer.Stop()
				ueB.Probe = &AttachProbe{Stop: stop}
			}
			res, err := m.RegisterAfterOtherRelease(ctx, supiA, GNBSpec{Config: other, AMFAddr: "amf.test:38412"}, ueB, tc.gap, tc.opts...)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("err = %v, want %v with %q", err, tc.wantErr, tc.wantErrText)
				}
				if res != nil && (!res.RequestedAt.IsZero() || res.Attach != nil) {
					t.Errorf("result %+v, want no request sent", res)
				}
			} else {
				if err != nil {
					t.Fatalf("RegisterAfterOtherRelease: %v", err)
				}
				if res.Deregistration == nil || res.Deregistration.Release == nil {
					t.Fatalf("result %+v, want the first UE released", res)
				}
				if got := res.RequestedAt.Sub(res.CompletedAt); res.RequestedAt.IsZero() || got < tc.gap {
					t.Errorf("request %s after the complete, want at least the gap of %s", got, tc.gap)
				}
				if !errors.Is(res.AttachErr, tc.wantAttachErr) || (res.Attach != nil) != (tc.wantAttachErr == nil) {
					t.Errorf("attach %+v, err %v; want err %v", res.Attach, res.AttachErr, tc.wantAttachErr)
				}
			}
			if _, err := m.Status(supiA); (err == nil) != tc.wantARegd {
				t.Errorf("first UE: Status err = %v, want registered %v", err, tc.wantARegd)
			}
			registered := res != nil && res.Attach != nil
			if _, err := m.Status(supiB); (err == nil) != registered {
				t.Errorf("other UE: Status err = %v, want registered %v", err, registered)
			}
			if registered {
				if !res.Attach.Registered || res.Attach.N2Source != res.N2Source {
					t.Errorf("attach %+v, want registered over %s", res.Attach, res.N2Source)
				}
				if dres, err := m.DeregisterWith(ctx, supiB); err != nil || dres.Release == nil {
					t.Errorf("deregistering the other UE: %+v, %v", dres, err)
				}
			}
			if tc.wantARegd {
				if err := m.Deregister(ctx, supiA); err != nil {
					t.Errorf("deregistering the first UE: %v", err)
				}
			}
			if got := amf.RegistrationSUPIs(); strings.Join(got, ",") != strings.Join(tc.wantRegs, ",") {
				t.Errorf("the mock answered Registration Requests of %v, want %v", got, tc.wantRegs)
			}
			if got := amf.DroppedRegistrations(); got != tc.wantDropped {
				t.Errorf("the mock dropped %d Registration Requests, want %d", got, tc.wantDropped)
			}
			wantDeregs := tc.wantDeregs
			if registered || tc.wantARegd {
				wantDeregs++
			}
			if got := amf.Deregistrations(); got != wantDeregs {
				t.Errorf("the mock saw %d deregistrations, want %d", got, wantDeregs)
			}
			if laddrs, _ := d.dialed(); len(laddrs) != 2 {
				t.Errorf("%d associations dialled, want 2: one per gNB", len(laddrs))
			}
			if n := amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
		})
	}
}

// The call refuses before dialling: a first UE the Manager does not hold,
// another UE it already holds, and a normal deregistration.
func TestRegisterAfterOtherReleaseRefusals(t *testing.T) {
	amf, cfg, ki, opc := newBindTestAMF(t)
	const supiA, supiB = "208930100000001", "208930100000002"
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	d := &fakeN2Dialer{amf: amf}
	m.dial = d.dial
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}
	for _, supi := range []string{supiA, supiB} {
		if _, err := m.Register(ctx, g, bindTestUE(t, supi, ki, opc)); err != nil {
			t.Fatalf("Register %s: %v", supi, err)
		}
	}
	tests := []struct {
		name    string
		supi    string
		other   string
		opts    []DeregisterOption
		wantErr string
	}{
		{name: "unknown first UE", supi: "208930100000009", other: "208930100000003", wantErr: "is not registered"},
		{name: "other UE already registered", supi: supiA, other: supiB, wantErr: "already registered"},
		{name: "normal deregistration", supi: supiA, other: "208930100000003", opts: []DeregisterOption{WithNormalDeregistration(time.Second)},
			wantErr: "switch-off deregistration only"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.RegisterAfterOtherRelease(ctx, tc.supi, g, bindTestUE(t, tc.other, ki, opc), 0, tc.opts...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if laddrs, _ := d.dialed(); len(laddrs) != 2 {
		t.Errorf("%d associations dialled, want only the two registrations'", len(laddrs))
	}
	for _, supi := range []string{supiA, supiB} {
		if _, err := m.Status(supi); err != nil {
			t.Errorf("a refused call dropped %s: %v", supi, err)
		}
	}
}

// Both associations are set to send each PDU as it is written before the
// deregistration goes out, so the first UE's release complete and the
// other UE's request are not held back by the transport.
func TestRegisterAfterOtherReleaseSetsNoDelay(t *testing.T) {
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
	var conns []*noDelayConn
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = func(ctx context.Context, laddr, raddr string) (n2Conn, error) {
		c, err := d.dial(ctx, laddr, raddr)
		conn := &noDelayConn{n2Conn: c}
		conns = append(conns, conn)
		return conn, err
	}
	const supiA, supiB = "208930100000001", "208930100000002"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, supiA, ki, opc)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	other := cfg
	other.ID = 2
	res, err := m.RegisterAfterOtherRelease(ctx, supiA, GNBSpec{Config: other, AMFAddr: "amf.test:38412"}, bindTestUE(t, supiB, ki, opc), 0)
	if err != nil || res.Attach == nil {
		t.Fatalf("RegisterAfterOtherRelease: %+v, %v", res, err)
	}
	if len(conns) != 2 {
		t.Fatalf("%d associations dialled, want 2", len(conns))
	}
	for i, c := range conns {
		if got := c.set.Load(); got != 1 {
			t.Errorf("association %d: SetNoDelay state %d, want it set on", i, got)
		}
	}
	if err := m.Deregister(ctx, supiB); err != nil {
		t.Error(err)
	}
}
