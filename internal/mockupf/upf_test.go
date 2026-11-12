package mockupf

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bgrewell/orbit/internal/gtpu"
	"github.com/bgrewell/orbit/internal/mockamf"
)

// gnbSocket is a raw UDP socket standing in for a gNB's N3 on a loopback
// address; the stub sends to <addr>:<port>, so the test points GTPUPort at
// this socket's port.
func gnbSocket(t *testing.T, ip string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func read(t *testing.T, c *net.UDPConn) []byte {
	t.Helper()
	buf := make([]byte, 2048)
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("gNB read: %v", err)
	}
	return buf[:n]
}

func newStub(t *testing.T, opts Options, gnb *net.UDPConn) *UPF {
	t.Helper()
	opts.GTPUPort = gnb.LocalAddr().(*net.UDPAddr).Port
	u, err := Listen("127.0.0.1:0", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.Close() })
	return u
}

// On a path switch the stub sends, to the OLD downlink tunnel and in this
// order: an End Marker (EndMarker), then a G-PDU (LateGPDU); with QFI each
// carries a DL PDU Session Container with the session's QFI, without it the
// mandatory header only. Nothing goes to the new tunnel.
func TestPathSwitchedSendsToTheOldTunnel(t *testing.T) {
	tests := []struct {
		name  string
		opts  Options
		kinds []uint8
	}{
		{name: "end marker", opts: Options{EndMarker: true}, kinds: []uint8{gtpu.MsgTypeEndMarker}},
		{name: "end marker with QFI", opts: Options{EndMarker: true, QFI: true}, kinds: []uint8{gtpu.MsgTypeEndMarker}},
		{name: "end marker then late g-pdu", opts: Options{EndMarker: true, LateGPDU: true}, kinds: []uint8{gtpu.MsgTypeEndMarker, gtpu.MsgTypeGPDU}},
		{name: "late g-pdu alone, with QFI", opts: Options{LateGPDU: true, QFI: true}, kinds: []uint8{gtpu.MsgTypeGPDU}},
		{name: "nothing", opts: Options{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldGNB := gnbSocket(t, "127.0.0.1")
			u := newStub(t, tc.opts, oldGNB)
			u.SessionUp(mockamf.SessionInfo{SUPI: "208930100000001", PDUSessionID: 1, UEIP: net.IPv4(10, 60, 0, 1), QFI: 5,
				UL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001}, DL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x77}})
			u.PathSwitched(mockamf.PathSwitch{SUPI: "208930100000001", PDUSessionID: 1,
				OldUL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001}, NewUL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001},
				OldDL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x77}, NewDL: mockamf.Tunnel{Addr: "127.0.0.2", TEID: 0x88}})

			for i, want := range tc.kinds {
				pkt := read(t, oldGNB)
				mt, _ := gtpu.MessageType(pkt)
				if mt != want {
					t.Fatalf("packet %d type = %d, want %d", i, mt, want)
				}
				if teid := uint32(pkt[4])<<24 | uint32(pkt[5])<<16 | uint32(pkt[6])<<8 | uint32(pkt[7]); teid != 0x77 {
					t.Errorf("packet %d TEID = %#x, want the old 0x77", i, teid)
				}
				hasExt := pkt[0]&0x04 != 0
				if hasExt != tc.opts.QFI {
					t.Errorf("packet %d extension flag = %v, want %v", i, hasExt, tc.opts.QFI)
				}
				if tc.opts.QFI {
					if pkt[11] != gtpu.ExtHeaderTypePDUSessionContainer || pkt[13]>>4 != 0 || pkt[14] != 5 {
						t.Errorf("packet %d container = % x, want DL PDU session info with QFI 5", i, pkt[8:16])
					}
				}
				if want == gtpu.MsgTypeGPDU {
					g, err := gtpu.DecodeGPDU(pkt)
					if err != nil || len(g.Payload) < 20 || g.Payload[0]>>4 != 4 {
						t.Errorf("late G-PDU does not carry an IPv4 packet: %v", err)
					}
				}
			}
			if err := oldGNB.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if n, _, err := oldGNB.ReadFromUDP(make([]byte, 64)); err == nil {
				t.Errorf("an unexpected %d-byte packet reached the old gNB", n)
			}
			if s, _ := u.Session("208930100000001", 1); s.DL.TEID != 0x88 {
				t.Errorf("session DL after the switch = %+v, want the new tunnel", s.DL)
			}
			if got := len(u.Sent()); got != len(tc.kinds) {
				t.Errorf("Sent() has %d entries, want %d", got, len(tc.kinds))
			}
			if errs := u.SendErrors(); len(errs) != 0 {
				t.Errorf("send errors: %v", errs)
			}
		})
	}
}

