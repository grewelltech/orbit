package loomgtp

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgrewell/loom/core/netpath"
	"github.com/bgrewell/loom/core/netpath/dgram"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
)

const (
	rigULTEID = 0x1001
	rigDLTEID = 0x2002
	rigQFI    = 9
)

var rigUEIP = net.ParseIP("192.168.100.5")

// upfRig is the loopback data-plane harness: a stand-in UPF UDP socket, a
// real Tunnel+Demux (the Phase-4 shape — demux layered on the per-session
// tunnel socket), the UE's downlink lane, and a NetworkFor bridge over it.
// The fake UPF GTP-U-decaps every uplink G-PDU, validates the inner IPv4+UDP
// headers loom's dgram network built, and reflects the datagram back down
// the tunnel with source/destination swapped — a UDP echo peer at the far
// end of N3.
type upfRig struct {
	upf *net.UDPConn
	tun *datapath.Tunnel
	rx  *datapath.UERx
	nw  netpath.Network

	mu  sync.Mutex
	gnb *net.UDPAddr // source of the last uplink G-PDU (the gNB N3 socket)
}

func newUPFRig(t *testing.T) *upfRig {
	t.Helper()
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upf.Close() })
	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: "127.0.0.1:0",
		UPFN3:   upf.LocalAddr().String(),
		ULTEID:  rigULTEID, DLTEID: rigDLTEID, QFI: rigQFI,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tun.Close() })
	d := tun.Demux()
	t.Cleanup(func() { d.Close() })
	rx := d.Register(rigDLTEID)

	nw, err := NetworkFor(tun, rx, rigUEIP, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nw.Close() })

	r := &upfRig{upf: upf, tun: tun, rx: rx, nw: nw}
	go r.serve(t)
	return r
}

// serve is the fake UPF: decap uplink, validate the inner packet, reflect it
// downlink. Exits when the UPF socket closes.
func (r *upfRig) serve(t *testing.T) {
	buf := make([]byte, 65536)
	for {
		n, from, err := r.upf.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil || g.MsgType != gtpu.MsgTypeGPDU || len(g.Payload) == 0 {
			t.Errorf("UPF got a non-G-PDU uplink frame (err %v)", err)
			continue
		}
		if g.TEID != rigULTEID {
			t.Errorf("uplink TEID = %#x, want %#x", g.TEID, rigULTEID)
			continue
		}
		r.mu.Lock()
		r.gnb = from
		r.mu.Unlock()
		inner := g.Payload
		if len(inner) >= 20 && inner[9] == 1 {
			continue // inner ICMP (the latency probe's echo) — nothing to reflect
		}
		payload, src, ok := datapath.ExtractUDPPayload(inner, 0)
		if !ok {
			t.Error("uplink inner packet is not a valid IPv4+UDP datagram")
			continue
		}
		if !src.IP.Equal(rigUEIP.To4()) {
			t.Errorf("inner packet sourced from %v, want UE IP %v", src.IP, rigUEIP)
		}
		ihl := int(inner[0]&0x0F) * 4
		if ck := binary.BigEndian.Uint16(inner[ihl+6 : ihl+8]); ck == 0 {
			t.Error("inner UDP checksum is zero; dgram should always compute it")
		}
		dstIP := net.IP(append([]byte(nil), inner[16:20]...))
		dstPort := binary.BigEndian.Uint16(inner[ihl+2 : ihl+4])
		// Reflect: dst becomes src, the echo comes "from" the dialed target.
		reply, err := datapath.BuildUDPPacket(dstIP, src.IP, dstPort, uint16(src.Port), payload)
		if err != nil {
			t.Errorf("build reflected packet: %v", err)
			continue
		}
		if _, err := r.upf.WriteToUDP(gtpu.EncodeGPDU(rigDLTEID, rigQFI, reply), from); err != nil {
			return
		}
	}
}

// TestNetworkForRoundTrip pushes a real datagram through the whole bridge —
// dgram net.PacketConn → RawL3 tx → GTP-U encap → fake UPF decap+reflect →
// GTP-U decap → Demux wildcard UDP lane → rx datapath → dgram demux — and
// checks payload, peer address, and the preserved arrival timestamp.
func TestNetworkForRoundTrip(t *testing.T) {
	r := newUPFRig(t)

	pc, err := r.nw.ListenPacket("udp", ":5004")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	target := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 7777}
	msg := []byte("orbit voip media over gtp-u")
	before := time.Now()
	if _, err := pc.WriteTo(msg, target); err != nil {
		t.Fatal(err)
	}

	mc, ok := pc.(dgram.MetaConn)
	if !ok {
		t.Fatal("dgram packet conn does not implement MetaConn")
	}
	_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, from, arrival, err := mc.ReadFromMeta(buf)
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Errorf("echoed payload = %q, want %q", buf[:n], msg)
	}
	ua, ok := from.(*net.UDPAddr)
	if !ok || !ua.IP.Equal(target.IP) || ua.Port != target.Port {
		t.Errorf("reply from %v, want %v", from, target)
	}
	if arrival.IsZero() {
		t.Fatal("arrival timestamp not preserved through the demux ring / Frame.Meta")
	}
	if arrival.Before(before) || arrival.After(after) {
		t.Errorf("arrival %v outside [%v, %v]", arrival, before, after)
	}
}

