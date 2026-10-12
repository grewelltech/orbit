package mockamf_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/free5gc/ngap/ngapType"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// pathSwitch sends one PathSwitchRequest for session 1 naming srcAMFID on
// conn and returns the mock's answer.
func pathSwitch(t *testing.T, ctx context.Context, conn gnb.Transport, srcAMFID int64) *ngapType.NGAPPDU {
	t.Helper()
	cfg := gnb.Config{ID: 2, Name: "orbit-gnb-2", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	ps, err := gnb.BuildPathSwitchRequest(cfg, srcAMFID, 7, []gnb.AdmittedSession{
		{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: "127.0.0.2", TEID: 0x99}, QFIs: []int64{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gnb.SendPDU(conn, 1, ps); err != nil {
		t.Fatal(err)
	}
	reply, err := gnb.ReadPDU(ctx, conn)
	if err != nil {
		t.Fatalf("no answer to the PathSwitchRequest: %v", err)
	}
	return reply
}

// failedSessions returns the session IDs in a PathSwitchRequestFailure's
// released list.
func failedSessions(pdu *ngapType.NGAPPDU) []int64 {
	var out []int64
	for _, ie := range pdu.UnsuccessfulOutcome.Value.PathSwitchRequestFailure.ProtocolIEs.List {
		if ie.Value.PDUSessionResourceReleasedListPSFail != nil {
			for _, it := range ie.Value.PDUSessionResourceReleasedListPSFail.List {
				out = append(out, it.PDUSessionID.Value)
			}
		}
	}
	return out
}

// Without a UPF the mock knows no PDU session, so a PathSwitchRequest, for an
// unknown UE or for a registered one, is answered with a
// PathSwitchRequestFailure listing the requested session, and the target
// association stays usable: a second request on it is answered too.
func TestPathSwitchWithoutUPFFailsAndKeepsTheAssociation(t *testing.T) {
	amf := testAMF(t) // no UPF
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A registered UE on the source association.
	src := amf.Connect(ctx)
	defer func() { _ = src.Close() }()
	srcCfg := gnb.Config{ID: 1, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	if ng, err := gnb.NGSetup(ctx, src, srcCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v", err)
	}
	ki, _ := auth.ParseHexKey("Ki", "5122250214c33e723a5dd523fc145fc0")
	opc, _ := auth.ParseHexKey("OPc", "981d464c7c52eb6e5036234984ad0bcf")
	id, err := ue.ParseIdentity("208930100007500", "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{Identity: id,
		Sub: auth.Subscription{SUPI: "208930100007500", Ki: ki, OPc: opc}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	tgt := amf.Connect(ctx)
	defer func() { _ = tgt.Close() }()
	for _, tc := range []struct {
		name     string
		srcAMFID int64
	}{
		{name: "registered UE, no session", srcAMFID: sess.Result.AMFUENGAPID},
		{name: "unknown UE", srcAMFID: 9999},
		{name: "second request on the same association", srcAMFID: sess.Result.AMFUENGAPID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := pathSwitch(t, ctx, tgt, tc.srcAMFID)
			if gnb.ClassifyPathSwitch(reply) != gnb.PathSwitchFailed {
				t.Fatalf("answer present=%d, want a PathSwitchRequestFailure", reply.Present)
			}
			if got := failedSessions(reply); len(got) != 1 || got[0] != 1 {
				t.Errorf("released list = %v, want [1]", got)
			}
		})
	}
	if n := amf.EncodeErrors(); n != 0 {
		t.Errorf("mock dropped %d PDUs that did not encode", n)
	}
}
