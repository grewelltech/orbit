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
	"github.com/bgrewell/orbit/internal/mockamf/sctpfront"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// AttachProbe.BeforeRegistration runs on the UE's own association after NG
// Setup and before the Initial UE Message: a RAN Configuration Update it
// runs is answered there, and the mock has seen no Registration Request
// yet. With a nil return the UE registers on that association; an error
// ends the attach, wrapped, with no Registration Request sent and the
// association closed.
func TestAttachProbeBeforeRegistration(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	cfg := gnb.Config{ID: 1, Name: "orbit-gnb-1", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	errStop := errors.New("stop here")
	tests := []struct {
		name         string
		supi         string
		hookErr      error
		wantRegister bool
	}{
		{name: "hook succeeds", supi: "208930100007601", wantRegister: true},
		{name: "hook fails", supi: "208930100007602", hookErr: errStop},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203"}, mockamf.WithGUTI())
			if err != nil {
				t.Fatal(err)
			}
			front := sctpfront.Start(t, amf)
			id, err := ue.ParseIdentity(tc.supi, "208", "93", "0")
			if err != nil {
				t.Fatal(err)
			}
			var calls, regReqsAtHook int
			var acked bool
			hook := func(ctx context.Context, conn gnb.Transport) error {
				calls++
				regReqsAtHook = len(amf.RegistrationRequests())
				pdu, err := gnb.BuildRANConfigurationUpdate(gnb.WithRANNodeName("orbit-gnb-1"))
				if err != nil {
					return err
				}
				res, err := gnb.RANConfigurationUpdate(ctx, conn, pdu)
				if err != nil {
					return err
				}
				acked = res.Acknowledged
				return tc.hookErr
			}
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			res, err := m.Register(ctx, GNBSpec{Config: cfg, AMFAddr: front.Addr}, UEConfig{
				Identity: id, Sub: auth.Subscription{SUPI: tc.supi, Ki: ki, OPc: opc},
				Probe: &AttachProbe{BeforeRegistration: hook},
			})
			if calls != 1 || regReqsAtHook != 0 || !acked {
				t.Errorf("hook: %d calls, %d Registration Requests before it, update acknowledged %v; want 1, 0, true", calls, regReqsAtHook, acked)
			}
			if got := len(amf.RANConfigUpdates()); got != 1 {
				t.Errorf("the mock received %d RAN CONFIGURATION UPDATEs, want 1", got)
			}
			if !tc.wantRegister {
				if !errors.Is(err, errStop) {
					t.Fatalf("Register: %+v, %v; want the hook's error", res, err)
				}
				if got := len(amf.RegistrationRequests()); got != 0 {
					t.Errorf("%d Registration Requests sent after the hook failed", got)
				}
				deadline := time.Now().Add(3 * time.Second)
				for front.Active() != 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if n := front.Active(); n != 0 {
					t.Errorf("%d associations still open after the failed attach", n)
				}
				return
			}
			if err != nil || !res.Registered {
				t.Fatalf("Register: %+v, %v", res, err)
			}
			if got := len(amf.RegistrationRequests()); got != 1 {
				t.Errorf("%d Registration Requests, want 1", got)
			}
			if err := m.Deregister(ctx, tc.supi); err != nil {
				t.Errorf("Deregister: %v", err)
			}
		})
	}
}