// Uplink G-PDUs are recorded with their TEID, QFI and payload, marked known
// when the TEID belongs to a session; downlink goes to the session's current
// tunnel; an unknown session is an error.
func TestUplinkRecordedAndDownlinkSent(t *testing.T) {
	gnb := gnbSocket(t, "127.0.0.1")
	u := newStub(t, Options{}, gnb)
	u.SessionUp(mockamf.SessionInfo{SUPI: "208930100000001", PDUSessionID: 1, QFI: 1,
		UL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001}, DL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x42}})

	for _, teid := range []uint32{0x10001, 0x99} {
		if _, err := gnb.WriteToUDP(gtpu.EncodeGPDU(teid, 1, []byte{0x45, 0, 0, 4}), u.Addr()); err != nil {
			t.Fatal(err)
		}
	}
	if !u.WaitFor(2*time.Second, func(u *UPF) bool { return len(u.Uplink()) == 2 }) {
		t.Fatalf("uplink = %+v, want 2 packets", u.Uplink())
	}
	ul := u.Uplink()
	if !ul[0].Known || ul[0].TEID != 0x10001 || !ul[0].HasQFI || ul[0].QFI != 1 {
		t.Errorf("first uplink = %+v, want known on 0x10001 with QFI 1", ul[0])
	}
	if ul[1].Known {
		t.Errorf("uplink on an unknown TEID marked known: %+v", ul[1])
	}

	if err := u.SendDownlink("208930100000001", 1, []byte{0x45, 0, 0, 4}); err != nil {
		t.Fatal(err)
	}
	g, err := gtpu.DecodeGPDU(read(t, gnb))
	if err != nil || g.TEID != 0x42 || g.HasQFI {
		t.Errorf("downlink = %+v (%v), want TEID 0x42 without a QFI header", g, err)
	}
	if err := u.SendDownlink("208930100000009", 1, nil); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Errorf("downlink to an unknown session: err = %v", err)
	}
}

// An Echo Request is answered from the stub's socket to the sender with an
// Echo Response carrying the request's sequence number; with NoEcho the
// stub stays silent. Either way the request is not recorded as uplink.
func TestEchoRequest(t *testing.T) {
	tests := []struct {
		name       string
		opts       Options
		wantAnswer bool
	}{
		{name: "answered", wantAnswer: true},
		{name: "NoEcho", opts: Options{NoEcho: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gnb := gnbSocket(t, "127.0.0.1")
			u := newStub(t, tc.opts, gnb)
			if _, err := gnb.WriteToUDP(gtpu.EncodeEchoRequest(0xBEEF), u.Addr()); err != nil {
				t.Fatal(err)
			}
			// A G-PDU after the request marks the point by which the stub
			// has handled it.
			if _, err := gnb.WriteToUDP(gtpu.EncodeGPDU(0x99, 1, []byte{0x45, 0, 0, 4}), u.Addr()); err != nil {
				t.Fatal(err)
			}
			if !u.WaitFor(2*time.Second, func(u *UPF) bool { return len(u.Uplink()) == 1 }) {
				t.Fatalf("uplink = %+v, want the G-PDU only", u.Uplink())
			}
			if err := gnb.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 2048)
			n, from, err := gnb.ReadFromUDP(buf)
			if !tc.wantAnswer {
				if err == nil {
					t.Fatalf("NoEcho stub answered: % x", buf[:n])
				}
				return
			}
			if err != nil {
				t.Fatalf("no Echo Response: %v", err)
			}
			if seq, ok := gtpu.EchoResponseSeq(buf[:n]); !ok || seq != 0xBEEF {
				t.Errorf("answer % x: sequence %#x ok %v, want an Echo Response with 0xbeef", buf[:n], seq, ok)
			}
			if from.Port != u.Addr().Port {
				t.Errorf("answer came from port %d, want the stub's %d", from.Port, u.Addr().Port)
			}
		})
	}
}

