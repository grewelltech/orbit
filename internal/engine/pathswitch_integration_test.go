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

// TestLivePathSwitch is discovery spike D-2 / the Xn feasibility gate: does the
// live SD-Core complete an NGAP PathSwitchRequest? It attaches a UE with a PDU
// session on a source gNB, then — modelling the target gNB after an in-process
// Xn handover — sends a PathSwitchRequest from a second gNB naming the UE by
// its source AMF-UE-NGAP-ID, and observes whether the AMF replies
// PathSwitchRequestAcknowledge (Xn supported) or Failure/silence.
//
// Run from the RAN node with distinct routed source IPs and FRESH gNB IDs:
//
//	ORBIT_TEST_KI/OPC, ORBIT_SRC_BIND=172.17.50.12:0 ORBIT_TGT_BIND=172.17.50.13:0,
//	ORBIT_SRC_GNB / ORBIT_TGT_GNB (fresh), ORBIT_TGT_N3=172.17.50.13
func TestLivePathSwitch(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	srcBind, tgtBind := os.Getenv("ORBIT_SRC_BIND"), os.Getenv("ORBIT_TGT_BIND")
	if kiHex == "" || opcHex == "" || srcBind == "" || tgtBind == "" {
		t.Skip("set ORBIT_TEST_KI/OPC and ORBIT_SRC_BIND/ORBIT_TGT_BIND")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007542")
	tgtN3 := envOr("ORBIT_TGT_N3", "172.17.50.13")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)
	slice := []gnb.SNSSAI{{SST: 1, SD: "010203"}}

	srcCfg := gnb.Config{ID: uint32(envInt("ORBIT_SRC_GNB", 0xC0)), Name: "orbit-xn-src", MCC: "208", MNC: "93", TAC: 1, Slices: slice}
	tgtCfg := gnb.Config{ID: uint32(envInt("ORBIT_TGT_GNB", 0xC1)), Name: "orbit-xn-tgt", MCC: "208", MNC: "93", TAC: 1, Slices: slice}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	id, _ := ue.ParseIdentity(supi, "208", "93", "0")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
	}, log, nil)
	if err != nil || !sess.Result.SessionActive {
		t.Fatalf("attach on source gNB: %v", err)
	}
	t.Logf("attached on source: AMF-UE-NGAP-ID %d, IP %s", sess.Result.AMFUENGAPID, sess.Result.PDUAddress)

	// Target gNB sends PathSwitchRequest for the UE (Xn completion).
	ps, err := gnb.BuildPathSwitchRequest(tgtCfg, sess.Result.AMFUENGAPID, 1, []gnb.AdmittedSession{
		{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: tgtN3, TEID: 0x300}, QFIs: []int64{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gnb.SendPDU(tgt, 1, ps); err != nil {
		t.Fatalf("send PathSwitchRequest: %v", err)
	}
	t.Log("→ PathSwitchRequest (from target gNB); waiting for the AMF's response...")

	rctx, rcancel := context.WithTimeout(ctx, 8*time.Second)
	defer rcancel()
	pdu, err := gnb.ReadPDU(rctx, tgt)
	if err != nil {
		t.Fatalf("D-2 result: SILENCE / no response to PathSwitchRequest (%v)", err)
	}
	switch gnb.ClassifyPathSwitch(pdu) {
	case gnb.PathSwitchAcknowledged:
		amfID, _ := gnb.ParsePathSwitchAcknowledge(pdu)
		t.Logf("D-2 result: PathSwitchRequestAcknowledge — SD-Core completes Xn path switch (new AMF-UE-NGAP-ID %d)", amfID)
	case gnb.PathSwitchFailed:
		t.Log("D-2 result: PathSwitchRequestFailure — AMF rejected the path switch (check cause)")
	default:
		t.Log("D-2 result: other NGAP PDU (not a PathSwitch response)")
	}
}
