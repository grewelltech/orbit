package datapath

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// TestSharedTunnelUEFlow checks that per-UE flows over one shared socket
// encapsulate with their own TEID and reach the (fake UPF) destination.
func TestSharedTunnelUEFlow(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()

	st, err := NewSharedTunnel("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Two UEs sharing the one socket, each with its own TEID.
	for _, tc := range []struct {
		teid uint32
		body string
	}{{0x1234, "ue-a"}, {0x00000100, "ue-b"}} {
		if err := st.UEFlow(tc.teid, 1).SendUplink([]byte(tc.body)); err != nil {
			t.Fatalf("send %s: %v", tc.body, err)
		}
		buf := make([]byte, 2048)
		_ = upf.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := upf.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("recv %s: %v", tc.body, err)
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil {
			t.Fatalf("decode %s: %v", tc.body, err)
		}
		if g.TEID != tc.teid {
			t.Errorf("%s: TEID = %#x, want %#x", tc.body, g.TEID, tc.teid)
		}
		if string(g.Payload) != tc.body {
			t.Errorf("payload = %q, want %q", g.Payload, tc.body)
		}
	}
}

// readGPDU reads one uplink G-PDU at the fake UPF within a second.
func readGPDU(t *testing.T, upf *net.UDPConn) *gtpu.GPDU {
	t.Helper()
	buf := make([]byte, 2048)
	_ = upf.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := upf.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("UPF read: %v", err)
	}
	g, err := gtpu.DecodeGPDU(buf[:n])
	if err != nil {
		t.Fatalf("UPF decode: %v", err)
	}
	return g
}

// TestSharedTunnelPerUEIsolation is the Phase-5 core property on ONE socket:
// two UE views on one SharedTunnel — uplink stamped with each session's own
// UL TEID/QFI, downlink routed to each UE's lane by DL TEID with no
// cross-talk, and per-UE per-QFI counters kept apart (DataStats stays
// per-UE).
func TestSharedTunnelPerUEIsolation(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()

	st, err := NewSharedTunnel("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ueA, err := st.Register(UETunnelConfig{ULTEID: 0xA1, DLTEID: 0xA2, QFI: 1})
	if err != nil {
		t.Fatal(err)
	}
	ueB, err := st.Register(UETunnelConfig{ULTEID: 0xB1, DLTEID: 0xB2, QFI: 5})
	if err != nil {
		t.Fatalf("second UE on the same shared socket: %v", err)
	}
	// A third UE colliding on B's DL TEID is refused, never silently shared.
	if _, err := st.Register(UETunnelConfig{ULTEID: 0xC1, DLTEID: 0xB2, QFI: 1}); err == nil {
		t.Fatal("Register with an occupied DL TEID must refuse")
	}

	// Uplink: each view stamps its own TEID/QFI on the one socket.
	pktA := icmpPkt(t, 0x0A, 1)
	pktB := icmpPkt(t, 0x0B, 1)
	if err := ueA.SendUplink(pktA); err != nil {
		t.Fatal(err)
	}
	if g := readGPDU(t, upf); g.TEID != 0xA1 || !g.HasQFI || g.QFI != 1 {
		t.Errorf("UE A uplink stamped TEID %#x QFI %d, want 0xA1/1", g.TEID, g.QFI)
	}
	if err := ueB.SendUplink(pktB); err != nil {
		t.Fatal(err)
	}
	if g := readGPDU(t, upf); g.TEID != 0xB1 || !g.HasQFI || g.QFI != 5 {
		t.Errorf("UE B uplink stamped TEID %#x QFI %d, want 0xB1/5", g.TEID, g.QFI)
	}

	// Downlink: routed by TEID to the right lane, nothing leaks across.
	ringA := ueA.Lane().SubscribeICMP()
	ringB := ueB.Lane().SubscribeICMP()
	gnb := st.LocalAddr().(*net.UDPAddr)
	if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(0xA2, 1, icmpPkt(t, 0xDA, 2)), gnb); err != nil {
		t.Fatal(err)
	}
	if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(0xB2, 5, icmpPkt(t, 0xDB, 2)), gnb); err != nil {
		t.Fatal(err)
	}
	fa, err := ringA.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("UE A downlink: %v", err)
	}
	if _, ok := MatchICMPEchoReply(fa.Payload, 0xDA, 2); ok {
		t.Fatal("fixture sanity: echo request misread as reply")
	}
	fb, err := ringB.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("UE B downlink: %v", err)
	}
	if len(fb.Payload) < 24 {
		t.Fatal("UE B downlink truncated")
	}
	// No cross-talk: both lanes drained their one packet each.
	if _, err := ringA.Read(30 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("UE B's packet leaked into UE A's lane (err=%v)", err)
	}
	if _, err := ringB.Read(30 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("UE A's packet leaked into UE B's lane (err=%v)", err)
	}

	// Per-UE stats: A counts only A's traffic (QFI 1), B only B's (QFI 5).
	sa, sb := ueA.Stats(), ueB.Stats()
	if sa[1].UplinkPackets != 1 || sa[1].DownlinkPackets != 1 {
		t.Errorf("UE A stats = %+v, want ul=1 dl=1 on QFI 1", sa[1])
	}
	if _, leaked := sa[5]; leaked {
		t.Errorf("UE B's QFI leaked into UE A's stats: %v", sa)
	}
	if sb[5].UplinkPackets != 1 || sb[5].DownlinkPackets != 1 {
		t.Errorf("UE B stats = %+v, want ul=1 dl=1 on QFI 5", sb[5])
	}

	// Closing one view wakes its consumers and leaves the other untouched.
	ueA.Close()
	if _, err := ringA.Read(time.Second); !errors.Is(err, net.ErrClosed) {
		t.Errorf("closed UE A ring read = %v, want net.ErrClosed", err)
	}
	if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(0xB2, 5, icmpPkt(t, 0xDB, 3)), gnb); err != nil {
		t.Fatal(err)
	}
	if _, err := ringB.Read(2 * time.Second); err != nil {
		t.Errorf("UE B stopped receiving after UE A closed: %v", err)
	}
}