// A released session is forgotten: SendDownlink refuses it and its uplink
// is no longer known. With GPDUAfterRelease the stub sends one G-PDU on the
// released downlink tunnel after that delay; without it, nothing.
func TestSessionReleased(t *testing.T) {
	tests := []struct {
		name     string
		opts     Options
		wantGPDU bool
	}{
		{name: "the tunnel goes quiet"},
		{name: "a late g-pdu", opts: Options{GPDUAfterRelease: 50 * time.Millisecond}, wantGPDU: true},
		{name: "a late g-pdu with QFI", opts: Options{GPDUAfterRelease: 50 * time.Millisecond, QFI: true}, wantGPDU: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gnb := gnbSocket(t, "127.0.0.1")
			u := newStub(t, tc.opts, gnb)
			s := mockamf.SessionInfo{SUPI: "208930100000001", PDUSessionID: 1, UEIP: net.IPv4(10, 60, 0, 1), QFI: 5,
				UL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001}, DL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x77}}
			u.SessionUp(s)
			released := time.Now()
			u.SessionReleased(s)
			if _, ok := u.Session(s.SUPI, 1); ok {
				t.Error("the released session is still known")
			}
			if err := u.SendDownlink(s.SUPI, 1, []byte{0x45}); err == nil {
				t.Error("SendDownlink to a released session succeeded")
			}
			if got := u.Releases(); len(got) != 1 || got[0].DL != s.DL {
				t.Errorf("releases = %+v, want the session", got)
			}
			if !tc.wantGPDU {
				if err := gnb.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				if n, _, err := gnb.ReadFromUDP(make([]byte, 2048)); err == nil {
					t.Errorf("%d bytes arrived on the released tunnel", n)
				}
				if len(u.Sent()) != 0 {
					t.Errorf("sent %+v, want nothing", u.Sent())
				}
				return
			}
			g, err := gtpu.DecodeGPDU(read(t, gnb))
			if err != nil {
				t.Fatal(err)
			}
			if after := time.Since(released); after < tc.opts.GPDUAfterRelease {
				t.Errorf("the G-PDU came %s after the release, before %s", after, tc.opts.GPDUAfterRelease)
			}
			if g.MsgType != gtpu.MsgTypeGPDU || g.TEID != 0x77 || g.HasQFI != tc.opts.QFI || (g.HasQFI && g.QFI != 5) {
				t.Errorf("G-PDU %+v, want TEID 0x77 with QFI header %v", g, tc.opts.QFI)
			}
			if !u.WaitFor(time.Second, func(u *UPF) bool { return len(u.Sent()) == 1 }) || u.Sent()[0].Kind != KindGPDU {
				t.Errorf("sent %+v, want one G-PDU", u.Sent())
			}
			if errs := u.SendErrors(); len(errs) != 0 {
				t.Errorf("send errors %v", errs)
			}
		})
	}
}

// A stub closed before the late G-PDU is due sends nothing and records no
// error.
func TestSessionReleasedThenClosed(t *testing.T) {
	gnb := gnbSocket(t, "127.0.0.1")
	opts := Options{GPDUAfterRelease: 100 * time.Millisecond, GTPUPort: gnb.LocalAddr().(*net.UDPAddr).Port}
	u, err := Listen("127.0.0.1:0", opts)
	if err != nil {
		t.Fatal(err)
	}
	s := mockamf.SessionInfo{SUPI: "208930100000001", PDUSessionID: 1, UEIP: net.IPv4(10, 60, 0, 1), QFI: 5,
		DL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x77}}
	u.SessionUp(s)
	u.SessionReleased(s)
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if len(u.Sent()) != 0 || len(u.SendErrors()) != 0 {
		t.Errorf("after Close: sent %+v, errors %v", u.Sent(), u.SendErrors())
	}
}

