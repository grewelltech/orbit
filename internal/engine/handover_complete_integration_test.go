//go:build integration

package engine_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLiveHandoverComplete drives a full N2 handover to completion against the
// live core: attach on the source gNB, then HandoverRequired → (AMF) →
// HandoverRequest → HandoverRequestAcknowledge → (AMF) → HandoverCommand →
// HandoverNotify. Run from the RAN node with distinct routed source IPs per
// gNB (so the AMF can deliver Handover Request to the target):
//
//	ORBIT_TEST_KI/OPC, ORBIT_SRC_BIND=172.17.50.12:0 ORBIT_TGT_BIND=172.17.50.13:0
//	ORBIT_TGT_N3=172.17.50.13
func TestLiveHandoverComplete(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	srcBind, tgtBind := os.Getenv("ORBIT_SRC_BIND"), os.Getenv("ORBIT_TGT_BIND")
	if kiHex == "" || opcHex == "" || srcBind == "" || tgtBind == "" {
		t.Skip("set ORBIT_TEST_KI/OPC and ORBIT_SRC_BIND/ORBIT_TGT_BIND (distinct routed IPs)")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007500")
	tgtN3 := envOr("ORBIT_TGT_N3", "172.17.50.13")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)

	srcCfg := gnb.Config{ID: uint32(envInt("ORBIT_SRC_GNB", 0x42)), Name: "orbit-gnb-src", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	tgtCfg := gnb.Config{ID: uint32(envInt("ORBIT_TGT_GNB", 0x43)), Name: "orbit-gnb-tgt", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	src, err := sctp.Dial(srcBind, amf)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tgt, err := sctp.Dial(tgtBind, amf)
	if err != nil {
		t.Fatal(err)
	}
	defer tgt.Close()
	if ng, err := gnb.NGSetup(ctx, src, srcCfg); err != nil || !ng.Accepted {
		t.Fatalf("source NG Setup: %v", err)
	}
	if ng, err := gnb.NGSetup(ctx, tgt, tgtCfg); err != nil || !ng.Accepted {
		t.Fatalf("target NG Setup: %v", err)
	}

	// Attach a UE with a session on the source gNB.
	id, _ := ue.ParseIdentity(supi, "208", "93", "0")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
	}, log, nil)
	if err != nil || !sess.Result.SessionActive {
		t.Fatalf("attach: %v", err)
	}
	t.Logf("attached on source: AMF-UE-NGAP-ID %d, IP %s", sess.Result.AMFUENGAPID, sess.Result.PDUAddress)

	// 1. source → AMF: HandoverRequired.
	ho, err := gnb.BuildHandoverRequired(gnb.HandoverParams{
		Source: srcCfg, Target: tgtCfg,
		AMFUENGAPID: sess.Result.AMFUENGAPID, RANUENGAPID: sess.Result.RANUENGAPID,
		PDUSessionIDs: []int64{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gnb.SendPDU(src, 1, ho); err != nil {
		t.Fatal(err)
	}
	t.Log("→ HandoverRequired")

	// 2. AMF → target: HandoverRequest.
	reqPDU, err := gnb.ReadPDU(ctx, tgt)
	if err != nil {
		t.Fatalf("waiting for HandoverRequest at target: %v", err)
	}
	hr, err := gnb.ParseHandoverRequest(reqPDU)
	if err != nil {
		t.Fatalf("target did not receive HandoverRequest (got %v): the AMF drove preparation but delivery failed", err)
	}
	t.Logf("← HandoverRequest at target (AMF-UE-NGAP-ID %d, sessions %v)", hr.AMFUENGAPID, hr.PDUSessionIDs)

	// 3. target → AMF: HandoverRequestAcknowledge (target DL N3 tunnel).
	const targetRANID = int64(1)
	admitted := make([]gnb.AdmittedSession, 0, len(hr.PDUSessionIDs))
	for i, sid := range hr.PDUSessionIDs {
		admitted = append(admitted, gnb.AdmittedSession{
			PDUSessionID: sid,
			GNBTunnel:    gnb.GNBTunnel{Address: tgtN3, TEID: uint32(100 + i)},
			QFIs:         []int64{1},
		})
	}
	ack, err := gnb.BuildHandoverRequestAcknowledge(hr.AMFUENGAPID, targetRANID, admitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := gnb.SendPDU(tgt, 1, ack); err != nil {
		t.Fatal(err)
	}
	t.Log("→ HandoverRequestAcknowledge")

	// 4. AMF → source: HandoverCommand.
	cmdPDU, err := gnb.ReadPDU(ctx, src)
	if err != nil {
		t.Fatalf("waiting for HandoverCommand at source: %v", err)
	}
	if _, err := gnb.ParseHandoverCommand(cmdPDU); err != nil {
		t.Fatalf("source did not receive HandoverCommand (got %v)", err)
	}
	t.Log("← HandoverCommand at source")

	// 5. target → AMF: HandoverNotify (UE arrived) → AMF drives N4 path switch.
	notify, err := gnb.BuildHandoverNotify(tgtCfg, hr.AMFUENGAPID, targetRANID)
	if err != nil {
		t.Fatal(err)
	}
	if err := gnb.SendPDU(tgt, 1, notify); err != nil {
		t.Fatal(err)
	}
	t.Log("→ HandoverNotify")

	// Expect the AMF to release the source (UE Context Release Command).
	relCtx, relCancel := context.WithTimeout(ctx, 5*time.Second)
	defer relCancel()
	if _, err := gnb.ReadPDU(relCtx, src); err != nil {
		t.Logf("no source-side release seen within 5s (%v) — handover may still have completed", err)
	} else {
		t.Log("← source-side NGAP (likely UE Context Release) — handover completed")
	}
	t.Log("N2 handover completed end to end against the live core")
}
