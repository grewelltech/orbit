package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// A completed Xn handover reports the Acknowledge's Switched List as it was
// received: the session, whether its transfer is there, and whether that
// names a new uplink endpoint.
func TestXnHandoverReportsTheAcknowledge(t *testing.T) {
	tests := []struct {
		name         string
		net24        int
		answer       mockamf.PathSwitchAnswer
		wantTransfer bool
		wantULTNL    bool
	}{
		{name: "uplink tunnel kept", net24: testnet.EngineXnAckKeptUL, wantTransfer: true},
		{name: "uplink tunnel reallocated", net24: testnet.EngineXnAckNewUL, answer: mockamf.AckWithNewUL, wantTransfer: true, wantULTNL: true},
		{name: "transfer omitted", net24: testnet.EngineXnAckNoTransfer, answer: mockamf.AckWithoutTransfer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false, mockamf.WithPathSwitchAnswer(tc.answer))
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100007510"
			p.register(t, m, supi)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := m.XnHandover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3})
			if err != nil {
				t.Fatalf("XnHandover: %v", err)
			}
			ack := res.PathSwitchAck
			if ack == nil || !ack.ListPresent || len(ack.Items) != 1 {
				t.Fatalf("PathSwitchAck = %+v, want a Switched List of one item", ack)
			}
			it := ack.Items[0]
			if it.PDUSessionID != 1 || it.HasTransfer() != tc.wantTransfer || it.HasULTNL != tc.wantULTNL {
				t.Errorf("item = %+v, want session 1, transfer %v, UL TNL %v", it, tc.wantTransfer, tc.wantULTNL)
			}
		})
	}
}

// A Path Switch Request the core answers with a Failure, or not at all,
// fails with ErrPathSwitchNotAcknowledged, and the UE stays on its source
// gNB, from which it still deregisters.
func TestXnHandoverNotAcknowledged(t *testing.T) {
	tests := []struct {
		name    string
		net24   int
		answer  mockamf.PathSwitchAnswer
		wait    time.Duration
		wantErr string
		// refused is whether the error is ErrPathSwitchRefused.
		refused bool
	}{
		{name: "failure", net24: testnet.EngineXnNotAckFailure, answer: mockamf.FailPathSwitch, wait: 15 * time.Second,
			wantErr: "the core answered with PATH SWITCH REQUEST FAILURE", refused: true},
		{name: "no answer", net24: testnet.EngineXnNotAckSilence, answer: mockamf.IgnorePathSwitch, wait: time.Second,
			wantErr: "await PathSwitch response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false, mockamf.WithPathSwitchAnswer(tc.answer), mockamf.WithGUTI())
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100007511"
			p.register(t, m, supi)
			ctx, cancel := context.WithTimeout(context.Background(), tc.wait)
			defer cancel()
			res, err := m.XnHandover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3})
			if !errors.Is(err, ErrPathSwitchNotAcknowledged) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("XnHandover = %v, want ErrPathSwitchNotAcknowledged naming %q", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrPathSwitchRefused); got != tc.refused {
				t.Errorf("XnHandover = %v: ErrPathSwitchRefused %v, want %v", err, got, tc.refused)
			}
			if res.PathSwitchAck != nil {
				t.Errorf("PathSwitchAck = %+v on a failed handover", res.PathSwitchAck)
			}
			sess, err := m.Status(supi)
			if err != nil || sess.ServingGNB().ID != p.cfg.ID {
				t.Fatalf("after the failed handover: session %v, err %v, want the UE on gNB %d", sess, err, p.cfg.ID)
			}
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			if err := m.Deregister(dctx, supi); err != nil {
				t.Errorf("Deregister from the source gNB: %v", err)
			}
			if got := p.amf.Deregistrations(); got != 1 {
				t.Errorf("mock AMF saw %d deregistrations, want 1", got)
			}
		})
	}
}

// XnHandover sends the capabilities the AMF supplied, so the mock sees no
// mismatch and its Acknowledge carries none. XnHandoverWith with
// WithPathSwitchCapabilities sends the given ones instead: the mock counts
// the mismatch, its Acknowledge's capabilities are reported, and the
// session holds the AMF's copy afterwards, never the ones sent.
func TestXnHandoverWithPathSwitchCapabilities(t *testing.T) {
	p := newMockPair(t, testnet.EngineXnCapsOption, mockupf.Options{}, false, mockamf.WithStoredCapabilitiesInPathSwitchAck(), mockamf.WithGUTI())
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.n3.grace = 250 * time.Millisecond
	const supi = "208930100007512"
	sess := p.register(t, m, supi)
	supplied := sess.UESecurityCapabilities()
	if supplied == nil {
		t.Fatal("the mock supplied no UE security capabilities")
	}
	other := gnb.NewUESecurityCapabilities(0x2000, 0x2000, 0, 0)
	if gnb.SameUESecurityCapabilities(supplied, other) {
		t.Fatal("the stimulus equals the supplied capabilities")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := m.XnHandoverWith(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3},
		WithPathSwitchCapabilities(other))
	if err != nil {
		t.Fatalf("XnHandoverWith: %v", err)
	}
	if got := p.amf.PathSwitchCapabilityMismatches(); got != 1 {
		t.Errorf("mock counted %d capability mismatches, want 1", got)
	}
	if !gnb.SameUESecurityCapabilities(res.PathSwitchCapabilities, supplied) {
		t.Errorf("Acknowledge capabilities %+v, want the supplied ones", res.PathSwitchCapabilities)
	}
	if !gnb.SameUESecurityCapabilities(sess.UESecurityCapabilities(), supplied) {
		t.Errorf("session capabilities %+v after the handover, want the supplied ones", sess.UESecurityCapabilities())
	}

	// Back without the option: the session's capabilities go out.
	res, err = m.XnHandover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 3, Name: "orbit-gnb-3"}, AMFAddr: p.amfAddr, N3Addr: p.srcN3})
	if err != nil {
		t.Fatalf("XnHandover: %v", err)
	}
	if got := p.amf.PathSwitchCapabilityMismatches(); got != 1 {
		t.Errorf("mock counted %d capability mismatches after a plain Xn handover, want still 1", got)
	}
	if res.PathSwitchCapabilities != nil {
		t.Errorf("Acknowledge capabilities %+v without a mismatch, want none", res.PathSwitchCapabilities)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	if err := m.Deregister(dctx, supi); err != nil {
		t.Errorf("Deregister: %v", err)
	}
}
