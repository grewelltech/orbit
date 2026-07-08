package datapath

import (
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
