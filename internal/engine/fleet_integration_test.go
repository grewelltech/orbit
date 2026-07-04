//go:build integration

package engine_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLiveFleet registers a fleet of UEs across two gNBs — two N2
// associations, each multiplexing its UEs — against the live core, the
// Phase-2 multi-cell scale-out verification. Distinct IMSIs from the
// provisioned range (…7500–599) keep it off other tenants on the shared core.
//
//	ORBIT_TEST_KI=<hex> ORBIT_TEST_OPC=<hex> [ORBIT_FLEET_UES=20] \
//	  go test -tags=integration ./internal/engine -run TestLiveFleet -v
func TestLiveFleet(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	if kiHex == "" || opcHex == "" {
		t.Skip("set ORBIT_TEST_KI and ORBIT_TEST_OPC to run the live fleet")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	n := envInt("ORBIT_FLEET_UES", 20)
	ki, err := auth.ParseHexKey("Ki", kiHex)
	if err != nil {
		t.Fatal(err)
	}
	opc, err := auth.ParseHexKey("OPc", opcHex)
	if err != nil {
		t.Fatal(err)
	}

	mkGNB := func(id uint32, name string) engine.GNBSpec {
		return engine.GNBSpec{
			AMFAddr: amf,
			Config: gnb.Config{ID: id, Name: name, MCC: "208", MNC: "93", TAC: 1,
				Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}},
		}
	}
	gnbs := []engine.GNBSpec{mkGNB(0x42, "orbit-gnb-a"), mkGNB(0x43, "orbit-gnb-b")}

	ues := make([]engine.UEConfig, n)
	for i := 0; i < n; i++ {
		supi := fmt.Sprintf("%015d", int64(208930100007500)+int64(i))
		id, err := ue.ParseIdentity(supi, "208", "93", "0")
		if err != nil {
			t.Fatal(err)
		}
		ues[i] = engine.UEConfig{Identity: id, Sub: auth.Subscription{SUPI: supi, Ki: ki, OPc: opc}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fleet, err := engine.NewFleet(ctx, gnbs, log)
	if err != nil {
		t.Fatalf("NewFleet: %v", err)
	}
	defer fleet.Close()

	start := time.Now()
	res := fleet.Register(ctx, ues, envInt("ORBIT_FLEET_WORKERS", 8))
	dur := time.Since(start)

	t.Logf("fleet: %d registered, %d failed across %d gNBs in %s (%.0f attach/s)",
		res.Registered, res.Failed, len(gnbs), dur.Round(time.Millisecond), float64(res.Registered)/dur.Seconds())
	if res.FirstError != nil {
		t.Logf("first error: %v", res.FirstError)
	}
	if res.Registered != n {
		t.Fatalf("only %d/%d UEs registered", res.Registered, n)
	}
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
