package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// countingUPF counts the sessions the mock AMF reports up.
type countingUPF struct{ up atomic.Int64 }

func (u *countingUPF) SessionUp(mockamf.SessionInfo)   { u.up.Add(1) }
func (u *countingUPF) PathSwitched(mockamf.PathSwitch) {}

// probeFixture is a UE registered on a mock AMF with a NAS probe open.
type probeFixture struct {
	amf *mockamf.AMF
	upf *countingUPF
	m   *Manager
	p   *NASProbe
}

const probeSUPI = "208930100000031"

func newProbeFixture(t *testing.T, opts ...mockamf.Option) *probeFixture {
	t.Helper()
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	upf := &countingUPF{}
	amf, err := mockamf.New(mockamf.Config{
		Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
		MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: upf, UPFAddress: "127.0.0.1",
	}, append([]mockamf.Option{mockamf.WithGUTI()}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeN2Dialer{amf: amf}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.dial = d.dial
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	if _, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: "amf.test:38412"}, bindTestUE(t, probeSUPI, ki, opc)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := m.ProbeNAS(ctx, probeSUPI)
	if err != nil {
		t.Fatalf("ProbeNAS: %v", err)
	}
	t.Cleanup(p.Close)
	return &probeFixture{amf: amf, upf: upf, m: m, p: p}
}

// sessionRequest protects a PDU Session Establishment Request for session
// psi with transaction pti at the UE's next uplink NAS COUNT.
func (f *probeFixture) sessionRequest(t *testing.T, psi, pti uint8) []byte {
	t.Helper()
	params := ue.PDUSessionParams{PDUSessionID: psi, PTI: pti, SST: 1, SD: "010203", DNN: "internet"}
	sm, err := ue.BuildPDUSessionEstablishmentRequest(params)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ue.BuildULNASTransportForPDUSession(sm, params)
	if err != nil {
		t.Fatal(err)
	}
	pdu, err := f.p.Protect(msg)
	if err != nil {
		t.Fatal(err)
	}
	return pdu
}

// A protected request is answered: the probe reads the PDU Session Resource
// Setup Request, decodes its NAS and answers it, so the mock reports the
// session up. With nothing more to read, Next is quiet.
func TestNASProbeRequestAnswered(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	if err := f.p.Send(f.sessionRequest(t, 2, 9)); err != nil {
		t.Fatal(err)
	}
	d, err := f.p.Next(ctx, 2*time.Second)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if d.Procedure != "PDUSessionResourceSetup" || len(d.Sessions) != 1 || d.Sessions[0] != 2 || d.Released {
		t.Errorf("downlink = %+v, want a setup of session 2", d)
	}
	if len(d.NAS) != 1 || d.NAS[0].Err != nil || d.NAS[0].MessageType() != f5nas.MsgTypeDLNASTransport {
		t.Fatalf("NAS = %+v, want one decoded DL NAS Transport", d.NAS)
	}
	sm, err := ue.ExtractN1SMContainer(d.NAS[0].Msg)
	if err != nil || len(sm) < 4 || sm[1] != 2 || sm[2] != 9 || sm[3] != f5nas.MsgTypePDUSessionEstablishmentAccept {
		t.Errorf("5GSM payload %x (%v), want an Establishment Accept for session 2, PTI 9", sm, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.upf.up.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.upf.up.Load() != 1 {
		t.Error("the mock did not see the PDU Session Resource Setup Response")
	}
	if _, err := f.p.Next(ctx, 50*time.Millisecond); !errors.Is(err, ErrProbeQuiet) {
		t.Errorf("Next with nothing pending = %v, want ErrProbeQuiet", err)
	}
	if g := f.p.GUTI(); len(g) != 11 {
		t.Errorf("GUTI = %x, want the 11-octet 5G-GUTI the mock assigned", g)
	}
}

// A request sent with its MAC inverted is what the mock's option makes of
// it: dropped, answered, a 5GMM STATUS, or a release, which the probe
// answers and reports.
func TestNASProbeAlteredUplink(t *testing.T) {
	tests := []struct {
		name         string
		mode         mockamf.UplinkAction
		wantQuiet    bool
		wantSessions int
		wantReleased bool
		wantNASType  uint8
	}{
		{name: "discarded", mode: mockamf.DiscardUplink, wantQuiet: true},
		{name: "processed", mode: mockamf.ProcessUplink, wantSessions: 1, wantNASType: f5nas.MsgTypeDLNASTransport},
		{name: "status", mode: mockamf.StatusOnUplink, wantNASType: f5nas.MsgTypeStatus5GMM},
		{name: "released", mode: mockamf.ReleaseOnUplink, wantReleased: true},
		{name: "muted", mode: mockamf.MuteOnUplink, wantQuiet: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newProbeFixture(t, mockamf.WithBadMACUplink(tc.mode))
			pdu := f.sessionRequest(t, 2, 1)
			for i := 2; i < 6; i++ {
				pdu[i] ^= 0xff
			}
			if err := f.p.Send(pdu); err != nil {
				t.Fatal(err)
			}
			d, err := f.p.Next(context.Background(), 300*time.Millisecond)
			if tc.wantQuiet {
				if !errors.Is(err, ErrProbeQuiet) {
					t.Fatalf("Next = %+v, %v, want ErrProbeQuiet", d, err)
				}
			} else {
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if len(d.Sessions) != tc.wantSessions || d.Released != tc.wantReleased {
					t.Errorf("downlink = %+v, want %d sessions, released %v", d, tc.wantSessions, tc.wantReleased)
				}
				if tc.wantNASType != 0 && (len(d.NAS) != 1 || d.NAS[0].Err != nil || d.NAS[0].MessageType() != tc.wantNASType) {
					t.Errorf("NAS = %+v, want one message of type %#x", d.NAS, tc.wantNASType)
				}
			}
			if got := f.amf.BadMACUplinks(); got != 1 {
				t.Errorf("mock counted %d bad-MAC uplinks, want 1", got)
			}
			if tc.wantReleased {
				deadline := time.Now().Add(2 * time.Second)
				for f.amf.ReleaseCompletes() != 1 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if f.amf.ReleaseCompletes() != 1 {
					t.Error("the release was not answered with UE Context Release Complete")
				}
			}
			// A muted mock drops the next valid request too; the others
			// (but the released one) still answer it.
			if tc.wantReleased {
				return
			}
			if err := f.p.Send(f.sessionRequest(t, 3, 2)); err != nil {
				t.Fatal(err)
			}
			d, err = f.p.Next(context.Background(), 300*time.Millisecond)
			if muted := tc.mode == mockamf.MuteOnUplink; muted != errors.Is(err, ErrProbeQuiet) {
				t.Errorf("valid request after the bad MAC: %+v, %v (muted %v)", d, err, muted)
			}
		})
	}
}

// Deregister reports whether the core released the UE, and Close drops the
// session: the Manager no longer knows the UE, and a second Close is a
// no-op.
func TestNASProbeDeregisterAndClose(t *testing.T) {
	tests := []struct {
		name         string
		opts         []mockamf.Option
		wantReleased bool
	}{
		{name: "released", wantReleased: true},
		{name: "no release", opts: []mockamf.Option{mockamf.WithNoReleaseOnDeregistration()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newProbeFixture(t, tc.opts...)
			released, err := f.p.Deregister(context.Background(), 200*time.Millisecond)
			if err != nil || released != tc.wantReleased {
				t.Errorf("Deregister = %v, %v, want %v", released, err, tc.wantReleased)
			}
			if got := f.amf.Deregistrations(); got != 1 {
				t.Errorf("mock saw %d Deregistration Requests, want 1", got)
			}
			f.p.Close()
			f.p.Close()
			if _, err := f.m.Status(probeSUPI); err == nil {
				t.Error("the UE is still in the Manager after Close")
			}
			if _, err := f.p.Next(context.Background(), 200*time.Millisecond); !errors.Is(err, ErrAssociationLost) && !errors.Is(err, ErrProbeQuiet) {
				t.Errorf("Next after Close = %v", err)
			}
		})
	}
}

// ProbeNAS refuses a UE the Manager does not hold, and Next ends with the
// caller's context.
func TestNASProbeErrors(t *testing.T) {
	f := newProbeFixture(t)
	if _, err := f.m.ProbeNAS(context.Background(), "208930100009999"); err == nil {
		t.Error("ProbeNAS on an unknown UE succeeded")
	}
	// The probe holds the procedure lock: a second one waits for ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.m.ProbeNAS(ctx, probeSUPI); err == nil {
		t.Error("a second ProbeNAS did not wait for the first")
	}
	if _, err := f.p.Next(ctx, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Next under an expired context = %v", err)
	}
}
