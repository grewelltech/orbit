package engine

import (
	"context"
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

// An N2 handover over the mock pair completes: the result carries the
// HANDOVER COMMAND's PDU Session Resource Handover List as received, the
// UPF's downlink moves to the target's tunnel, and the UE deregisters from
// the target gNB.
func TestN2HandoverOverMockPair(t *testing.T) {
	tests := []struct {
		name     string
		net24    int
		answer   mockamf.HandoverAnswer
		wantList bool
	}{
		{name: "completes", net24: testnet.EngineN2Completes, wantList: true},
		{name: "command without the list", net24: testnet.EngineN2NoList, answer: mockamf.CommandWithoutList},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false, mockamf.WithHandoverAnswer(tc.answer), mockamf.WithGUTI())
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100007520"
			sess := p.register(t, m, supi)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := m.Handover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3})
			if err != nil {
				t.Fatalf("Handover: %v", err)
			}
			if res.PathSwitchAck != nil {
				t.Errorf("PathSwitchAck = %+v on an N2 handover", res.PathSwitchAck)
			}
			cmd := res.Command
			if cmd == nil || cmd.ListPresent != tc.wantList {
				t.Fatalf("Command = %+v, want list present %v", cmd, tc.wantList)
			}
			if tc.wantList && (len(cmd.Items) != 1 || cmd.Items[0].PDUSessionID != 1 || !cmd.Items[0].HasTransfer()) {
				t.Errorf("Command items = %+v, want session 1 with a transfer", cmd.Items)
			}
			if got := p.amf.HandoversCompleted(); got != 1 {
				t.Errorf("mock AMF completed %d handovers, want 1", got)
			}
			if sess.ServingGNB().ID != 2 {
				t.Errorf("UE on gNB %d, want 2", sess.ServingGNB().ID)
			}
			us, ok := p.upf.Session(supi, 1)
			if !ok || us.DL.Addr != p.tgtN3 || us.DL.TEID != sess.Result.DLTEID {
				t.Errorf("UPF downlink %+v, want the target tunnel %s TEID %#x", us.DL, p.tgtN3, sess.Result.DLTEID)
			}
			dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dcancel()
			if err := m.Deregister(dctx, supi); err != nil {
				t.Errorf("Deregister from the target gNB: %v", err)
			}
			if got := p.amf.Deregistrations(); got != 1 {
				t.Errorf("mock AMF saw %d deregistrations, want 1", got)
			}
		})
	}
}

// A HANDOVER REQUIRED the core answers with a HANDOVER PREPARATION FAILURE,
// or not at all, fails without a command, and the UE stays on its source
// gNB, from which it still deregisters.
func TestN2HandoverPreparationFails(t *testing.T) {
	tests := []struct {
		name    string
		net24   int
		answer  mockamf.HandoverAnswer
		wait    time.Duration
		wantErr string
	}{
		{name: "preparation failure", net24: testnet.EngineN2PrepFailure, answer: mockamf.FailHandoverPreparation, wait: 2 * time.Second,
			wantErr: "await HandoverRequest"},
		{name: "no answer", net24: testnet.EngineN2Unanswered, answer: mockamf.IgnoreHandoverRequired, wait: time.Second,
			wantErr: "await HandoverRequest"},
		{name: "failure after the acknowledge", net24: testnet.EngineN2FailAfterAck, answer: mockamf.FailHandoverAfterAcknowledge, wait: 10 * time.Second,
			wantErr: "after the target's HandoverRequestAcknowledge: HANDOVER PREPARATION FAILURE, cause radio-network/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newMockPair(t, tc.net24, mockupf.Options{}, false, mockamf.WithHandoverAnswer(tc.answer), mockamf.WithGUTI())
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.n3.grace = 250 * time.Millisecond
			const supi = "208930100007521"
			p.register(t, m, supi)
			ctx, cancel := context.WithTimeout(context.Background(), tc.wait)
			defer cancel()
			res, err := m.Handover(ctx, supi, GNBEndpoint{Config: gnb.Config{ID: 2, Name: "orbit-gnb-2"}, AMFAddr: p.amfAddr, N3Addr: p.tgtN3})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Handover = %v, want an error naming %q", err, tc.wantErr)
			}
			if res.Command != nil {
				t.Errorf("Command = %+v on a handover that reached none", res.Command)
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
