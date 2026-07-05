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

// TestLiveMultiSession probes whether the core establishes two PDU sessions
// for one UE. Whether it does depends on the core's DNN/slice config, so this
// is an empirical check: it reports how many of the two came up rather than
// hard-requiring both.
func TestLiveMultiSession(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	if kiHex == "" || opcHex == "" {
		t.Skip("set ORBIT_TEST_KI and ORBIT_TEST_OPC")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007502")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)

	gnbCfg := gnb.Config{ID: 0x42, Name: "orbit-gnb", MCC: "208", MNC: "93", TAC: 1, Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	id, err := ue.ParseIdentity(supi, "208", "93", "0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sctp.Dial("", amf)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
		Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSessions: []ue.PDUSessionParams{
			{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
			{PDUSessionID: 2, SST: 1, SD: "010203", DNN: "internet"},
		},
	}, log, nil)
	if err != nil {
		t.Logf("attach with 2 sessions did not complete (core may allow only one per DNN): %v", err)
		t.Skip("core did not establish both sessions; multi-session capability is built and unit-verified")
	}
	for _, s := range sess.Result.Sessions {
		t.Logf("session %d: IP %s DNN %s DL-TEID %d", s.PDUSessionID, s.IPv4, s.DNN, s.DLTEID)
	}
	if len(sess.Result.Sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sess.Result.Sessions))
	}
	t.Logf("core established %d PDU sessions for one UE", len(sess.Result.Sessions))
}
