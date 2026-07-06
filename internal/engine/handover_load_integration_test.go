//go:build integration

package engine_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/load"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLiveHandoverUnderLoad hands a mobile UE through several N2 handovers
// while a background attach storm loads the core, and reports whether handover
// latency/success hold up. Run from the RAN node with distinct routed source
// IPs (172.17.50.12/.13). Env: ORBIT_TEST_KI/OPC.
func TestLiveHandoverUnderLoad(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	if kiHex == "" || opcHex == "" {
		t.Skip("set ORBIT_TEST_KI and ORBIT_TEST_OPC")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	ki, _ := auth.ParseHexKey("Ki", kiHex)
	opc, _ := auth.ParseHexKey("OPc", opcHex)
	slice := []gnb.SNSSAI{{SST: 1, SD: "010203"}}

	cfg := func(id uint32, name string) gnb.Config {
		return gnb.Config{ID: id, Name: name, MCC: "208", MNC: "93", TAC: 1, Slices: slice}
	}
	ep := func(id uint32, bind, n3 string) engine.GNBEndpoint {
		return engine.GNBEndpoint{Config: cfg(id, fmt.Sprintf("orbit-ho-%d", id)), AMFAddr: amf, BindAddr: bind, N3Addr: n3}
	}

	spec := engine.HandoverLoadSpec{
		MobileSUPI: "208930100007540", Ki: ki, OPc: opc, MCC: "208", MNC: "93", AMFAddr: amf,
		SourceGNB: cfg(240, "orbit-ho-src"), SourceN3: "172.17.50.12",
		Slice: ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
		Hops: []engine.GNBEndpoint{
			ep(241, "172.17.50.13:0", "172.17.50.13"),
			ep(242, "172.17.50.12:0", "172.17.50.12"),
			ep(243, "172.17.50.13:0", "172.17.50.13"),
			ep(244, "172.17.50.12:0", "172.17.50.12"),
		},
		Background: &engine.LoadSpec{
			GNBs:     []engine.GNBSpec{{AMFAddr: amf, Config: cfg(250, "orbit-ho-bg"), BindAddr: "172.17.50.14:0"}},
			BaseIMSI: "208930100007560", Count: 20, MCC: "208", MNC: "93",
			Ki: ki, OPc: opc, Concurrency: 8, Rate: load.Constant{RPS: 10},
		},
		Profile: "sdcore",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep, err := engine.RunHandoverUnderLoad(ctx, log, spec)
	if err != nil && rep == nil {
		t.Fatalf("handover-under-load setup: %v", err)
	}
	// Diagnostic: report what the core did rather than hard-failing — the
	// interesting result is whether handover latency/success holds under load.
	if rep.Background != nil {
		t.Logf("concurrent attach storm: %d/%d succeeded at %.1f/s",
			rep.Background.Succeeded, rep.Background.Attempted, rep.Background.AchievedRate)
	}
	t.Logf("handovers under load: %d ok / %d failed", rep.Handovers, rep.HandoverFailures)
	if rep.Handovers > 0 {
		t.Logf("  handover latency P50 %v P99 %v max %v",
			rep.P50.Round(time.Millisecond), rep.P99.Round(time.Millisecond), rep.Max.Round(time.Millisecond))
	}
	if rep.FirstError != nil {
		t.Logf("  first handover error: %v", rep.FirstError)
	}
}