// After an AN release the stub buffers a session's downlink from N6 and
// raises one downlink data notification per buffering period; when the
// session comes up again it sends what it held to the new tunnel, in
// order, and routes later packets straight there. Packets that are not
// IPv4, or for no session's UE, are refused.
func TestDownlinkBufferedWhileIdle(t *testing.T) {
	gnb := gnbSocket(t, "127.0.0.1")
	u := newStub(t, Options{}, gnb)
	ueIP := net.IPv4(10, 60, 0, 9)
	s := mockamf.SessionInfo{SUPI: "208930100000001", PDUSessionID: 1, UEIP: ueIP, QFI: 1,
		UL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001}, DL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x21}}
	u.SessionUp(s)
	type note struct {
		supi string
		id   uint8
	}
	notes := make(chan note, 8)
	u.OnDownlinkData(func(supi string, id uint8) { notes <- note{supi, id} })
	packet := func(marker byte) []byte {
		p := make([]byte, 21)
		p[0] = 0x45
		copy(p[16:20], ueIP.To4())
		p[20] = marker
		return p
	}

	// Connected: straight down the tunnel, no notification.
	if err := u.DownlinkFromN6(packet('a')); err != nil {
		t.Fatal(err)
	}
	if g, err := gtpu.DecodeGPDU(read(t, gnb)); err != nil || g.TEID != 0x21 || g.Payload[20] != 'a' {
		t.Fatalf("downlink while connected: %+v, %v", g, err)
	}
	if n, buffering := u.Buffered(s.SUPI, 1); n != 0 || buffering {
		t.Errorf("Buffered = %d, %v before the release", n, buffering)
	}

	u.ANReleased(s)
	for _, m := range []byte{'b', 'c'} {
		if err := u.DownlinkFromN6(packet(m)); err != nil {
			t.Fatal(err)
		}
	}
	if n, buffering := u.Buffered(s.SUPI, 1); n != 2 || !buffering {
		t.Errorf("Buffered = %d, %v after two packets, want 2 buffered", n, buffering)
	}
	select {
	case got := <-notes:
		if got != (note{s.SUPI, 1}) {
			t.Errorf("notification for %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no downlink data notification")
	}
	select {
	case got := <-notes:
		t.Errorf("a second notification in one buffering period: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}

	up := s
	up.DL.TEID = 0x22
	u.SessionUp(up)
	for _, want := range []byte{'b', 'c'} {
		if g, err := gtpu.DecodeGPDU(read(t, gnb)); err != nil || g.TEID != 0x22 || g.Payload[20] != want {
			t.Fatalf("flushed packet: %+v, %v; want %c on the new tunnel", g, err, want)
		}
	}
	if _, buffering := u.Buffered(s.SUPI, 1); buffering {
		t.Error("still buffering after the session came up")
	}
	if err := u.DownlinkFromN6(packet('d')); err != nil {
		t.Fatal(err)
	}
	if g, err := gtpu.DecodeGPDU(read(t, gnb)); err != nil || g.TEID != 0x22 || g.Payload[20] != 'd' {
		t.Fatalf("downlink after reactivation: %+v, %v", g, err)
	}

	for name, pkt := range map[string][]byte{
		"short":              {0x45, 0},
		"IPv6":               append([]byte{0x60}, make([]byte, 40)...),
		"no such UE address": append([]byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 1, 0, 0, 8, 8, 8, 8}, 10, 60, 0, 200),
	} {
		if err := u.DownlinkFromN6(pkt); err == nil {
			t.Errorf("%s packet accepted", name)
		}
	}
	if errs := u.SendErrors(); len(errs) != 0 {
		t.Errorf("send errors: %v", errs)
	}
}

// udpDatagram is an IPv4 UDP datagram from src:sport to dst:dport.
func udpDatagram(src, dst net.IP, sport, dport uint16, payload []byte) []byte {
	pkt := make([]byte, 28+len(payload))
	pkt[0], pkt[8], pkt[9] = 0x45, 64, 17
	pkt[2], pkt[3] = byte(len(pkt)>>8), byte(len(pkt))
	copy(pkt[12:16], src.To4())
	copy(pkt[16:20], dst.To4())
	pkt[20], pkt[21] = byte(sport>>8), byte(sport)
	pkt[22], pkt[23] = byte(dport>>8), byte(dport)
	pkt[24], pkt[25] = byte((8+len(payload))>>8), byte(8+len(payload))
	copy(pkt[28:], payload)
	return pkt
}

// With Echo the stub answers an uplink UDP datagram on a session's uplink
// TEID with the same datagram, addresses and ports swapped, on the session's
// downlink tunnel (with the QFI container when QFI is set). Uplink on an
// unknown TEID, and uplink that is not UDP, is recorded and not answered;
// without Echo nothing is.
func TestEcho(t *testing.T) {
	ue, peer := net.IPv4(10, 60, 0, 1), net.IPv4(192, 0, 2, 7)
	tests := []struct {
		name     string
		opts     Options
		teid     uint32
		inner    []byte
		wantEcho bool
	}{
		{name: "udp on the session's TEID", opts: Options{Echo: true}, teid: 0x10001, inner: udpDatagram(ue, peer, 4000, 7, []byte("hello")), wantEcho: true},
		{name: "with the QFI container", opts: Options{Echo: true, QFI: true}, teid: 0x10001, inner: udpDatagram(ue, peer, 4000, 7, []byte("hello")), wantEcho: true},
		{name: "unknown TEID", opts: Options{Echo: true}, teid: 0x10002, inner: udpDatagram(ue, peer, 4000, 7, []byte("hello"))},
		{name: "not UDP", opts: Options{Echo: true}, teid: 0x10001, inner: lateInner(ue)},
		{name: "echo off", opts: Options{}, teid: 0x10001, inner: udpDatagram(ue, peer, 4000, 7, []byte("hello"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gnb := gnbSocket(t, "127.0.0.1")
			u := newStub(t, tc.opts, gnb)
			u.SessionUp(mockamf.SessionInfo{SUPI: "208930100000001", PDUSessionID: 1, UEIP: ue, QFI: 5,
				UL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x10001}, DL: mockamf.Tunnel{Addr: "127.0.0.1", TEID: 0x77}})
			if _, err := gnb.WriteToUDP(gtpu.EncodeGPDU(tc.teid, 5, tc.inner), u.Addr()); err != nil {
				t.Fatal(err)
			}
			if !u.WaitFor(2*time.Second, func(u *UPF) bool { return len(u.Uplink()) == 1 }) {
				t.Fatal("the stub never recorded the uplink")
			}
			if !tc.wantEcho {
				time.Sleep(100 * time.Millisecond)
				if sent := u.Sent(); len(sent) != 0 {
					t.Fatalf("stub sent %+v, want nothing", sent)
				}
				return
			}
			g, err := gtpu.DecodeGPDU(read(t, gnb))
			if err != nil {
				t.Fatal(err)
			}
			if g.TEID != 0x77 || g.HasQFI != tc.opts.QFI || (g.HasQFI && g.QFI != 5) {
				t.Errorf("echo on TEID %#x, QFI container %v (QFI %d); want TEID 0x77, container %v", g.TEID, g.HasQFI, g.QFI, tc.opts.QFI)
			}
			if want := udpDatagram(peer, ue, 7, 4000, []byte("hello")); string(g.Payload) != string(want) {
				t.Errorf("echo payload % x, want % x", g.Payload, want)
			}
			if errs := u.SendErrors(); len(errs) != 0 {
				t.Errorf("send errors: %v", errs)
			}
		})
	}
}

// echoUDP refuses what is not an IPv4 UDP datagram with a whole header.
func TestEchoUDPRefuses(t *testing.T) {
	good := udpDatagram(net.IPv4(10, 0, 0, 1), net.IPv4(10, 0, 0, 2), 1, 2, nil)
	v6 := append([]byte(nil), good...)
	v6[0] = 0x65
	shortIHL := append([]byte(nil), good...)
	shortIHL[0] = 0x44
	for name, pkt := range map[string][]byte{"empty": nil, "truncated header": good[:19], "IPv6": v6, "IHL below 5": shortIHL, "no room for the UDP header": good[:27]} {
		if _, ok := echoUDP(pkt); ok {
			t.Errorf("%s: echoed", name)
		}
	}
	if _, ok := echoUDP(good); !ok {
		t.Error("a whole datagram was refused")
	}
}
