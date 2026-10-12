// Package mockupf is a stub UPF for tests and the conformance harness: one
// UDP socket on the N3 side that records uplink G-PDUs, sends downlink on
// request, and reacts to an Xn path switch the way a UPF configured for it
// would, by sending a GTP-U End Marker on the old downlink tunnel (TS 29.281
// §7.3.2, TS 29.244 §8.2.31) and, when asked, a late G-PDU after it. It
// implements mockamf.UPF, so the mock AMF tells it about sessions and path
// switches and it needs no NGAP of its own.
//
// It is not a UPF: no N6, no QoS enforcement, no ICMP engine. Production code
// never imports it.
package mockupf

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/bgrewell/orbit/internal/datapath"
	"github.com/bgrewell/orbit/internal/gtpu"
	"github.com/bgrewell/orbit/internal/mockamf"
)

// Options select how the stub behaves on a path switch and on downlink.
type Options struct {
	// EndMarker sends one End Marker to the old downlink tunnel after each
	// path switch; false models a UPF with End Marker disabled.
	EndMarker bool
	// LateGPDU sends one G-PDU to the old downlink tunnel after the End
	// Marker (or alone, without EndMarker), the ordering violation a
	// conformance check must catch.
	LateGPDU bool
	// QFI adds a PDU Session Container extension header carrying the
	// session's QFI to downlink G-PDUs and End Markers (TS 29.281 §5.2.2.7);
	// without it they carry the mandatory header only.
	QFI bool
	// GTPUPort is the gNB's GTP-U port; 0 means 2152.
	GTPUPort int
}

// Uplink is one G-PDU the stub received.
type Uplink struct {
	Arrival time.Time
	From    *net.UDPAddr
	TEID    uint32
	Known   bool // TEID belongs to a current session
	QFI     uint8
	HasQFI  bool
	Payload []byte
}

// Sent kinds.
const (
	KindGPDU      = "g-pdu"
	KindEndMarker = "end-marker"
)

// Sent is one packet the stub sent towards a gNB.
type Sent struct {
	At     time.Time
	Kind   string
	To     *net.UDPAddr
	TEID   uint32
	HasQFI bool
}

// UPF is the stub. Its methods are safe for concurrent use.
type UPF struct {
	conn *net.UDPConn
	opts Options

	mu       sync.Mutex
	sessions map[sessionKey]mockamf.SessionInfo
	uplink   []Uplink
	sent     []Sent
	switches []mockamf.PathSwitch
	sendErrs []error       // sends that failed inside PathSwitched, which cannot return them
	changed  chan struct{} // closed and replaced on every state change

	done chan struct{}
}

type sessionKey struct {
	supi string
	id   uint8
}

var _ mockamf.UPF = (*UPF)(nil)

// Listen opens the stub's N3 socket on addr (host:port; ":2152" style ports
// are what a gNB derives from an NGAP tunnel address) and starts reading.
func Listen(addr string, opts Options) (*UPF, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("mockupf: resolve %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, fmt.Errorf("mockupf: listen %q: %w", addr, err)
	}
	if opts.GTPUPort == 0 {
		opts.GTPUPort = 2152
	}
	u := &UPF{conn: conn, opts: opts, sessions: map[sessionKey]mockamf.SessionInfo{},
		changed: make(chan struct{}), done: make(chan struct{})}
	go u.serve()
	return u, nil
}

// Addr is the stub's N3 address.
func (u *UPF) Addr() *net.UDPAddr { return u.conn.LocalAddr().(*net.UDPAddr) }

// Close stops the stub and closes its socket.
func (u *UPF) Close() error {
	err := u.conn.Close()
	<-u.done
	return err
}

// SessionUp records a session (mockamf.UPF).
func (u *UPF) SessionUp(s mockamf.SessionInfo) {
	u.mu.Lock()
	u.sessions[sessionKey{s.SUPI, s.PDUSessionID}] = s
	u.notifyLocked()
	u.mu.Unlock()
}

// PathSwitched moves a session to its new tunnels and, per Options, sends
// the End Marker and the late G-PDU on the old downlink tunnel
// (mockamf.UPF).
func (u *UPF) PathSwitched(p mockamf.PathSwitch) {
	u.mu.Lock()
	k := sessionKey{p.SUPI, p.PDUSessionID}
	s, ok := u.sessions[k]
	if ok {
		s.UL, s.DL = p.NewUL, p.NewDL
		u.sessions[k] = s
	}
	u.switches = append(u.switches, p)
	u.notifyLocked()
	u.mu.Unlock()

	qfi := uint8(1)
	var ueIP net.IP
	if ok {
		qfi, ueIP = s.QFI, s.UEIP
	}
	if u.opts.EndMarker {
		u.recordErr(u.send(KindEndMarker, p.OldDL, encodeEndMarker(p.OldDL.TEID, qfi, u.opts.QFI)))
	}
	if u.opts.LateGPDU {
		u.recordErr(u.send(KindGPDU, p.OldDL, encodeDLGPDU(p.OldDL.TEID, qfi, u.opts.QFI, lateInner(ueIP))))
	}
}

// SendErrors returns the sends that failed inside PathSwitched, which the
// mockamf.UPF interface gives no way to return.
func (u *UPF) SendErrors() []error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]error(nil), u.sendErrs...)
}

func (u *UPF) recordErr(err error) {
	if err == nil {
		return
	}
	u.mu.Lock()
	u.sendErrs = append(u.sendErrs, err)
	u.notifyLocked()
	u.mu.Unlock()
}

