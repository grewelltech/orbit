//go:build integration

package engine_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	orbitngap "github.com/bgrewell/orbit/internal/ngap"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLiveHandoverPrep is discovery spike D-1 / milestone gate M-1: does the
// live SD-Core drive N2 handover? It attaches a UE on a source gNB, sends
// HandoverRequired targeting a second gNB, and watches both associations for
// the AMF's reaction — HandoverRequest at the target (supported), Handover
// Command at the source (supported), or HandoverPreparationFailure / silence
// (unsupported). Observe-only; it does not complete the handover.
func TestLiveHandoverPrep(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	if kiHex == "" || opcHex == "" {
		t.Skip("set ORBIT_TEST_KI and ORBIT_TEST_OPC")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007500")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)

	srcCfg := gnb.Config{ID: 0x42, Name: "orbit-gnb-src", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	tgtCfg := gnb.Config{ID: 0x43, Name: "orbit-gnb-tgt", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Distinct source IPs per gNB avoid the AMF keying both associations to
	// one address (the "Ran addr is nil" symptom / PacketRusher #138).
	src, err := sctp.Dial(envOr("ORBIT_SRC_BIND", ""), amf)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tgt, err := sctp.Dial(envOr("ORBIT_TGT_BIND", ""), amf)
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

	// Attach a UE with a PDU session on the source gNB.
	id, _ := ue.ParseIdentity(supi, "208", "93", "0")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess, err := engine.Attach(ctx, src, srcCfg, engine.UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
	}, log, nil)
	if err != nil || !sess.Result.SessionActive {
		t.Fatalf("attach on source gNB: %v", err)
	}
	t.Logf("UE on source gNB: AMF-UE-NGAP-ID %d, session IP %s", sess.Result.AMFUENGAPID, sess.Result.PDUAddress)

	// Watch both associations (blocking reads); they stop when the conns are
	// closed after the observation window.
	outcomes := make(chan string, 16)
	var wg sync.WaitGroup
	watch := func(name string, c *sctp.Conn) {
		defer wg.Done()
		buf := make([]byte, 65536)
		for {
			payload, _, _, err := c.ReadMsg(buf)
			if err != nil {
				return
			}
			pdu, err := orbitngap.Decode(payload)
			if err != nil {
				continue
			}
			switch gnb.ClassifyHandover(pdu) {
			case gnb.HandoverRequestAtTarget:
				outcomes <- name + ": HandoverRequest (AMF forwarded to target — SUPPORTED)"
			case gnb.HandoverCommandReceived:
				outcomes <- name + ": HandoverCommand (AMF prepared handover — SUPPORTED)"
			case gnb.HandoverPreparationFailed:
				outcomes <- name + ": HandoverPreparationFailure (AMF rejected preparation)"
			default:
				outcomes <- name + ": other NGAP PDU"
			}
		}
	}
	wg.Add(2)
	go watch("source", src)
	go watch("target", tgt)

	// Send HandoverRequired from source → target.
	ho, err := gnb.BuildHandoverRequired(gnb.HandoverParams{
		Source: srcCfg, Target: tgtCfg,
		AMFUENGAPID: sess.Result.AMFUENGAPID, RANUENGAPID: sess.Result.RANUENGAPID,
		PDUSessionIDs: []int64{1},
	})
	if err != nil {
		t.Fatalf("build HandoverRequired: %v", err)
	}
	if err := gnb.SendPDU(src, 1, ho); err != nil {
		t.Fatalf("send HandoverRequired: %v", err)
	}
	t.Log("sent HandoverRequired; watching for the AMF's reaction (6s)...")

	time.Sleep(6 * time.Second)
	src.Close()
	tgt.Close()
	wg.Wait()
	close(outcomes)

	var seen []string
	for o := range outcomes {
		t.Log("  " + o)
		seen = append(seen, o)
	}
	if len(seen) == 0 {
		t.Log("D-1 result: SILENCE — the AMF sent no NGAP reaction to HandoverRequired")
	}
	t.Logf("D-1 result: %d NGAP reaction(s) observed", len(seen))
}
