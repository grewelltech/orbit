//go:build integration

package engine_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLiveAttach drives a full control-plane attach of one UE against the
// live core (integration-CI). It needs the test subscriber's Ki/OPc, read
// from the environment so no secret is committed:
//
//	ORBIT_AMF_N2=172.17.50.11:38412 \
//	ORBIT_TEST_SUPI=208930100007500 \
//	ORBIT_TEST_KI=<hex> ORBIT_TEST_OPC=<hex> \
//	go test -tags=integration ./internal/engine -run TestLiveAttach -v
//
// The ATB-01 defaults are used when the AMF/SUPI vars are unset; Ki/OPc must
// always be supplied.
func TestLiveAttach(t *testing.T) {
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007500")
	kiHex := os.Getenv("ORBIT_TEST_KI")
	opcHex := os.Getenv("ORBIT_TEST_OPC")
	if kiHex == "" || opcHex == "" {
		t.Skip("set ORBIT_TEST_KI and ORBIT_TEST_OPC to run the live attach")
	}
	ki, err := auth.ParseHexKey("Ki", kiHex)
	if err != nil {
		t.Fatal(err)
	}
	opc, err := auth.ParseHexKey("OPc", opcHex)
	if err != nil {
		t.Fatal(err)
	}

	gnbCfg := gnb.Config{
		ID: 0x42, Name: "orbit-gnb-1", MCC: "208", MNC: "93", TAC: 1,
		Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}},
	}
	id, err := ue.ParseIdentity(supi, gnbCfg.MCC, gnbCfg.MNC, "0")
	if err != nil {
		t.Fatal(err)
	}

	conn, err := sctp.Dial("", amf)
	if err != nil {
		t.Fatalf("SCTP dial %s: %v", amf, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// NG Setup first (per-association, once).
	ng, err := gnb.NGSetup(ctx, conn, gnbCfg)
	if err != nil {
		t.Fatalf("NG Setup: %v", err)
	}
	if !ng.Accepted {
		t.Fatalf("NG Setup rejected: %s", ng.Cause)
	}

	log := observability.NewLogger(os.Stderr, -4) // debug
	res, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
		Identity: id,
		Sub:      auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
	}, log)
	if err != nil {
		t.Fatalf("attach failed: %v", err)
	}
	if !res.Registered {
		t.Fatal("UE did not reach REGISTERED")
	}
	t.Logf("UE %s REGISTERED (AMF-UE-NGAP-ID %d)", res.SUPI, res.AMFUENGAPID)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
