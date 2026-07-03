//go:build integration

package engine_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/engine"
	"github.com/bgrewell/orbit/internal/gnb"
	"github.com/bgrewell/orbit/internal/observability"
	"github.com/bgrewell/orbit/internal/sctp"
	"github.com/bgrewell/orbit/internal/ue"
	"github.com/bgrewell/orbit/internal/ue/auth"
)

// TestLivePingN3 attaches a UE with a PDU session, then sends an ICMP echo
// through the GTP-U tunnel to the data network and waits for the reply —
// the Phase-1b user-plane proof (gnbsim's N3 smoke test).
//
// Must run where the UPF N3 access-net is reachable (the RAN node, not
// grewell01 — see the N3 topology note). Required env:
//
//	ORBIT_TEST_KI, ORBIT_TEST_OPC  subscriber creds
//	ORBIT_GNB_N3   gNB N3 bind/report IP (a RAN-node access IP, e.g. 172.17.50.13)
//	ORBIT_UPF_HINT (optional) override UPF N3 IP if the reported one is unusable
//	ORBIT_PING_DST (optional) echo target, default 8.8.8.8
func TestLivePingN3(t *testing.T) {
	kiHex, opcHex := os.Getenv("ORBIT_TEST_KI"), os.Getenv("ORBIT_TEST_OPC")
	gnbN3 := os.Getenv("ORBIT_GNB_N3")
	if kiHex == "" || opcHex == "" || gnbN3 == "" {
		t.Skip("set ORBIT_TEST_KI, ORBIT_TEST_OPC and ORBIT_GNB_N3 to run the N3 ping")
	}
	amf := envOr("ORBIT_AMF_N2", "172.17.50.11:38412")
	supi := envOr("ORBIT_TEST_SUPI", "208930100007505")
	pingDst := envOr("ORBIT_PING_DST", "8.8.8.8")

	ki, err := auth.ParseHexKey("Ki", kiHex)
	if err != nil {
		t.Fatal(err)
	}
	opc, err := auth.ParseHexKey("OPc", opcHex)
	if err != nil {
		t.Fatal(err)
	}

	gnbCfg := gnb.Config{ID: 0x42, Name: "orbit-gnb-1", MCC: "208", MNC: "93", TAC: 1,
		Slices: []gnb.SNSSAI{{SST: 1, SD: "010203"}}}
	id, err := ue.ParseIdentity(supi, gnbCfg.MCC, gnbCfg.MNC, "0")
	if err != nil {
		t.Fatal(err)
	}

	conn, err := sctp.Dial("", amf)
	if err != nil {
		t.Fatalf("SCTP dial: %v", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if ng, err := gnb.NGSetup(ctx, conn, gnbCfg); err != nil || !ng.Accepted {
		t.Fatalf("NG Setup: %v", err)
	}

	log := observability.NewLogger(os.Stderr, 0)
	sess, err := engine.Attach(ctx, conn, gnbCfg, engine.UEConfig{
		Identity:   id,
		Sub:        auth.Subscription{SUPI: supi, Ki: ki, OPc: opc},
		PDUSession: &ue.PDUSessionParams{PDUSessionID: 1, SST: 1, SD: "010203", DNN: "internet"},
		GNBN3Addr:  gnbN3, // reported to the UPF so downlink returns here
	}, log, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	res := sess.Result
	if !res.SessionActive {
		t.Fatal("no PDU session")
	}
	t.Logf("session: UE IP %s, UPF %s TEID %d, gNB DL TEID %d, QFI %d",
		res.PDUAddress, res.UPFAddress, res.UPFTEID, res.DLTEID, res.QFI)

	upfN3 := res.UPFAddress
	if h := os.Getenv("ORBIT_UPF_HINT"); h != "" {
		upfN3 = h
	}
	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: net.JoinHostPort(gnbN3, "2152"),
		UPFN3:   net.JoinHostPort(upfN3, "2152"),
		ULTEID:  res.UPFTEID,
		DLTEID:  res.DLTEID,
		QFI:     res.QFI,
	})
	if err != nil {
		t.Fatalf("tunnel: %v", err)
	}
	defer tun.Close()

	req, err := datapath.BuildICMPEchoRequest(net.ParseIP(res.PDUAddress), net.ParseIP(pingDst), 0xBEEF, 1, []byte("orbit-n3"))
	if err != nil {
		t.Fatal(err)
	}

	// Retry a few times: the first packets may be lost while the UPF's
	// downlink path settles.
	var got *datapath.EchoReply
	for i := 1; i <= 5 && got == nil; i++ {
		if err := tun.SendUplink(req); err != nil {
			t.Fatalf("send uplink: %v", err)
		}
		inner, err := tun.ReadDownlink(2 * time.Second)
		if err != nil {
			t.Logf("attempt %d: no reply yet (%v)", i, err)
			continue
		}
		if r, ok := datapath.MatchICMPEchoReply(inner, 0xBEEF, 1); ok {
			got = r
		}
	}
	if got == nil {
		t.Fatalf("no ICMP echo reply from %s through N3", pingDst)
	}
	st := tun.Stats()[res.QFI]
	t.Logf("N3 ping OK: reply from %s (uplink %d pkts/%d B, downlink %d pkts/%d B)",
		got.From, st.UplinkPackets, st.UplinkBytes, st.DownlinkPackets, st.DownlinkBytes)
}
