package loomgtp

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
)

// countingUplink stands in for the GTP-U tunnel: it validates that each inner
// packet is a well-formed UDP datagram sourced from the UE IP to the target,
// and tallies payload bytes/packets.
type countingUplink struct {
	packets, bytes atomic.Int64
	ueIP           net.IP
	dstPort        uint16
	t              *testing.T
}

func (u *countingUplink) SendUplink(inner []byte) error {
	payload, from, ok := datapath.ExtractUDPPayload(inner, u.dstPort)
	if !ok {
		u.t.Error("uplink got a packet that is not a valid inner UDP datagram to the target port")
		return nil
	}
	if !from.IP.Equal(u.ueIP) {
		u.t.Errorf("packet sourced from %v, want UE IP %v", from.IP, u.ueIP)
	}
	u.packets.Add(1)
	u.bytes.Add(int64(len(payload)))
	return nil
}

// TestRunFlowOverTunnel drives a real loom UDP flow through the bridge over a
// loopback tunnel and checks loom's throughput accounting matches the packets
// that actually went up, each correctly sourced from the UE IP.
func TestRunFlowOverTunnel(t *testing.T) {
	up := &countingUplink{ueIP: net.ParseIP("192.168.100.5"), dstPort: 9999, t: t}
	res, err := RunFlow(context.Background(), Config{
		Uplink:     up,
		UEIP:       up.ueIP,
		Target:     "10.0.0.9:9999",
		PacketSize: 200,
		Rate:       "10Mbps",
		Duration:   300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Packets == 0 {
		t.Fatal("loom sent no packets over the tunnel")
	}
	if int64(res.Packets) != up.packets.Load() {
		t.Errorf("loom counted %d packets, the tunnel saw %d", res.Packets, up.packets.Load())
	}
	if int64(res.Bytes) != up.bytes.Load() {
		t.Errorf("loom counted %d payload bytes, the tunnel saw %d", res.Bytes, up.bytes.Load())
	}
	// 10Mbps for ~300ms ≈ 375 KB ≈ ~1875 × 200B packets — sanity, not exact.
	if res.Mbps < 5 || res.Mbps > 15 {
		t.Errorf("throughput %.1f Mbps far from the 10Mbps offered", res.Mbps)
	}
	t.Logf("loom→GTP-U: %d packets, %d payload bytes, %.1f Mbps over %v",
		res.Packets, res.Bytes, res.Mbps, res.Duration.Round(time.Millisecond))
}

func TestRunFlowValidatesConfig(t *testing.T) {
	if _, err := RunFlow(context.Background(), Config{Uplink: &countingUplink{t: t}, Target: "bad"}); err == nil {
		t.Fatal("expected error on malformed target")
	}
}
