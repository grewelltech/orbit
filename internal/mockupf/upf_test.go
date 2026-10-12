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
