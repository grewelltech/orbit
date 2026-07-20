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

// echoProbe turns each uplink ICMP echo request into a downlink echo reply
// pushed onto a demux-style ICMP ring (the lane the pinger now consumes),
// optionally dropping every dropEvery-th probe to model loss.
type echoProbe struct {
	ring      *datapath.Ring
	seq       int
	dropEvery int
}

func newEchoProbe(dropEvery int) *echoProbe {
	return &echoProbe{ring: datapath.NewRing(16), dropEvery: dropEvery}
}

func (e *echoProbe) SendUplink(inner []byte) error {
	e.seq++
	if e.dropEvery > 0 && e.seq%e.dropEvery == 0 {
		return nil // dropped: no reply
	}
	r := make([]byte, len(inner))
	copy(r, inner)
	var tmp [4]byte // swap IP src/dst so the reply comes "from" the target
	copy(tmp[:], r[12:16])
	copy(r[12:16], r[16:20])
	copy(r[16:20], tmp[:])
	ihl := int(r[0]&0x0F) * 4
	r[ihl] = 0 // ICMP echo reply
	e.ring.Push(r, time.Now())
	return nil
}

func TestRunLatencyOverTunnel(t *testing.T) {
	p := newEchoProbe(0)
	res, err := RunLatency(context.Background(), LatencyConfig{
		Uplink: p, RX: p.ring, UEIP: net.ParseIP("192.168.100.5"), Target: "10.0.0.9",
		Probes: 10, Spacing: time.Millisecond, Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sent != 10 || res.Received != 10 || res.Lost != 0 {
		t.Fatalf("sent/recv/lost = %d/%d/%d, want 10/10/0", res.Sent, res.Received, res.Lost)
	}
	t.Logf("latency over tunnel: min %v mean %v max %v jitter %v loss %.0f%%",
		res.Min, res.Mean, res.Max, res.Jitter, res.LossPct)
}

func TestRunLatencyReportsLoss(t *testing.T) {
	p := newEchoProbe(2)
	res, err := RunLatency(context.Background(), LatencyConfig{
		Uplink: p, RX: p.ring, UEIP: net.ParseIP("192.168.100.5"), Target: "10.0.0.9",
		Probes: 10, Spacing: time.Millisecond, Timeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lost != 5 || res.LossPct != 50 {
		t.Fatalf("lost/lossPct = %d/%.0f, want 5/50", res.Lost, res.LossPct)
	}
}
