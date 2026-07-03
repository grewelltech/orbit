package datapath

import (
	"net"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
)

// TestTunnelLoopback exercises encap → send → (fake UPF echoes) → receive →
// decap end to end over loopback, without a core. A stand-in UPF listens on
// a UDP socket, decapsulates the uplink, swaps the ICMP echo request into a
// reply, and returns it on the downlink TEID.
func TestTunnelLoopback(t *testing.T) {
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer upf.Close()

	const ulTEID, dlTEID = 0x111, 0x222
	tun, err := NewTunnel(Config{
		LocalN3: "127.0.0.1:0",
		UPFN3:   upf.LocalAddr().String(),
		ULTEID:  ulTEID, DLTEID: dlTEID, QFI: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	ueIP := net.IPv4(192, 168, 100, 83)
	dst := net.IPv4(8, 8, 8, 8)
	req, err := BuildICMPEchoRequest(ueIP, dst, 0xABCD, 1, []byte("orbit"))
	if err != nil {
		t.Fatal(err)
	}

	// Stand-in UPF: receive uplink, verify QFI/TEID, echo back on the gNB DL
	// tunnel with source/dest swapped and ICMP type set to reply.
	go func() {
		buf := make([]byte, 65536)
		n, _, err := upf.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil || g.TEID != ulTEID || !g.HasQFI || g.QFI != 1 {
			return
		}
		reply := makeReply(g.Payload)
		gpdu := gtpu.EncodeGPDU(dlTEID, 1, reply)
		upf.WriteToUDP(gpdu, tun.conn.LocalAddr().(*net.UDPAddr))
	}()

	if err := tun.SendUplink(req); err != nil {
		t.Fatal(err)
	}
	inner, err := tun.ReadDownlink(2 * time.Second)
	if err != nil {
		t.Fatalf("no downlink: %v", err)
	}
	rep, ok := MatchICMPEchoReply(inner, 0xABCD, 1)
	if !ok {
		t.Fatal("downlink is not the matching echo reply")
	}
	if !rep.From.Equal(dst) {
		t.Errorf("reply from %v, want %v", rep.From, dst)
	}

	st := tun.Stats()[1]
	if st.UplinkPackets != 1 || st.DownlinkPackets != 1 {
		t.Errorf("counters: ul=%d dl=%d, want 1/1", st.UplinkPackets, st.DownlinkPackets)
	}
}

// makeReply turns an IPv4 ICMP echo request into a reply: swap src/dst, set
// ICMP type 0, and leave the identifier/seq intact.
func makeReply(req []byte) []byte {
	out := make([]byte, len(req))
	copy(out, req)
	// swap src/dst
	var tmp [4]byte
	copy(tmp[:], out[12:16])
	copy(out[12:16], out[16:20])
	copy(out[16:20], tmp[:])
	ihl := int(out[0]&0x0F) * 4
	out[ihl] = 0 // echo reply
	// zero + recompute ICMP checksum
	out[ihl+2], out[ihl+3] = 0, 0
	c := checksum(out[ihl:])
	out[ihl+2] = byte(c >> 8)
	out[ihl+3] = byte(c)
	return out
}

func TestChecksum(t *testing.T) {
	// A packet whose checksum field is correct sums (with the field) to
	// 0xFFFF, i.e. verifying re-checksum over the whole buffer yields 0.
	req, err := BuildICMPEchoRequest(net.IPv4(1, 2, 3, 4), net.IPv4(5, 6, 7, 8), 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := checksum(req[:20]); got != 0 {
		t.Errorf("IPv4 header checksum verification = %#x, want 0", got)
	}
}