// TestNetworkForDialConn covers the connected-conn path (DialContext) and
// several sequential round trips on one conn.
func TestNetworkForDialConn(t *testing.T) {
	r := newUPFRig(t)

	conn, err := r.nw.DialContext(context.Background(), "udp", "10.0.0.9:7777")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	buf := make([]byte, 2048)
	for i := 0; i < 5; i++ {
		msg := []byte{'p', 'k', 't', byte('0' + i)}
		if _, err := conn.Write(msg); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("round trip %d: %v", i, err)
		}
		if !bytes.Equal(buf[:n], msg) {
			t.Fatalf("round trip %d: got %q, want %q", i, buf[:n], msg)
		}
	}
}

// TestNetworkForSharesDownlinkWithICMP checks the §6 invariant the demux
// exists for: the media lane and the ICMP latency lane consume the same
// tunnel socket without stealing each other's packets.
func TestNetworkForSharesDownlinkWithICMP(t *testing.T) {
	r := newUPFRig(t)

	icmp := r.rx.SubscribeICMP()
	defer r.rx.UnsubscribeICMP(icmp)

	// Media round trip while the ICMP subscription is live.
	conn, err := r.nw.DialContext(context.Background(), "udp", "10.0.0.9:7777")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("media")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("media round trip with ICMP lane subscribed: %v", err)
	}

	// An uplink ICMP echo through the same tunnel: the reflected reply must
	// land on the ICMP ring, not the media lane.
	req, err := datapath.BuildICMPEchoRequest(rigUEIP, net.IPv4(10, 0, 0, 9), 0xBEEF, 1, []byte("probe"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake UPF only reflects UDP; inject the "reply" downlink directly.
	reply, err := datapath.BuildICMPEchoRequest(net.IPv4(10, 0, 0, 9), rigUEIP, 0xBEEF, 1, []byte("probe"))
	if err != nil {
		t.Fatal(err)
	}
	ihl := int(reply[0]&0x0F) * 4
	reply[ihl] = 0 // type: echo reply
	if err := r.tun.SendUplink(req); err != nil {
		t.Fatal(err)
	}
	gnb := r.gnbAddr(t)
	if _, err := r.upf.WriteToUDP(gtpu.EncodeGPDU(rigDLTEID, rigQFI, reply), gnb); err != nil {
		t.Fatal(err)
	}
	f, err := icmp.Read(5 * time.Second)
	if err != nil {
		t.Fatalf("ICMP lane never saw the echo reply: %v", err)
	}
	if _, ok := datapath.MatchICMPEchoReply(f.Payload, 0xBEEF, 1); !ok {
		t.Error("ICMP lane frame is not the expected echo reply")
	}
}

// nullUplink discards uplink packets (caps-validation tests never send).
type nullUplink struct{}

func (nullUplink) SendUplink([]byte) error { return nil }

// TestDgramValidatesRawL3Caps pins the contract the whole bridge hangs on:
// dgram.New accepts the RawL3 pair and refuses the legacy payload datapath
// (which hands loom raw payloads, not IP packets — headers would be built
// twice or not at all).
func TestDgramValidatesRawL3Caps(t *testing.T) {
	local := netip.MustParseAddr("192.168.100.5")

	// The RawL3 pair is accepted.
	okNet, err := dgram.New(newRawTx(nullUplink{}, DefaultInnerMTU), newRx(datapath.NewRing(4), nil), local, DefaultInnerMTU)
	if err != nil {
		t.Fatalf("dgram.New rejected the RawL3 pair: %v", err)
	}
	okNet.Close()

	// The legacy payload tx (no RawL3) is refused.
	ptx := newPayloadTx(nullUplink{}, rigUEIP, net.IPv4(10, 0, 0, 9), 5001, 9999, 1400)
	if ptx.Caps().RawL3 {
		t.Fatal("payload tx must not advertise RawL3 — its frames are raw payloads")
	}
	if _, err := dgram.New(ptx, newRx(datapath.NewRing(4), nil), local, DefaultInnerMTU); err == nil {
		t.Fatal("dgram.New accepted the non-RawL3 payload datapath")
	} else if !strings.Contains(err.Error(), "RawL3") {
		t.Errorf("refusal should name RawL3, got: %v", err)
	}
}

func TestNetworkForValidatesArgs(t *testing.T) {
	rxLane := (&upfRigLane{}).uerx(t)
	if _, err := NetworkFor(nil, rxLane, rigUEIP, 0); err == nil {
		t.Error("expected error for nil uplink")
	}
	if _, err := NetworkFor(nullUplink{}, nil, rigUEIP, 0); err == nil {
		t.Error("expected error for nil UERx")
	}
	if _, err := NetworkFor(nullUplink{}, rxLane, net.ParseIP("2001:db8::1"), 0); err == nil {
		t.Error("expected error for IPv6 UE address")
	}
}

// upfRigLane builds a standalone UERx (via a throwaway tunnel demux) for
// validation tests that never move packets.
type upfRigLane struct{}

func (upfRigLane) uerx(t *testing.T) *datapath.UERx {
	t.Helper()
	upf, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upf.Close() })
	tun, err := datapath.NewTunnel(datapath.Config{
		LocalN3: "127.0.0.1:0", UPFN3: upf.LocalAddr().String(),
		ULTEID: 1, DLTEID: 2, QFI: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tun.Close() })
	d := tun.Demux()
	t.Cleanup(func() { d.Close() })
	return d.Register(2)
}

// gnbAddr waits until the fake UPF has seen at least one uplink G-PDU and
// returns its source — the gNB N3 socket address downlink injections target.
func (r *upfRig) gnbAddr(t *testing.T) *net.UDPAddr {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		a := r.gnb
		r.mu.Unlock()
		if a != nil {
			return a
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fake UPF never saw an uplink G-PDU")
	return nil
}
