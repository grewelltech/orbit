//go:build integration

package engine_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/coreprofile"
	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLiveHandoverDataContinuity is discovery spike D-4: does a user-plane
// flow survive an N2 handover? It attaches a UE with a session and a working
// N3 data path on the source gNB, hands the UE over to the target gNB, then
// pings through the TARGET's N3 tunnel — which only works if the UPF honoured
// the downlink path switch. The UE keeps its IP and the UPF's uplink TEID; the
// target gets the downlink.
//
//	ORBIT_TEST_KI/OPC, ORBIT_SRC_BIND=172.17.50.12:0 ORBIT_TGT_BIND=172.17.50.13:0,
//	ORBIT_SRC_N3=172.17.50.12 ORBIT_TGT_N3=172.17.50.13, fresh ORBIT_SRC_GNB/TGT_GNB.
func TestLiveHandoverDataContinuity(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	srcBind, tgtBind := os.Getenv("ORBIT_SRC_BIND"), os.Getenv("ORBIT_TGT_BIND")
	if kiHex == "" || opcHex == "" || srcBind == "" || tgtBind == "" {
		t.Skip("set ORBIT_TEST_KI/OPC and ORBIT_SRC_BIND/ORBIT_TGT_BIND")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007504")
	srcN3, tgtN3 := envOr("ORBIT_SRC_N3", "172.17.50.12"), envOr("ORBIT_TGT_N3", "172.17.50.13")
	pingDst := envOr("ORBIT_PING_DST", "8.8.8.8")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)

	srcCfg := gnb.Config{ID: uint32(envInt("ORBIT_SRC_GNB", 0x42)), Name: "orbit-gnb-src", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	tgtCfg := gnb.Config{ID: uint32(envInt("ORBIT_TGT_GNB", 0x43)), Name: "orbit-gnb-tgt", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

	// Attach with a session + N3 data path on the source gNB.
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
	t.Logf("attached on source: IP %s, UPF %s UL-TEID %d, source DL-TEID %d", r.PDUAddress, r.UPFAddress, r.UPFTEID, r.DLTEID)

	// Baseline: the flow works through the source before handover.
	if ok := ping(t, srcN3, r.UPFAddress, r.UPFTEID, r.DLTEID, r.QFI, r.PDUAddress, pingDst); !ok {
		t.Fatal("baseline ping through the source gNB failed")
	}
	t.Log("baseline: ping OK through the SOURCE gNB")

	// Hand the UE over to the target gNB.
	const targetRANID, targetDLTEID = int64(1), uint32(200)
	if err := runHandover(ctx, t, src, tgt, srcCfg, tgtCfg, r.AMFUENGAPID, r.RANUENGAPID, tgtN3, targetRANID, targetDLTEID); err != nil {
		t.Fatalf("handover: %v", err)
	}
	t.Log("handover completed; the UPF should now send downlink to the target")

	// D-4: does the flow survive on the TARGET after the path switch? The UE
	// keeps its IP and the UPF's uplink TEID; the downlink should follow to
	// the target. Reported (not hard-failed): the answer is a property of the
	// core's N4 path-switch behaviour, which this diagnoses.
	time.Sleep(1 * time.Second) // let the N4 path switch settle
	if ping(t, tgtN3, r.UPFAddress, r.UPFTEID, targetDLTEID, r.QFI, r.PDUAddress, pingDst) {
		t.Log("D-4: data continuity CONFIRMED — ping OK through the TARGET after handover")
	} else {
		t.Log("D-4: NO downlink continuity through the target — the core did not switch the DL path " +
			"(uplink from the target still reaches the UPF; check the SMF DL FAR OuterHeaderCreation)")
	}
}

// ping runs an ICMP echo through a GTP-U tunnel and returns whether a reply
// arrived (retrying a few times while the path settles).
func ping(t *testing.T, localN3, upfN3 string, ulTEID, dlTEID uint32, qfi uint8, ueIP, dst string) bool {
	t.Helper()
	if qfi == 0 {
		qfi = 1
	}
	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: net.JoinHostPort(localN3, "2152"), UPFN3: net.JoinHostPort(upfN3, "2152"),
		ULTEID: ulTEID, DLTEID: dlTEID, QFI: qfi,
	})
	if err != nil {
		t.Logf("tunnel: %v", err)
		return false
	}
	defer tun.Close()
	req, err := datapath.BuildICMPEchoRequest(net.ParseIP(ueIP), net.ParseIP(dst), 0xD400, 1, []byte("orbit-ho"))
	if err != nil {
		t.Log(err)
		return false
	}
	for i := 0; i < 5; i++ {
		if err := tun.SendUplink(req); err != nil {
			return false
		}
		inner, err := tun.ReadDownlink(2 * time.Second)
		if err != nil {
			continue
		}
		if _, ok := datapath.MatchICMPEchoReply(inner, 0xD400, 1); ok {
			return true
		}
	}
	return false
}

// runHandover drives the N2 handover source→AMF→target→AMF→source→target.
func runHandover(ctx context.Context, t *testing.T, src, tgt *sctp.Conn, srcCfg, tgtCfg gnb.Config, amfID, srcRANID int64, tgtN3 string, targetRANID int64, targetDLTEID uint32) error {
	t.Helper()
	ho, err := gnb.BuildHandoverRequired(gnb.HandoverParams{
		Source: srcCfg, Target: tgtCfg, AMFUENGAPID: amfID, RANUENGAPID: srcRANID, PDUSessionIDs: []int64{1},
	})
	if err != nil {
		return err
	}
	if err := gnb.SendPDU(src, 1, ho); err != nil {
		return err
	}
	reqPDU, err := gnb.ReadPDU(ctx, tgt)
	if err != nil {
		return err
	}
	hr, err := gnb.ParseHandoverRequest(reqPDU)
	if err != nil {
		return err
	}
	admitted := make([]gnb.AdmittedSession, 0, len(hr.PDUSessionIDs))
	for _, sid := range hr.PDUSessionIDs {
		admitted = append(admitted, gnb.AdmittedSession{PDUSessionID: sid, GNBTunnel: gnb.GNBTunnel{Address: tgtN3, TEID: targetDLTEID}, QFIs: []int64{1}})
	}
	profile, _ := coreprofile.Get(os.Getenv("ORBIT_CORE_PROFILE"))
	ack, err := gnb.BuildHandoverRequestAcknowledge(hr.AMFUENGAPID, targetRANID, admitted, profile.Quirks)
	if err != nil {
		return err
	}
	if err := gnb.SendPDU(tgt, 1, ack); err != nil {
		return err
	}
	if _, err := gnb.ReadPDU(ctx, src); err != nil { // HandoverCommand
		return err
	}
	notify, err := gnb.BuildHandoverNotify(tgtCfg, hr.AMFUENGAPID, targetRANID)
	if err != nil {
		return err
	}
	return gnb.SendPDU(tgt, 1, notify)
}
