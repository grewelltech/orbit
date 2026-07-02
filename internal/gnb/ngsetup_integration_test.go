//go:build integration

package gnb_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/sctp"
)

// TestLiveNGSetup is the Phase-0 exit criterion: a real NG Setup exchange
// with the live AMF. Requires the lab core (integration-CI, not unit-CI):
//
//	go test -tags=integration ./internal/gnb -run TestLiveNGSetup -v
//
// ORBIT_AMF_N2 overrides the AMF address (default: ATB-01).
// The gNB ID is deliberately distinct from the-earlier-project's gNBs — the ATB-01
// core is shared, and NG Setup registrations are per-gNB-ID at the AMF.
func TestLiveNGSetup(t *testing.T) {
	amf := os.Getenv("ORBIT_AMF_N2")
	if amf == "" {
		amf = "172.17.50.11:38412"
	}

	conn, err := sctp.Dial("", amf)
	if err != nil {
		t.Fatalf("SCTP association to %s failed: %v", amf, err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := gnb.NGSetup(ctx, conn, gnb.Config{
		ID:     0x42,
		Name:   "orbit-gnb-1",
		MCC:    "208",
		MNC:    "93",
		TAC:    1,
		Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}},
	})
	if err != nil {
		t.Fatalf("NG Setup exchange failed: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("AMF rejected NG Setup: cause %s", res.Cause)
	}
	t.Logf("NG Setup accepted; AMF name %q, reply PPID %d", res.AMFName, res.ReplyPPID)
	if res.ReplyPPID == sctp.PPIDNGAPSwapped {
		t.Log("note: AMF sent the byte-swapped PPID 0x3c000000 (known SD-Core quirk, pcap-verified 2026-07-02)")
	}
}
