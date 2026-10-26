package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	f5nas "github.com/free5gc/nas"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/nas"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// With OnNAS set, a Registration Accept that does not decode is skipped,
// and the attach never registers. DeregisterUndecodableAccept instead ends
// the attach there with an *UndecodableAcceptError after a protected
// switch-off Deregistration Request, which the core receives.
func TestDeregisterUndecodableAccept(t *testing.T) {
	tests := []struct {
		name       string
		deregister bool
		nea2       bool // the AMF selects NEA2, so the Accept's header type is unreadable
		wantDereg  int
	}{
		{name: "deregistered", deregister: true, wantDereg: 1},
		{name: "deregistered under NEA2", deregister: true, nea2: true, wantDereg: 1},
		{name: "skipped without the option", deregister: false, wantDereg: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
			opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
			amf, err := mockamf.New(mockamf.Config{
				Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203",
			}, mockamf.WithUndecodableAccept(), mockamf.WithCipheringOrder(nas.NEA2, nas.NEA0))
			if err != nil {
				t.Fatal(err)
			}
			mctx, mcancel := context.WithCancel(context.Background())
			defer mcancel()
			conn := amf.Connect(mctx)
			defer func() { _ = conn.Close() }()
			cfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			if ng, err := gnb.NGSetup(mctx, conn, cfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			rec := &nasEvents{}
			ueCfg := bindTestUE(t, "208930100007500", ki, opc)
			ueCfg.Probe = &AttachProbe{OnNAS: rec.add, DeregisterUndecodableAccept: tc.deregister}
			if tc.nea2 {
				caps := mustCaps(t, []uint8{nas.NEA0, nas.NEA2}, []uint8{nas.NIA2})
				ueCfg.SecurityCapabilities = &caps
			}
			ctx, cancel := context.WithTimeout(mctx, 500*time.Millisecond)
			defer cancel()
			_, err = Attach(ctx, conn, cfg, ueCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			var ue *UndecodableAcceptError
			if tc.deregister {
				if !errors.As(err, &ue) || ue.Err == nil || ue.Deregistration != nil {
					t.Fatalf("Attach err = %v, want an UndecodableAcceptError with the deregistration sent", err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Attach err = %v, want the deadline (the Accept is skipped)", err)
			}
			var sawBadAccept bool
			for _, ev := range rec.list() {
				// Under NEA2 the header type is unreadable (0).
				if (ev.Type == f5nas.MsgTypeRegistrationAccept || tc.nea2 && ev.Type == 0) && ev.Err != nil {
					sawBadAccept = true
				}
			}
			if !sawBadAccept {
				t.Error("OnNAS never saw the undecodable Accept")
			}
			deadline := time.Now().Add(2 * time.Second)
			for amf.Deregistrations() < tc.wantDereg && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := amf.Deregistrations(); got != tc.wantDereg {
				t.Errorf("the core received %d Deregistration Requests, want %d", got, tc.wantDereg)
			}
		})
	}
}
