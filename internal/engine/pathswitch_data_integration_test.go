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

// TestLivePathSwitchDataContinuity is the Xn data-plane spike: does a flow
// survive an Xn path switch? It attaches a UE with a working N3 path on the
// source gNB, confirms a baseline ping, sends a PathSwitchRequest from the
// target gNB, then pings through the TARGET's tunnel — which only works if the
// UPF honoured the downlink path switch. The N2 equivalent (D-4) was blocked by
// SD-Core Finding 2; the PathSwitch transfer decodes cleanly (D-2), so this may
// be the first data continuity across handover on SD-Core.
//
//	ORBIT_TEST_KI/OPC, ORBIT_SRC_BIND=172.17.50.12:0 ORBIT_TGT_BIND=172.17.50.13:0,
//	ORBIT_SRC_N3=172.17.50.12 ORBIT_TGT_N3=172.17.50.13, fresh ORBIT_SRC_GNB/TGT_GNB.
func TestLivePathSwitchDataContinuity(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	srcBind, tgtBind := os.Getenv("ORBIT_SRC_BIND"), os.Getenv("ORBIT_TGT_BIND")
	if kiHex == "" || opcHex == "" || srcBind == "" || tgtBind == "" {
		t.Skip("set ORBIT_TEST_KI/OPC and ORBIT_SRC_BIND/ORBIT_TGT_BIND")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007543")
	srcN3, tgtN3 := envOr("ORBIT_SRC_N3", "172.17.50.12"), envOr("ORBIT_TGT_N3", "172.17.50.13")
	pingDst := envOr("ORBIT_PING_DST", "8.8.8.8")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)
	slice := []gnb.SNSSAI{{SST: 1, SD: "010203"}}

	srcCfg := gnb.Config{ID: uint32(envInt("ORBIT_SRC_GNB", 0xC0)), Name: "orbit-xn-src", MCC: "208", MNC: "93", TAC: 1, Slices: slice}
	tgtCfg := gnb.Config{ID: uint32(envInt("ORBIT_TGT_GNB", 0xC1)), Name: "orbit-xn-tgt", MCC: "208", MNC: "93", TAC: 1, Slices: slice}

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

	id, _ := ue.ParseIdentity(supi, "208", "93", "0")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
		GNBN3Addr:  srcN3,
	}, log, nil)
	if err != nil || !sess.Result.SessionActive {
		t.Fatalf("attach: %v", err)
	}
	r := sess.Result
	t.Logf("attached: IP %s, UPF %s UL-TEID %d", r.PDUAddress, r.UPFAddress, r.UPFTEID)

	if !ping(t, srcN3, r.UPFAddress, r.UPFTEID, r.DLTEID, r.QFI, r.PDUAddress, pingDst) {
		t.Fatal("baseline ping through the source gNB failed")
	}
	t.Log("baseline: ping OK through the SOURCE gNB")

	// Target gNB requests the path switch to its own DL tunnel.
	const targetDLTEID = uint32(0x300)
	ps, err := gnb.BuildPathSwitchRequest(tgtCfg, r.AMFUENGAPID, 1, []gnb.AdmittedSession{
		{PDUSessionID: 1, GNBTunnel: gnb.GNBTunnel{Address: tgtN3, TEID: targetDLTEID}, QFIs: []int64{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gnb.SendPDU(tgt, 1, ps); err != nil {
		t.Fatal(err)
	}
	pdu, err := gnb.ReadPDU(ctx, tgt)
	if err != nil {
		t.Fatalf("await PathSwitch response: %v", err)
	}
	if gnb.ClassifyPathSwitch(pdu) != gnb.PathSwitchAcknowledged {
		t.Fatalf("path switch not acknowledged: %v", gnb.ClassifyPathSwitch(pdu))
	}
	t.Log("PathSwitchRequestAcknowledge received")

	time.Sleep(1 * time.Second) // let the N4 path switch settle
	if ping(t, tgtN3, r.UPFAddress, r.UPFTEID, targetDLTEID, r.QFI, r.PDUAddress, pingDst) {
		t.Log("XN DATA CONTINUITY CONFIRMED — ping OK through the TARGET after the Xn path switch")
	} else {
		t.Log("no downlink continuity through the target — the UPF did not switch the DL path")
	}
}