// SendDownlink sends one inner IP packet to a session's current downlink
// tunnel.
func (u *UPF) SendDownlink(supi string, pduSessionID uint8, inner []byte) error {
	u.mu.Lock()
	s, ok := u.sessions[sessionKey{supi, pduSessionID}]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("mockupf: no session %d for %s", pduSessionID, supi)
	}
	return u.send(KindGPDU, s.DL, encodeDLGPDU(s.DL.TEID, s.QFI, u.opts.QFI, inner))
}

// Session returns a session's current state.
func (u *UPF) Session(supi string, pduSessionID uint8) (mockamf.SessionInfo, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.sessions[sessionKey{supi, pduSessionID}]
	return s, ok
}

// Uplink returns the G-PDUs received so far.
func (u *UPF) Uplink() []Uplink {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Uplink(nil), u.uplink...)
}

// Sent returns the packets sent so far.
func (u *UPF) Sent() []Sent {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]Sent(nil), u.sent...)
}

// Switches returns the path switches seen so far.
func (u *UPF) Switches() []mockamf.PathSwitch {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]mockamf.PathSwitch(nil), u.switches...)
}

// WaitFor blocks until cond holds (checked on every state change) or the
// timeout passes, and reports whether it held.
func (u *UPF) WaitFor(timeout time.Duration, cond func(u *UPF) bool) bool {
	deadline := time.After(timeout)
	for {
		u.mu.Lock()
		ch := u.changed
		u.mu.Unlock()
		if cond(u) {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return cond(u)
		}
	}
}

func (u *UPF) notifyLocked() {
	close(u.changed)
	u.changed = make(chan struct{})
}

func (u *UPF) send(kind string, t mockamf.Tunnel, pkt []byte) error {
	to, err := net.ResolveUDPAddr("udp", net.JoinHostPort(t.Addr, strconv.Itoa(u.opts.GTPUPort)))
	if err != nil {
		return fmt.Errorf("mockupf: resolve gNB %q: %w", t.Addr, err)
	}
	if _, err := u.conn.WriteToUDP(pkt, to); err != nil {
		return fmt.Errorf("mockupf: send %s to %s: %w", kind, to, err)
	}
	hasQFI := u.opts.QFI
	u.mu.Lock()
	u.sent = append(u.sent, Sent{At: time.Now(), Kind: kind, To: to, TEID: t.TEID, HasQFI: hasQFI})
	u.notifyLocked()
	u.mu.Unlock()
	return nil
}

func (u *UPF) serve() {
	defer close(u.done)
	buf := make([]byte, 65536)
	for {
		n, from, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g, err := gtpu.DecodeGPDU(buf[:n])
		if err != nil || g.MsgType != gtpu.MsgTypeGPDU {
			continue
		}
		u.mu.Lock()
		known := false
		for _, s := range u.sessions {
			if s.UL.TEID == g.TEID {
				known = true
				break
			}
		}
		u.uplink = append(u.uplink, Uplink{Arrival: time.Now(), From: from, TEID: g.TEID, Known: known,
			QFI: g.QFI, HasQFI: g.HasQFI, Payload: append([]byte(nil), g.Payload...)})
		u.notifyLocked()
		u.mu.Unlock()
	}
}

// pduSessDL is the PDU Session Container's first octet for DL PDU SESSION
// INFORMATION (PDU type 0, TS 38.415 §5.5.2.1).
const pduSessDL = 0x00

// encodeDLGPDU builds a downlink G-PDU, with a PDU Session Container
// carrying qfi when withQFI is set.
func encodeDLGPDU(teid uint32, qfi uint8, withQFI bool, payload []byte) []byte {
	if !withQFI {
		buf := make([]byte, 8, 8+len(payload))
		buf[0], buf[1] = 0x30, gtpu.MsgTypeGPDU
		binary.BigEndian.PutUint16(buf[2:], uint16(len(payload)))
		binary.BigEndian.PutUint32(buf[4:], teid)
		return append(buf, payload...)
	}
	buf := make([]byte, 16, 16+len(payload))
	buf[0], buf[1] = 0x34, gtpu.MsgTypeGPDU
	binary.BigEndian.PutUint16(buf[2:], uint16(8+len(payload)))
	binary.BigEndian.PutUint32(buf[4:], teid)
	buf[11] = gtpu.ExtHeaderTypePDUSessionContainer
	buf[12], buf[13], buf[14], buf[15] = 0x01, pduSessDL, qfi&0x3F, 0x00
	return append(buf, payload...)
}

// encodeEndMarker builds an End Marker for teid, with a PDU Session
// Container carrying qfi when withQFI is set.
func encodeEndMarker(teid uint32, qfi uint8, withQFI bool) []byte {
	if !withQFI {
		return gtpu.EncodeEndMarker(teid)
	}
	buf := make([]byte, 16)
	buf[0], buf[1] = 0x34, gtpu.MsgTypeEndMarker
	binary.BigEndian.PutUint16(buf[2:], 8)
	binary.BigEndian.PutUint32(buf[4:], teid)
	buf[11] = gtpu.ExtHeaderTypePDUSessionContainer
	buf[12], buf[13], buf[14], buf[15] = 0x01, pduSessDL, qfi&0x3F, 0x00
	return buf
}

// lateInner is the inner packet of a late G-PDU: an ICMP echo request from
// 8.8.8.8 to the UE (any well-formed IPv4 packet would do).
func lateInner(ueIP net.IP) []byte {
	if ueIP == nil {
		ueIP = net.IPv4(10, 60, 0, 1)
	}
	pkt, err := datapath.BuildICMPEchoRequest(net.IPv4(8, 8, 8, 8), ueIP, 0x1a7e, 1, []byte("late"))
	if err != nil {
		return []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 1, 0, 0, 8, 8, 8, 8, 10, 60, 0, 1}
	}
	return pkt
}
