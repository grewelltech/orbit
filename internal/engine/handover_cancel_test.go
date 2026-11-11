package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/mockupf"
	"github.com/bgrewell/orbit/internal/testnet"
)

// A handover prepared and cancelled over the mock pair: with or without the
// acknowledge the UE stays on its source gNB with its session untouched,
// the target's prepared context is released and answered, and the UE then
// deregisters from the source, answering that release too (the wait for
// the acknowledge leaves no read behind to take it).
func TestCancelHandoverOverMockPair(t *testing.T) {
	tests := []struct {
		name    string
		net24   int
		opts    []mockamf.Option
		wantAck bool
	}{
		{name: "acknowledged", net24: testnet.EngineCancelAcked, wantAck: true},
		{name: "acknowledge withheld", net24: testnet.EngineCancelNoAck, opts: []mockamf.Option{mockamf.WithNoHandoverCancelAcknowledge()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false, append(tc.opts, mockamf.WithGUTI())...)
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100007530"
			sess := p.register(t, m, supi)
			dlBefore := sess.Result.DLTEID
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			start := time.Now()
			res, err := m.CancelHandover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3}, 400*time.Millisecond)
			if err != nil {
				t.Fatalf("CancelHandover: %v", err)
			}
			if res.Acknowledged != tc.wantAck || (tc.wantAck && res.AckAfter <= 0) {
				t.Errorf("result %+v, want acknowledged %v", res, tc.wantAck)
			}
			if !tc.wantAck && time.Since(start) < 400*time.Millisecond {
				t.Errorf("returned after %s, before the wait for the acknowledge ran out", time.Since(start))
			}
			if !res.TargetReleased {
				t.Error("the target's prepared context was not released")
			}
			if res.Target.ID != 2 || res.Target.MCC != p.cfg.MCC {
				t.Errorf("Target = %+v, want gNB 2 with the serving PLMN", res.Target)
			}
			if got := p.amf.HandoverCancels(); got != 1 {
				t.Errorf("mock AMF saw %d Handover Cancels, want 1", got)
			}
			if got := p.amf.HandoversCompleted(); got != 0 {
				t.Errorf("mock AMF completed %d handovers, want 0", got)
			}
			if sess.ServingGNB().ID != p.cfg.ID || sess.Result.DLTEID != dlBefore {
				t.Errorf("UE on gNB %d with DL TEID %#x, want gNB %d and %#x", sess.ServingGNB().ID, sess.Result.DLTEID, p.cfg.ID, dlBefore)
			}
			if us, ok := p.upf.Session(supi, 1); !ok || us.DL.Addr != p.srcN3 || us.DL.TEID != dlBefore {
				t.Errorf("UPF downlink %+v, want the source tunnel", us.DL)
			}
			if phase, _ := sess.Mobility(); phase != "" {
				t.Errorf("mobility phase %q after a cancelled handover, want none", phase)
			}
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			if err := m.Deregister(dctx, supi); err != nil {
				t.Errorf("Deregister from the source gNB: %v", err)
			}
			// One Complete from the target, one answering the switch-off;
			// the mock counts them as it reads them.
			deadline := time.Now().Add(2 * time.Second)
			for p.amf.ReleaseCompletes() < 2 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			sent, got := p.amf.DeregistrationReleaseIDs(), p.amf.ReleaseCompleteIDs()
			if len(sent) != 1 || len(got) != 2 || got[1] != sent[0] {
				t.Errorf("Release Completes %v, want the target's and then the switch-off's %v", got, sent)
			}
			if n := p.amf.StrayReleaseCompletes(); n != 0 {
				t.Errorf("%d stray UE Context Release Completes", n)
			}
		})
	}
}

// A handover that is never prepared has nothing to cancel: the error is
// ErrHandoverNotPrepared, no HANDOVER CANCEL is sent, and the UE stays on
// its source gNB.
func TestCancelHandoverNotPrepared(t *testing.T) {
	p := newMockPair(t, testnet.EngineCancelNoPrep, mockupf.Options{}, false, mockamf.WithHandoverAnswer(mockamf.FailHandoverPreparation), mockamf.WithGUTI())
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.n3.grace = 250 * time.Millisecond
	const supi = "208930100007531"
	sess := p.register(t, m, supi)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := m.CancelHandover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3}, time.Second)
	if !errors.Is(err, ErrHandoverNotPrepared) || res.Acknowledged {
		t.Fatalf("CancelHandover = %+v, %v; want ErrHandoverNotPrepared", res, err)
	}
	if got := p.amf.HandoverCancels(); got != 0 {
		t.Errorf("mock AMF saw %d Handover Cancels, want 0", got)
	}
	if sess.ServingGNB().ID != p.cfg.ID {
		t.Errorf("UE on gNB %d, want %d", sess.ServingGNB().ID, p.cfg.ID)
	}
	if _, err := m.CancelHandover(context.Background(), "208930100009999", GNBEndpoint{}, time.Second); err == nil {
		t.Error("CancelHandover for an unregistered UE succeeded")
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	if err := m.Deregister(dctx, supi); err != nil {
		t.Errorf("Deregister from the source gNB: %v", err)
	}
}

// A source association that ends during the wait for the acknowledge is
// reported in the result, not as an error: the cancel was sent. The wait is
// shorter than the target's grace, which must not turn the loss into a
// missing acknowledge.
func TestCancelHandoverSourceLost(t *testing.T) {
	p := newMockPair(t, testnet.EngineCancelLost, mockupf.Options{}, false, mockamf.WithCrashOnHandoverCancel(), mockamf.WithGUTI())
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.n3.grace = 250 * time.Millisecond
	const supi = "208930100007532"
	p.register(t, m, supi)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := m.CancelHandover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3}, 300*time.Millisecond)
	if err != nil || res.SourceLost == nil || res.Acknowledged || res.TargetReleased {
		t.Fatalf("CancelHandover = %+v, %v; want a lost source association and no error", res, err)
	}
	if got := p.amf.HandoverCancels(); got != 1 {
		t.Errorf("mock AMF saw %d Handover Cancels, want 1", got)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	_ = m.Deregister(dctx, supi)
}