// TestUETunnelRebindAndDetachCarry covers the two handover moves on the
// shared path: Rebind (intra-gNB TEID swap on one socket) and Detach →
// Register on a second SharedTunnel (inter-gNB), with the live lane and the
// per-UE counters carried across so DataStats stays cumulative.
func TestUETunnelRebindAndDetachCarry(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()

	src, err := NewSharedTunnel("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ue, err := src.Register(UETunnelConfig{ULTEID: 0x11, DLTEID: 0x100, QFI: 1})
	if err != nil {
		t.Fatal(err)
	}
	ring := ue.Lane().SubscribeICMP()

	send := func(st *SharedTunnel, teid uint32) {
		t.Helper()
		if _, err := upf.WriteToUDP(gtpu.EncodeGPDU(teid, 1, icmpPkt(t, 1, 1)), st.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}

	send(src, 0x100)
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("pre-rebind delivery: %v", err)
	}

	// Intra-gNB: TEID swap on the same socket; the ring rides along.
	if err := ue.Rebind(0x200); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if ue.DLTEID() != 0x200 {
		t.Fatalf("DLTEID = %#x, want 0x200", ue.DLTEID())
	}
	send(src, 0x200)
	if _, err := ring.Read(2 * time.Second); err != nil {
		t.Fatalf("post-rebind delivery: %v", err)
	}

	// Inter-gNB: detach the live lane + counters, register on the target.
	tgt, err := NewSharedTunnel("127.0.0.1:0", upf.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tgt.Close()
	lane, stats := ue.Detach()
	if lane == nil || stats == nil {
		t.Fatal("Detach returned nothing to carry")
	}
	ue2, err := tgt.Register(UETunnelConfig{ULTEID: 0x11, DLTEID: 0x300, QFI: 1, Lane: lane, Stats: stats})
	if err != nil {
		t.Fatalf("Register carried lane on target: %v", err)
	}
	// The source view is dead; closing it must not close the moved lane.
	ue.Close()
	send(tgt, 0x300)
	f, err := ring.Read(2 * time.Second)
	if err != nil {
		t.Fatalf("post-move delivery on the SAME ring: %v", err)
	}
	if len(f.Payload) == 0 {
		t.Fatal("empty frame after move")
	}
	// Counters carried: 3 downlink packets across both sockets.
	if st := ue2.Stats()[1]; st.DownlinkPackets != 3 {
		t.Errorf("carried DL packets = %d, want 3 (stats must survive the move)", st.DownlinkPackets)
	}
}
