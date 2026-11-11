package mockamf_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/free5gc/nas/nasType"
	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/mockamf"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// The gNB holds the UE security capabilities the AMF gave it in the Initial
// Context Setup Request, and a Path Switch Request carrying them matches the
// mock's stored copy. One carrying anything else is counted, and with
// WithStoredCapabilitiesInPathSwitchAck the Acknowledge returns the stored
// copy (TS 33.501 6.7.3.1).
func TestPathSwitchUESecurityCapabilities(t *testing.T) {
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	id, err := ue.ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	nasCaps := func(ea, ia []uint8) *nasType.UESecurityCapability {
		c, err := ue.NewSecurityCapability(ea, ia)
		if err != nil {
			t.Fatal(err)
		}
		return &c
	}
	other := gnb.NewUESecurityCapabilities(0xE000, 0xE000, 0, 0)
	inAck := []mockamf.Option{mockamf.WithStoredCapabilitiesInPathSwitchAck()}

	tests := []struct {
		name string
		opts []mockamf.Option
		caps *nasType.UESecurityCapability // nil: 5G-EA0 and 128-5G-IA2
		// wantHeld is what the gNB holds after the attach: the NAS
		// capabilities without the null algorithms, algorithm 1 in the
		// first bit.
		wantHeld *ngapType.UESecurityCapabilities
		// sent overrides the held capabilities in the Path Switch Request.
		sent           *ngapType.UESecurityCapabilities
		wantMismatches int64
		wantInAck      bool
	}{
		{name: "default UE", wantHeld: gnb.NewUESecurityCapabilities(0, 0x4000, 0, 0)},
		{name: "default UE, stored copy on request", opts: inAck, wantHeld: gnb.NewUESecurityCapabilities(0, 0x4000, 0, 0)},
		{name: "EA0-2 and IA1-2", caps: nasCaps([]uint8{0, 1, 2}, []uint8{1, 2}), wantHeld: gnb.NewUESecurityCapabilities(0xC000, 0xC000, 0, 0)},
		{name: "EA0 and EA3, IA2 and IA3", opts: inAck, caps: nasCaps([]uint8{0, 3}, []uint8{2, 3}), wantHeld: gnb.NewUESecurityCapabilities(0x2000, 0x6000, 0, 0)},
		{name: "other capabilities, counted only", wantHeld: gnb.NewUESecurityCapabilities(0, 0x4000, 0, 0), sent: other, wantMismatches: 1},
		{name: "other capabilities, stored copy returned", opts: inAck, wantHeld: gnb.NewUESecurityCapabilities(0, 0x4000, 0, 0), sent: other,
			wantMismatches: 1, wantInAck: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			amf, err := mockamf.New(mockamf.Config{Ki: ki, OPc: opc, SQN: []byte{0, 0, 0, 0, 0, 0x21}, AMF: []byte{0x80, 0x00},
				MCC: "208", MNC: "93", SST: 1, SD: "010203", UPF: nopUPF{}, UPFAddress: "127.0.0.9"}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srcCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
			src := amf.Connect(ctx)
			defer func() { _ = src.Close() }() // test teardown
			if ng, err := gnb.NGSetup(ctx, src, srcCfg); err != nil || !ng.Accepted {
				t.Fatalf("NG Setup: %v", err)
			}
			tgt := amf.Connect(ctx)
			defer func() { _ = tgt.Close() }() // test teardown
			tgtCfg := srcCfg
			tgtCfg.ID, tgtCfg.Name = 2, "orbit-gnb-2"
			if ng, err := gnb.NGSetup(ctx, tgt, tgtCfg); err != nil || !ng.Accepted {
				t.Fatalf("target NG Setup: %v", err)
			}
			sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{Identity: id,
				Sub:                  auth.Subscription{SUPI: "208930100007500", Ki: ki, OPc: opc},
				SecurityCapabilities: tc.caps,
				PDUSession:           &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"}},
				slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			held := sess.UESecurityCapabilities()
			if !gnb.SameUESecurityCapabilities(held, tc.wantHeld) {
				t.Fatalf("gNB holds %+v, want %+v", held, tc.wantHeld)
			}
			sent := held
			if tc.sent != nil {
				sent = tc.sent
			}
			ps, err := gnb.BuildPathSwitchRequest(tgtCfg, sess.Result.AMFUENGAPID, 7, sent, []gnb.AdmittedSession{
				{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: "127.0.0.2", TEID: 0x99}, QFIs: []int64{1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := gnb.SendPDU(tgt, 1, ps); err != nil {
				t.Fatal(err)
			}
			ack, err := gnb.ReadPDU(ctx, tgt)
			if err != nil {
				t.Fatal(err)
			}
			if gnb.ClassifyPathSwitch(ack) != gnb.PathSwitchAcknowledged {
				t.Fatalf("path switch not acknowledged (present %d)", ack.Present)
			}
			if n := amf.PathSwitchCapabilityMismatches(); n != tc.wantMismatches {
				t.Errorf("mock counted %d capability mismatches, want %d", n, tc.wantMismatches)
			}
			got := gnb.PathSwitchAcknowledgeUESecurityCapabilities(ack)
			switch {
			case !tc.wantInAck && got != nil:
				t.Errorf("Acknowledge carries UE security capabilities %+v, want none", got)
			case tc.wantInAck && !gnb.SameUESecurityCapabilities(got, tc.wantHeld):
				t.Errorf("Acknowledge carries %+v, want the stored %+v", got, tc.wantHeld)
			}
			if n := amf.EncodeErrors(); n != 0 {
				t.Errorf("mock dropped %d PDUs that did not encode", n)
			}
		})
	}
}
